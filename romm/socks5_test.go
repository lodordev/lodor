package romm

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"lodor/config"
)

// readConnectTarget reads a full SOCKS5 no-auth greeting + CONNECT request from c,
// replies success, and returns the requested (atyp, host, port). It performs the
// server half of the handshake exactly per RFC 1928 so the client-side code is
// exercised on the wire.
func readSocks5Connect(t *testing.T, c net.Conn) (atyp byte, host string, port int) {
	t.Helper()
	greet := make([]byte, 3)
	if _, err := io.ReadFull(c, greet); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if greet[0] != 0x05 || greet[1] != 0x01 || greet[2] != 0x00 {
		t.Fatalf("bad greeting %v", greet)
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		t.Fatalf("write method reply: %v", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("read connect head: %v", err)
	}
	if head[0] != 0x05 || head[1] != 0x01 || head[2] != 0x00 {
		t.Fatalf("bad connect head %v", head)
	}
	atyp = head[3]
	switch atyp {
	case 0x01:
		b := make([]byte, 4)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	case 0x04:
		b := make([]byte, 16)
		io.ReadFull(c, b)
		host = net.IP(b).String()
	case 0x03:
		l := make([]byte, 1)
		io.ReadFull(c, l)
		b := make([]byte, int(l[0]))
		io.ReadFull(c, b)
		host = string(b)
	default:
		t.Fatalf("unexpected atyp 0x%02x", atyp)
	}
	pb := make([]byte, 2)
	io.ReadFull(c, pb)
	port = int(pb[0])<<8 | int(pb[1])
	return atyp, host, port
}

func TestSocks5Handshake_DomainConnect(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	var gotAtyp byte
	var gotHost string
	var gotPort int
	done := make(chan struct{})
	go func() {
		gotAtyp, gotHost, gotPort = readSocks5Connect(t, srv)
		// success reply: VER REP RSV ATYP(ipv4) BND.ADDR(4) BND.PORT(2)
		srv.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		close(done)
	}()

	if err := socks5Handshake(cli, "box.romm.tailnet.ts.net", 443); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	<-done
	if gotAtyp != 0x03 {
		t.Fatalf("atyp: got 0x%02x want 0x03 (domain / remote DNS)", gotAtyp)
	}
	if gotHost != "box.romm.tailnet.ts.net" {
		t.Fatalf("host: got %q (the proxy must receive the NAME, not a resolved IP)", gotHost)
	}
	if gotPort != 443 {
		t.Fatalf("port: got %d want 443", gotPort)
	}
}

func TestSocks5Handshake_IPv4LiteralUsesAddrAtyp(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	var gotAtyp byte
	var gotHost string
	done := make(chan struct{})
	go func() {
		gotAtyp, gotHost, _ = readSocks5Connect(t, srv)
		srv.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
		close(done)
	}()

	// A literal tailnet IP (the documented MagicDNS fallback) must go out as an
	// IPv4 atyp, not a domain.
	if err := socks5Handshake(cli, "100.64.0.5", 8443); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	<-done
	if gotAtyp != 0x01 {
		t.Fatalf("atyp: got 0x%02x want 0x01 (ipv4 literal)", gotAtyp)
	}
	if gotHost != "100.64.0.5" {
		t.Fatalf("host: got %q want 100.64.0.5", gotHost)
	}
}

func TestSocks5Handshake_Rejected(t *testing.T) {
	cli, srv := net.Pipe()
	defer cli.Close()
	defer srv.Close()

	go func() {
		readSocks5Connect(t, srv)
		// REP 0x05 = connection refused
		srv.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	}()

	if err := socks5Handshake(cli, "nope.ts.net", 443); err == nil {
		t.Fatal("expected error on REP!=0 reject, got nil")
	}
}

// forwardingSocks5Proxy is a minimal SOCKS5 proxy that performs the no-auth +
// CONNECT handshake, records the requested target NAME (proving remote DNS), and
// then pipes the tunneled bytes to the fixed backend address — simulating a
// tailscaled that resolves a MagicDNS name internally. It returns its own listen
// address and a pointer to the recorded last target host.
func startForwardingSocks5Proxy(t *testing.T, backend string) (addr string, lastTarget *string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	target := new(string)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, host, _ := readSocks5Connect(t, c)
				mu.Lock()
				*target = host
				mu.Unlock()
				up, err := net.Dial("tcp", backend)
				if err != nil {
					c.Write([]byte{0x05, 0x01, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); io.Copy(up, c) }()
				go func() { defer wg.Done(); io.Copy(c, up) }()
				wg.Wait()
			}(c)
		}
	}()
	return ln.Addr().String(), target, func() { ln.Close() }
}

// TestProbeReachableHost_RoutesThroughSocks5RemoteDNS is the end-to-end proof that
// a tier-1 host with a socks5_proxy reaches a backend it CANNOT resolve locally,
// because the proxy resolves the name. The RootURI uses a bogus .ts.net name that
// has no DNS record; the probe still succeeds because every connection is dialed
// through the forwarding proxy, and the proxy records that it received the NAME.
func TestProbeReachableHost_RoutesThroughSocks5RemoteDNS(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	backendAddr := backend.Listener.Addr().String()

	proxyAddr, lastTarget, stop := startForwardingSocks5Proxy(t, backendAddr)
	defer stop()

	host := config.Host{
		RootURI:     "http://box.romm.tailnet.ts.net", // unresolvable name on purpose
		Socks5Proxy: proxyAddr,
	}
	if !ProbeReachableHost(host, 3*time.Second) {
		t.Fatal("ProbeReachableHost via socks5 proxy: got false, want true (proxy should reach backend)")
	}
	if *lastTarget != "box.romm.tailnet.ts.net" {
		t.Fatalf("remote DNS: proxy received target %q, want the .ts.net NAME (socks5h)", *lastTarget)
	}
}

// TestNewClient_NoSocks5IsDirect guards the fallback: a host WITHOUT a socks5_proxy
// (and not insecure) gets the stdlib default transport (hostTransport returns nil),
// so the tier-2 / legacy path is byte-identical — no SOCKS5 dialer is installed.
func TestNewClient_NoSocks5IsDirect(t *testing.T) {
	if tr := hostTransport(config.Host{RootURI: "https://public.example"}); tr != nil {
		t.Fatalf("plain public host must get nil transport (stdlib default), got %#v", tr)
	}
	if tr := hostTransport(config.Host{RootURI: "https://x", Socks5Proxy: "localhost:1055"}); tr == nil || tr.DialContext == nil {
		t.Fatal("socks5 host must get a transport with a DialContext")
	}
}
