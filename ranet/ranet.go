// Package ranet is a from-scratch, CGO-free client for RetroArch's Network
// Control Interface: single-datagram UDP commands ("QUIT", "SAVE_STATE",
// "GET_STATUS", "SCREENSHOT", …) sent to the port RetroArch listens on when
// network_cmd_enable = "true" (default 55355, loopback).
//
// DESIGN PROVENANCE: the command set and the send / send-with-reply split
// (fire-and-forget default; 250ms recv timeout with bounded retries for the
// commands that answer, like GET_STATUS) follow Allium's RetroArch client
// (allium common/src/retroarch.rs, MIT — see engine/CREDITS.md). The code is
// original, stdlib net only.
//
// The engine uses this to BRACKET standalone/vendor RetroArch sessions the
// heavy-pak wrappers drive (task #145): probe GET_STATUS to learn whether this
// build actually implements the interface (many vendor forks compile it out —
// silence is a supported answer, the caller degrades), then QUIT so RetroArch
// flushes SRAM to disk before the save push. Nothing here is ever load-bearing
// for a launch: every failure is returned, never fatal.
package ranet

import (
	"fmt"
	"net"
	"strings"
	"time"
)

// DefaultPort is RetroArch's stock network_cmd_port.
const DefaultPort = 55355

const (
	// recvTimeout bounds ONE reply wait. RetroArch answers GET_STATUS from the
	// same socket within milliseconds when the interface is compiled in; 250ms
	// (Allium's constant) is generous on loopback.
	recvTimeout = 250 * time.Millisecond
	// recvRetries is how many send+wait attempts SendRecv makes before deciding
	// the interface is absent/silent. 3 × 250ms keeps the worst case under a
	// second — an exit path must never stall the return to the menu.
	recvRetries = 3
)

// Addr builds the loopback target for a RetroArch command port. port <= 0
// selects DefaultPort.
func Addr(port int) string {
	if port <= 0 {
		port = DefaultPort
	}
	return fmt.Sprintf("127.0.0.1:%d", port)
}

// Send fires one command datagram and returns without waiting for any reply —
// the right shape for commands that answer with actions, not bytes (QUIT,
// SAVE_STATE, SCREENSHOT, PAUSE_TOGGLE). An error means the datagram could not
// even be handed to the kernel (bad address); a running-but-deaf RetroArch is
// indistinguishable from a listening one here, by design.
func Send(addr, cmd string) error {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return fmt.Errorf("ra-net dial: %w", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(cmd)); err != nil {
		return fmt.Errorf("ra-net send: %w", err)
	}
	return nil
}

// SendRecv sends cmd and waits for a reply, retrying the whole send+wait up to
// recvRetries times (UDP may drop either direction; RetroArch replies to the
// sending socket). Returns the reply with trailing whitespace trimmed. A silent
// peer returns an error after ~recvRetries×recvTimeout — the probe signal the
// caller uses to classify a vendor build as ra-net UNSUPPORTED.
func SendRecv(addr, cmd string) (string, error) {
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return "", fmt.Errorf("ra-net dial: %w", err)
	}
	defer conn.Close()

	buf := make([]byte, 4096)
	var lastErr error
	for attempt := 0; attempt < recvRetries; attempt++ {
		if _, err := conn.Write([]byte(cmd)); err != nil {
			lastErr = fmt.Errorf("ra-net send: %w", err)
			continue
		}
		if err := conn.SetReadDeadline(time.Now().Add(recvTimeout)); err != nil {
			lastErr = fmt.Errorf("ra-net deadline: %w", err)
			continue
		}
		n, err := conn.Read(buf)
		if err == nil && n > 0 {
			return strings.TrimRight(string(buf[:n]), "\r\n \t\x00"), nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no reply")
	}
	return "", fmt.Errorf("ra-net recv (%d attempts): %w", recvRetries, lastErr)
}
