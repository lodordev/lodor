package ranet

// Unit tests against a loopback fake-RA UDP server (task #145): the echo case
// (a reply comes back), the GET_STATUS case (a realistic status line), and the
// SILENT case — the one that matters most, because a silent peer is how a
// vendor RetroArch without the network interface presents, and SendRecv's
// bounded timeout+retry is the wrapper's UNSUPPORTED-classification signal.

import (
	"net"
	"strings"
	"testing"
	"time"
)

// fakeRA starts a loopback UDP listener. respond decides what (if anything) to
// send back for each datagram; a nil respond never answers (the silent vendor
// build). Returns the address to dial and a stop func.
func fakeRA(t *testing.T, respond func(cmd string) (string, bool)) (addr string, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			select {
			case <-done:
				return
			default:
			}
			_ = pc.(*net.UDPConn).SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, from, err := pc.ReadFrom(buf)
			if err != nil || n == 0 {
				continue
			}
			if respond == nil {
				continue // deaf: read and never answer
			}
			if reply, ok := respond(string(buf[:n])); ok {
				_, _ = pc.WriteTo([]byte(reply), from)
			}
		}
	}()
	return pc.LocalAddr().String(), func() { close(done); _ = pc.Close() }
}

func TestSendRecvEcho(t *testing.T) {
	addr, stop := fakeRA(t, func(cmd string) (string, bool) { return "echo:" + cmd, true })
	defer stop()

	got, err := SendRecv(addr, "VERSION")
	if err != nil {
		t.Fatalf("SendRecv: %v", err)
	}
	if got != "echo:VERSION" {
		t.Errorf("reply = %q, want %q", got, "echo:VERSION")
	}
}

func TestSendRecvStatus(t *testing.T) {
	// Real RetroArch replies to GET_STATUS with "GET_STATUS PLAYING <core>,<game>,…\n".
	addr, stop := fakeRA(t, func(cmd string) (string, bool) {
		if strings.TrimSpace(cmd) != "GET_STATUS" {
			return "", false
		}
		return "GET_STATUS PLAYING mupen64plus,GoldenEye 007,crc32=abcd1234\n", true
	})
	defer stop()

	got, err := SendRecv(addr, "GET_STATUS")
	if err != nil {
		t.Fatalf("SendRecv: %v", err)
	}
	if !strings.HasPrefix(got, "GET_STATUS PLAYING") {
		t.Errorf("reply = %q, want GET_STATUS PLAYING prefix", got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("reply %q not trimmed", got)
	}
}

// TestSendRecvSilent locks the degrade signal: a listening-but-deaf peer (the
// vendor-fork shape) must return an error, and must do so within the bounded
// retry budget — an exit path can never stall the menu return.
func TestSendRecvSilent(t *testing.T) {
	addr, stop := fakeRA(t, nil)
	defer stop()

	start := time.Now()
	_, err := SendRecv(addr, "GET_STATUS")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("SendRecv against a silent peer returned nil error")
	}
	// 3 × 250ms + slack: anything over 2s means the bound is broken.
	if elapsed > 2*time.Second {
		t.Errorf("silent probe took %v, want < 2s (bounded exit path)", elapsed)
	}
}

// TestSendFireAndForget: Send succeeds against a silent peer (no reply is ever
// expected) and against nobody at all on a closed port (UDP has no handshake;
// only a dial-level failure errors).
func TestSendFireAndForget(t *testing.T) {
	addr, stop := fakeRA(t, nil)
	defer stop()
	if err := Send(addr, "QUIT"); err != nil {
		t.Errorf("Send to silent peer: %v", err)
	}
	if err := Send("not-an-address", "QUIT"); err == nil {
		t.Error("Send to garbage address returned nil error")
	}
}

func TestAddr(t *testing.T) {
	if got := Addr(0); got != "127.0.0.1:55355" {
		t.Errorf("Addr(0) = %q", got)
	}
	if got := Addr(4321); got != "127.0.0.1:4321" {
		t.Errorf("Addr(4321) = %q", got)
	}
}
