package ui

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

// Linux framebuffer backend. Opens /dev/fb0, queries geometry via ioctl, mmaps the
// buffer, and blits a Canvas honoring the device's pixel format (offsets/lengths from
// fb_var_screeninfo). muOS sets the H700 panel to 32bpp (mufbset -d 32), but we read the
// real format rather than assume, so a 16bpp (RGB565) panel works too. Stdlib syscall only.

const (
	fbiogetVSCREENINFO = 0x4600
	fbiogetFSCREENINFO = 0x4602
)

// Framebuffer is an open /dev/fb0 with its geometry and a mmap'd pixel buffer.
type Framebuffer struct {
	f          *os.File
	mem        []byte
	xres, yres int
	bpp        int
	lineLength int
	rOff, rLen uint32
	gOff, gLen uint32
	bOff, bLen uint32
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// OpenFramebuffer opens dev (e.g. "/dev/fb0") and maps it. The returned Framebuffer's
// Xres/Yres should size the Canvas you Flush to it.
//
// TEST SEAM (off-hardware real-loop harness): if LODOR_FB_DEV is set, it OVERRIDES dev so
// a test can point the REAL blit path at a plain file. Production (env unset) is byte-for-
// byte unchanged: /dev/fb0 opens, the FBIOGET_*SCREENINFO ioctls succeed, and none of the
// test-backend code below runs. Only when LODOR_FB_DEV is set AND the ioctls fail with
// ENOTTY (a regular file, not an fb device) do we synthesize the geometry
// (fb_var_screeninfo / fb_fix_screeninfo) from LODOR_FB_GEOM (default 640x480x32 — a real
// RG34XX panel) and mmap the file. We fake ONLY the ioctl geometry; pack/Flush/mmap and
// everything downstream are the identical real code paths that run on-device.
func OpenFramebuffer(dev string) (*Framebuffer, error) {
	testDev := os.Getenv("LODOR_FB_DEV")
	if testDev != "" {
		dev = testDev
	}
	flags := os.O_RDWR
	if testDev != "" {
		flags |= os.O_CREATE // the harness hands us a path to (re)create as the backing file
	}
	f, err := os.OpenFile(dev, flags, 0o644)
	if err != nil {
		return nil, err
	}
	var vinfo [160]byte
	var finfo [80]byte
	ioErr := ioctl(f.Fd(), fbiogetVSCREENINFO, unsafe.Pointer(&vinfo[0]))
	if ioErr == nil {
		ioErr = ioctl(f.Fd(), fbiogetFSCREENINFO, unsafe.Pointer(&finfo[0]))
	}
	if ioErr != nil {
		if testDev == "" {
			// Production: a real /dev/fb0 that won't answer the ioctls is a hard, honest error.
			f.Close()
			return nil, fmt.Errorf("FBIOGET_*SCREENINFO: %w", ioErr)
		}
		// File-backed test framebuffer: the path is a regular file, so the ioctls returned
		// ENOTTY. Synthesize the two screeninfo blobs into the SAME byte offsets the real
		// driver fills, so the parse below is identical for real and fake.
		synthScreenInfo(vinfo[:], finfo[:])
	}
	le := binary.LittleEndian
	fb := &Framebuffer{
		f:    f,
		xres: int(le.Uint32(vinfo[0:])),
		yres: int(le.Uint32(vinfo[4:])),
		bpp:  int(le.Uint32(vinfo[24:])),
		// fb_bitfield {offset,length,msb_right} - red@32, green@44, blue@56.
		rOff: le.Uint32(vinfo[32:]), rLen: le.Uint32(vinfo[36:]),
		gOff: le.Uint32(vinfo[44:]), gLen: le.Uint32(vinfo[48:]),
		bOff: le.Uint32(vinfo[56:]), bLen: le.Uint32(vinfo[60:]),
		lineLength: int(le.Uint32(finfo[48:])), // fb_fix_screeninfo.line_length @ 48 (64-bit)
	}
	smemLen := int(le.Uint32(finfo[24:]))
	if smemLen <= 0 {
		smemLen = fb.lineLength * fb.yres
	}
	if testDev != "" {
		// The backing file must be at least smemLen bytes or the mmap'd region SIGBUSes on
		// first write. Size it exactly to the synthesized framebuffer.
		if err := f.Truncate(int64(smemLen)); err != nil {
			f.Close()
			return nil, fmt.Errorf("size test fb %q: %w", dev, err)
		}
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, smemLen, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("mmap fb: %w", err)
	}
	fb.mem = mem
	// Sane default bitfields if the driver reported none (some report all-zero for 32bpp).
	if fb.rLen == 0 && fb.gLen == 0 && fb.bLen == 0 {
		if fb.bpp == 16 {
			fb.rOff, fb.rLen = 11, 5
			fb.gOff, fb.gLen = 5, 6
			fb.bOff, fb.bLen = 0, 5
		} else {
			fb.rOff, fb.rLen = 16, 8
			fb.gOff, fb.gLen = 8, 8
			fb.bOff, fb.bLen = 0, 8
		}
	}
	return fb, nil
}

func (fb *Framebuffer) Xres() int { return fb.xres }
func (fb *Framebuffer) Yres() int { return fb.yres }
func (fb *Framebuffer) Bpp() int  { return fb.bpp }

func (fb *Framebuffer) pack(r, g, b uint32) uint32 {
	return (r>>(8-fb.rLen))<<fb.rOff | (g>>(8-fb.gLen))<<fb.gOff | (b>>(8-fb.bLen))<<fb.bOff
}

// Flush blits the canvas to the framebuffer. The canvas is assumed sized to the panel;
// pixels outside the panel are clipped. Honors line_length stride and the device bpp.
func (fb *Framebuffer) Flush(c *Canvas) {
	bytesPP := fb.bpp / 8
	if bytesPP < 2 {
		bytesPP = 4
	}
	maxY := c.H
	if fb.yres < maxY {
		maxY = fb.yres
	}
	maxX := c.W
	if fb.xres < maxX {
		maxX = fb.xres
	}
	le := binary.LittleEndian
	for y := 0; y < maxY; y++ {
		rowOff := y * fb.lineLength
		srcRow := y * c.W
		for x := 0; x < maxX; x++ {
			r, g, b := c.Pix[srcRow+x].rgb()
			px := fb.pack(r, g, b)
			off := rowOff + x*bytesPP
			if off+bytesPP > len(fb.mem) {
				continue
			}
			if bytesPP == 2 {
				le.PutUint16(fb.mem[off:], uint16(px))
			} else {
				le.PutUint32(fb.mem[off:], px)
			}
		}
	}
}

// Close unmaps and closes the framebuffer.
func (fb *Framebuffer) Close() error {
	if fb.mem != nil {
		_ = syscall.Munmap(fb.mem)
		fb.mem = nil
	}
	return fb.f.Close()
}

// synthScreenInfo writes a synthetic fb_var_screeninfo (vinfo) + fb_fix_screeninfo (finfo)
// into the SAME byte offsets the kernel driver populates, so OpenFramebuffer's real parse
// produces a valid Framebuffer over a plain file. Geometry comes from LODOR_FB_GEOM
// ("WxHxBPP", default 640x480x32 — a real RG34XX panel). Only 16bpp (RGB565) and 32bpp
// (XRGB8888) are modeled; anything else falls back to 32bpp. TEST-ONLY.
func synthScreenInfo(vinfo, finfo []byte) {
	xres, yres, bpp := 640, 480, 32
	if g := os.Getenv("LODOR_FB_GEOM"); g != "" {
		p := strings.Split(strings.ToLower(g), "x")
		if len(p) == 3 {
			if v, err := strconv.Atoi(p[0]); err == nil && v > 0 {
				xres = v
			}
			if v, err := strconv.Atoi(p[1]); err == nil && v > 0 {
				yres = v
			}
			if v, err := strconv.Atoi(p[2]); err == nil && (v == 16 || v == 32) {
				bpp = v
			}
		}
	}
	le := binary.LittleEndian
	le.PutUint32(vinfo[0:], uint32(xres)) // xres
	le.PutUint32(vinfo[4:], uint32(yres)) // yres
	le.PutUint32(vinfo[24:], uint32(bpp)) // bits_per_pixel
	// fb_bitfield {offset,length,msb_right} for red@32, green@44, blue@56.
	if bpp == 16 {
		le.PutUint32(vinfo[32:], 11)
		le.PutUint32(vinfo[36:], 5) // red
		le.PutUint32(vinfo[44:], 5)
		le.PutUint32(vinfo[48:], 6) // green
		le.PutUint32(vinfo[56:], 0)
		le.PutUint32(vinfo[60:], 5) // blue
	} else {
		le.PutUint32(vinfo[32:], 16)
		le.PutUint32(vinfo[36:], 8) // red
		le.PutUint32(vinfo[44:], 8)
		le.PutUint32(vinfo[48:], 8) // green
		le.PutUint32(vinfo[56:], 0)
		le.PutUint32(vinfo[60:], 8) // blue
	}
	lineLength := xres * (bpp / 8)
	le.PutUint32(finfo[24:], uint32(lineLength*yres)) // smem_len
	le.PutUint32(finfo[48:], uint32(lineLength))      // line_length
}

// unpack is the inverse of pack: recover 8-bit r,g,b from a packed device pixel. Used only
// by SavePNG to read a blitted frame back for off-hardware verification.
func (fb *Framebuffer) unpack(px uint32) (r, g, b uint8) {
	ex := func(off, ln uint32) uint8 {
		if ln == 0 {
			return 0
		}
		v := (px >> off) & ((1 << ln) - 1)
		return uint8(v << (8 - ln)) // left-justify back to 8-bit
	}
	return ex(fb.rOff, fb.rLen), ex(fb.gOff, fb.gLen), ex(fb.bOff, fb.bLen)
}

// SavePNG reads the CURRENT framebuffer contents back out of the mmap'd memory and writes a
// PNG — the off-hardware frame dump for the real-loop harness. Because it reads what Flush
// actually blitted (through pack + the device pixel format), it verifies the whole render
// pipeline end to end, not just the in-memory Canvas. Stdlib image/png only.
func (fb *Framebuffer) SavePNG(path string) error {
	if fb.mem == nil {
		return fmt.Errorf("framebuffer closed")
	}
	bytesPP := fb.bpp / 8
	if bytesPP < 2 {
		bytesPP = 4
	}
	le := binary.LittleEndian
	img := image.NewRGBA(image.Rect(0, 0, fb.xres, fb.yres))
	for y := 0; y < fb.yres; y++ {
		rowOff := y * fb.lineLength
		for x := 0; x < fb.xres; x++ {
			off := rowOff + x*bytesPP
			if off+bytesPP > len(fb.mem) {
				continue
			}
			var px uint32
			if bytesPP == 2 {
				px = uint32(le.Uint16(fb.mem[off:]))
			} else {
				px = le.Uint32(fb.mem[off:])
			}
			r, g, b := fb.unpack(px)
			img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 0xff})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
