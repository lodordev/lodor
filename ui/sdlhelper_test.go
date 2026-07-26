package ui

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

func TestEncodeRGB565(t *testing.T) {
	c := NewCanvas(2, 1)
	c.Set(0, 0, Color(0xFF0000)) // red
	c.Set(1, 0, Color(0x0000FF)) // blue
	dst := make([]byte, 2*2*1)
	EncodeRGB565(c, dst)
	// red = 0xF800, blue = 0x001F, little-endian bytes.
	got0 := uint16(dst[0]) | uint16(dst[1])<<8
	got1 := uint16(dst[2]) | uint16(dst[3])<<8
	if got0 != 0xF800 {
		t.Errorf("red: got %#04x want 0xF800", got0)
	}
	if got1 != 0x001F {
		t.Errorf("blue: got %#04x want 0x001F", got1)
	}
}

func TestEncodeRGB565Green(t *testing.T) {
	c := NewCanvas(1, 1)
	c.Set(0, 0, Color(0x00FF00))
	dst := make([]byte, 2)
	EncodeRGB565(c, dst)
	got := uint16(dst[0]) | uint16(dst[1])<<8
	if got != 0x07E0 {
		t.Errorf("green: got %#04x want 0x07E0", got)
	}
}

func TestBuildHeader(t *testing.T) {
	h := buildHeader(1280, 720)
	if !bytes.Equal(h[0:4], []byte("LFB1")) {
		t.Fatalf("magic: %q", h[0:4])
	}
	if binary.LittleEndian.Uint32(h[4:]) != 1280 {
		t.Errorf("w: %d", binary.LittleEndian.Uint32(h[4:]))
	}
	if binary.LittleEndian.Uint32(h[8:]) != 720 {
		t.Errorf("h: %d", binary.LittleEndian.Uint32(h[8:]))
	}
	if binary.LittleEndian.Uint32(h[12:]) != 2 {
		t.Errorf("bpp: %d", binary.LittleEndian.Uint32(h[12:]))
	}
}

// TestSDLHelperDisplayRoundTrip runs the DISPLAY-ONLY Go backend against a FAKE helper (a
// shell script, no SDL): it validates that the 16-byte header + one RGB565 frame are read
// off stdin, and that the helper's single "#surface" diag line on fd 3 is tee'd to rawLog.
// The helper produces NO buttons (input is the Go EvdevSource, tested separately) — this
// proves the display half of the lane split off-device.
func TestSDLHelperDisplayRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	// Fake helper: emit the #surface diag line on fd 3, then drain header+frame from stdin
	// (16 hdr + 8 frame for a 2x2 RGB565 panel = 24 bytes), then exit (EOF on our stdin
	// after Close()). It writes NO button lines — a display-only helper never does.
	script := `
printf '#surface w=2 h=2 bpp=16 wantw=2 wanth=2 bp=2 input=evdev-go\n' >&3
head -c 24 >/dev/null
sleep 1
`
	f, err := os.CreateTemp("", "fakehelper-*.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	f.WriteString(script)
	f.Close()

	rawLog := &syncBuf{}
	h2, err := openSDLHelperArgs("sh", []string{f.Name()}, 2, 2, rawLog)
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Close()

	c := NewCanvas(2, 2)
	c.Clear(Color(0xFF0000))
	if err := h2.Present(c); err != nil {
		t.Fatalf("present: %v", err)
	}

	// The #surface diag line must reach rawLog (best-effort, give the reader a moment).
	deadline := time.After(3 * time.Second)
	for !bytes.Contains(rawLog.snapshot(), []byte("#surface")) {
		select {
		case <-deadline:
			t.Fatalf("rawLog never received #surface line: %q", string(rawLog.snapshot()))
		case <-time.After(20 * time.Millisecond):
		}
	}
	if h2.Xres() != 2 || h2.Yres() != 2 {
		t.Errorf("Xres/Yres = %dx%d, want 2x2", h2.Xres(), h2.Yres())
	}
}

// openSDLHelperArgs is a test-only variant of OpenSDLHelper that allows argv (so the fake
// helper can be `sh script.sh`). Production always uses the argless binary path.
func openSDLHelperArgs(path string, args []string, w, h int, rawLog io.Writer) (*SDLHelper, error) {
	cmd := exec.Command(path, args...)
	cmd.Env = os.Environ()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	diagR, diagW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{diagW}
	sh := &SDLHelper{cmd: cmd, stdin: stdin, w: w, h: h, buf: make([]byte, w*h*2), rawLog: rawLog}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	diagW.Close()
	if _, err := stdin.Write(buildHeader(w, h)); err != nil {
		sh.hdrErr = err
	}
	go sh.readDiag(diagR)
	return sh, nil
}

// syncBuf is a mutex-guarded byte sink: the helper's readDiag goroutine writes
// to it while the test polls snapshot() -- a plain bytes.Buffer races there (the
// sink is io.Writer in production, where the single reader is a session-log file).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) snapshot() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}
