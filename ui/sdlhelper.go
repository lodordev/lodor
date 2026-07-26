package ui

// sdlhelper.go — the SDL-lane DISPLAY backend (launch-card-v2).
//
// WHY THIS EXISTS: on the DE2/PowerVR SDL-only panels (TrimUI tg5040/tg5050, Miyoo Flip
// my355) raw /dev/fb0 is a dead scanout surface — the wizard drew a correct card and the
// screen stayed black (Decisions/2026-07-12-nextui-card-sdl-helper.md). The fix keeps the
// wizard CGO-free by shelling out to a tiny prebuilt SDL2 helper (lodor-fbhelper, built
// from integrations/nextui/fbhelper) that owns the SDL surface. Go composes every pixel
// into a Canvas exactly as before, encodes to RGB565, and pipes frames to the helper; the
// helper flips them to the panel.
//
// DISPLAY ONLY (2026-07-12 fix): this helper handles NO input. The Phase A spike proved SDL
// joystick/keyboard input is dead on the TrimUI (surface up, zero button events), while the
// wizard's Go EvdevSource reads /dev/input on that exact device fine. So the lane split is:
//   DISPLAY = SDLHelper.Present (SDL lane) vs Framebuffer.Flush (Miyoo/H700 raw-fb lane)
//   INPUT   = ui.EvdevSource on BOTH lanes (the proven path)
// SDLHelper therefore exposes ONLY Present/Xres/Yres/Close — it is NOT an InputSource and
// deliberately opens/grabs no input device (an SDL grab would starve the Go evdev reader).
//
// SEAM: Present(*Canvas) mirrors what the raw-fb path does via presentCanvas(fb, c), and
// Xres()/Yres() mirror Framebuffer so the caller sizes the Canvas to the panel. The launch
// card selects (SDLHelper display + EvdevSource input) on the SDL lane and
// (Framebuffer display + EvdevSource input) on the raw-fb lane, reusing one runCardLoop.
//
// PROTOCOL (must match lodor-fbhelper.c):
//   helper stdin : 16-byte header once ("LFB1" + u32LE w + u32LE h + u32LE bpp=2), then
//                  raw W*H*2 RGB565 frames.
//   helper fd 3  : OPTIONAL diagnostics only — a single "#surface ...\n" line at startup
//                  (tee'd to rawLog for the spike log). NO button events (the helper reads
//                  no input); any "#..." line is recorded and otherwise ignored.
// This file is CGO-free and stdlib-only (os/exec + pipes), preserving the engine invariant.

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// fbMagic is the 4-byte header magic shared with lodor-fbhelper.c.
var fbMagic = [4]byte{'L', 'F', 'B', '1'}

// EncodeRGB565 packs a Canvas into little-endian RGB565 (2 bytes/pixel, R in the high 5
// bits) into dst, which must be len >= 2*W*H. Pure + unit-testable; this is the byte format
// lodor-fbhelper reads as SDL_PIXELFORMAT_RGB565.
func EncodeRGB565(c *Canvas, dst []byte) {
	n := c.W * c.H
	if len(dst) < n*2 {
		return
	}
	for i := 0; i < n; i++ {
		r, g, b := c.Pix[i].rgb()
		v := uint16((r&0xF8)<<8 | (g&0xFC)<<3 | (b >> 3))
		dst[i*2] = byte(v)
		dst[i*2+1] = byte(v >> 8)
	}
}

// buildHeader returns the 16-byte protocol header for a W×H RGB565 stream.
func buildHeader(w, h int) []byte {
	hdr := make([]byte, 16)
	copy(hdr[0:4], fbMagic[:])
	binary.LittleEndian.PutUint32(hdr[4:], uint32(w))
	binary.LittleEndian.PutUint32(hdr[8:], uint32(h))
	binary.LittleEndian.PutUint32(hdr[12:], 2) // RGB565
	return hdr
}

// SDLHelper drives lodor-fbhelper: it owns the child process and the frame stdin pipe, and
// presents Canvases to the panel. DISPLAY ONLY — it yields no buttons (input is the Go
// EvdevSource). NOT safe for concurrent Present calls (the card loop is single-threaded).
type SDLHelper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	w, h   int
	buf    []byte    // reused RGB565 frame buffer
	rawLog io.Writer // if non-nil, the helper's fd-3 diag lines (#surface, #btn) are copied here
	once   sync.Once
	hdrErr error
	// INPUT (miyoomini SDL lane): MinUI's SDL grabs the keypad, so the helper forwards key
	// presses on fd 3 as "#btn code=<scancode>". When km is non-nil, readDiag decodes those to
	// logical Buttons on btnCh, and the helper acts as the card's InputSource (Buttons/Close).
	// km == nil => display-only (e.g. the spike, which keeps its own evdev input).
	km    Keymap
	btnCh chan Button
}

