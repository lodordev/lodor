// SOCKS5 CONNECT client with domain-name (remote-DNS, "socks5h") addressing —
// the tier-1 (Tailscale) transport for two-tier RomM access (#84).
//
// The userspace `tailscaled --tun=userspace-networking --socks5-server=...` started
// by the pak exposes a SOCKS5 proxy that BOTH resolves a *.ts.net MagicDNS name
// internally AND routes the connection over the tailnet. So the engine reaches the
// tier-1 RomM host by dialing the proxy and asking it (by NAME) to CONNECT — no
// system resolver, no /dev/net/tun, no resolvconf. We hand-roll the ~100 lines of
// RFC 1928 here rather than pull golang.org/x/net/proxy, to keep the engine
// pure-stdlib and CGO-free (no new module deps / go.sum / vendor).
//
// Only the no-auth method (0x00) is used: the proxy is bound to localhost and is
// the device's own tailscaled, so there is nothing to authenticate to.
//
// Only the stdlib is used.
package romm

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// socks5 protocol constants (RFC 1928).
const (
	socks5Version  = 0x05
	socks5NoAuth   = 0x00
	socks5Connect  = 0x01
	socks5Reserved = 0x00

	socks5AtypIPv4   = 0x01
	socks5AtypDomain = 0x03
	socks5AtypIPv6   = 0x04
)

// socks5DialContext returns a DialContext-compatible function that routes every
// connection through the SOCKS5 proxy at proxyAddr using REMOTE DNS: the
// destination given to it (network, addr="host:port") is forwarded to the proxy by
// NAME via a CONNECT request, and the PROXY resolves it. When host is already a
// literal IP it is sent as an IPv4/IPv6 address atyp instead (so the documented
// tailnet-IP fallback works without DNS at all); otherwise it is sent as a domain
// atyp (the *.ts.net MagicDNS path). The returned func is safe to install as
// http.Transport.DialContext.
//
// Network must be a TCP variant (the only thing the HTTP transport dials); a
// non-TCP network is rejected so a stray UDP dial can never silently bypass the
// proxy. The context's deadline (if any) bounds both the proxy connect and the
// handshake.
func socks5DialContext(proxyAddr string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			return nil, fmt.Errorf("socks5: unsupported network %q (tier-1 proxy is TCP-only)", network)
		}

		host, portStr, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("socks5: bad target address %q: %w", addr, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil || port < 0 || port > 0xFFFF {
			return nil, fmt.Errorf("socks5: bad target port %q", portStr)
		}

		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("socks5: dial proxy %s: %w", proxyAddr, err)
		}
		// Apply the context deadline to the whole negotiation so a wedged proxy can
		// never hang the sync; clear it before handing the live conn back so the
		// HTTP transport sets its own per-request deadlines.
		if dl, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(dl)
		}

		if err := socks5Handshake(conn, host, port); err != nil {
			conn.Close()
			return nil, err
		}

		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
}

// socks5Handshake performs the no-auth method negotiation followed by a CONNECT
// request for host:port over an already-open proxy connection. host is sent as a
// domain name (remote DNS) unless it is a literal IP, in which case the matching
// IPv4/IPv6 address atyp is used. Returns nil only when the proxy replies CONNECT
// success (REP 0x00).
func socks5Handshake(conn net.Conn, host string, port int) error {
	// --- method negotiation: offer only "no auth" ---
	if _, err := conn.Write([]byte{socks5Version, 0x01, socks5NoAuth}); err != nil {
		return fmt.Errorf("socks5: write greeting: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("socks5: read method reply: %w", err)
	}
	if reply[0] != socks5Version {
		return fmt.Errorf("socks5: bad version 0x%02x in method reply", reply[0])
	}
	if reply[1] != socks5NoAuth {
		return fmt.Errorf("socks5: proxy demands auth method 0x%02x (only no-auth offered)", reply[1])
	}

	// --- CONNECT request ---
	req := []byte{socks5Version, socks5Connect, socks5Reserved}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			req = append(req, socks5AtypIPv4)
			req = append(req, v4...)
		} else {
			req = append(req, socks5AtypIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return fmt.Errorf("socks5: hostname too long (%d > 255)", len(host))
		}
		req = append(req, socks5AtypDomain, byte(len(host)))
		req = append(req, host...)
	}
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], uint16(port))
	req = append(req, p[0], p[1])
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("socks5: write connect: %w", err)
	}

	// --- CONNECT reply: VER REP RSV ATYP BND.ADDR BND.PORT ---
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("socks5: read connect reply: %w", err)
	}
	if head[0] != socks5Version {
		return fmt.Errorf("socks5: bad version 0x%02x in connect reply", head[0])
	}
	if head[1] != 0x00 {
		return fmt.Errorf("socks5: connect rejected (REP 0x%02x: %s)", head[1], socks5RepText(head[1]))
	}
	// Drain the bound address+port so the stream is positioned at the start of the
	// tunneled payload. Length depends on the returned atyp.
	var addrLen int
	switch head[3] {
	case socks5AtypIPv4:
		addrLen = 4
	case socks5AtypIPv6:
		addrLen = 16
	case socks5AtypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return fmt.Errorf("socks5: read bound domain len: %w", err)
		}
		addrLen = int(l[0])
	default:
		return fmt.Errorf("socks5: unknown bound atyp 0x%02x", head[3])
	}
	if _, err := io.ReadFull(conn, make([]byte, addrLen+2)); err != nil { // +2 = bound port
		return fmt.Errorf("socks5: read bound addr/port: %w", err)
	}
	return nil
}

// socks5RepText maps the RFC 1928 reply codes to a short reason for diagnostics.
func socks5RepText(rep byte) string {
	switch rep {
	case 0x01:
		return "general failure"
	case 0x02:
		return "connection not allowed"
	case 0x03:
		return "network unreachable"
	case 0x04:
		return "host unreachable"
	case 0x05:
		return "connection refused"
	case 0x06:
		return "TTL expired"
	case 0x07:
		return "command not supported"
	case 0x08:
		return "address type not supported"
	default:
		return "unknown"
	}
}