// OpenSDLHelper spawns helperPath and negotiates a W×H RGB565 stream. panelW/panelH is the
// real panel size (the caller composes and Presents Canvases of exactly this size — the
// helper centers them, but matching sizes means a full-screen 1:1 flip). rawLog, if set,
// receives a copy of every diagnostic line the helper emits on fd 3 (the "#surface" line —
// used by the spike log to record the real surface geometry). env is passed through (e.g.
// LD_LIBRARY_PATH the pak set for /usr/trimui/lib SDL2).
// km selects input forwarding: pass the platform keymap to consume the helper's forwarded key
// presses (the card SDL lane, where SDL grabs the keypad); pass nil for a display-only helper
// (the spike keeps its own evdev input).
func OpenSDLHelper(helperPath string, panelW, panelH int, rawLog io.Writer, extraEnv []string, km Keymap) (*SDLHelper, error) {
	if panelW < 1 || panelH < 1 {
		return nil, fmt.Errorf("sdlhelper: bad panel size %dx%d", panelW, panelH)
	}
	cmd := exec.Command(helperPath)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// fd 3 in the child = write end of diagPipe; the helper writes a single "#surface" diag
	// line here at startup (no button events — it reads no input). ExtraFiles[0] => fd 3.
	diagR, diagW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{diagW}

	h := &SDLHelper{
		cmd:    cmd,
		stdin:  stdin,
		w:      panelW,
		h:      panelH,
		buf:    make([]byte, panelW*panelH*2),
		rawLog: rawLog,
		km:     km,
	}
	if km != nil {
		h.btnCh = make(chan Button, 16)
	}

	if err := cmd.Start(); err != nil {
		diagR.Close()
		diagW.Close()
		return nil, err
	}
	diagW.Close() // parent keeps only the read end

	// send the header immediately so the helper can size its source surface.
	if _, err := stdin.Write(buildHeader(panelW, panelH)); err != nil {
		h.hdrErr = err
	}

	go h.readDiag(diagR)
	return h, nil
}

// readDiag tees the helper's fd-3 diagnostic lines (just "#surface ...") to rawLog. It never
// produces input; the helper writes no button events. Drains and exits on the child's death.
func (h *SDLHelper) readDiag(r *os.File) {
	defer r.Close()
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		t := sc.Text()
		if h.rawLog != nil {
			fmt.Fprintln(h.rawLog, t)
		}
		// Forwarded input (miyoomini SDL lane): "#btn code=<scancode>" per key press. Decode via
		// the platform keymap and deliver as a logical Button. Non-blocking send so a full channel
		// (a card view that isn't reading) never stalls the diag reader.
		if h.km != nil && h.btnCh != nil && strings.HasPrefix(t, "#btn code=") {
			if code, err := strconv.Atoi(strings.TrimPrefix(t, "#btn code=")); err == nil {
				if b := h.km[uint16(code)]; b != BtnNone {
					select {
					case h.btnCh <- b:
					default:
					}
				}
			}
		}
	}
}

// Buttons yields forwarded key presses so the helper can BE the card's InputSource on the SDL
// lane (its SDL owns the grabbed keypad — a separate evdev reader is starved). Returns nil when
// the helper was opened display-only (km == nil), in which case input comes from elsewhere.
func (h *SDLHelper) Buttons() <-chan Button { return h.btnCh }

// Present encodes c to RGB565 and writes one frame to the helper. c must be W×H (the panel
// size passed to OpenSDLHelper); a mismatched canvas is centered by the helper. A write
// error (helper died) is returned so the caller can degrade to "launch anyway".
func (h *SDLHelper) Present(c *Canvas) error {
	if h.hdrErr != nil {
		return h.hdrErr
	}
	need := c.W * c.H * 2
	if cap(h.buf) < need {
		h.buf = make([]byte, need)
	}
	h.buf = h.buf[:need]
	EncodeRGB565(c, h.buf[:need])
	_, err := h.stdin.Write(h.buf[:need])
	return err
}

// Xres/Yres mirror Framebuffer so the caller sizes its Canvas to the panel.
func (h *SDLHelper) Xres() int { return h.w }
func (h *SDLHelper) Yres() int { return h.h }

// Close signals the helper to exit (closing stdin => the helper's read_full hits EOF and it
// SDL_Quit()s WITHOUT blanking the panel), then reaps it. Idempotent.
func (h *SDLHelper) Close() error {
	h.once.Do(func() {
		_ = h.stdin.Close() // EOF => clean helper exit
		if h.cmd.Process != nil {
			_ = h.cmd.Wait()
		}
	})
	return nil
}
