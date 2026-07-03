package ui

import (
	"encoding/binary"
	"fmt"
	"os"
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
func OpenFramebuffer(dev string) (*Framebuffer, error) {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	var vinfo [160]byte
	if err := ioctl(f.Fd(), fbiogetVSCREENINFO, unsafe.Pointer(&vinfo[0])); err != nil {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_VSCREENINFO: %w", err)
	}
	var finfo [80]byte
	if err := ioctl(f.Fd(), fbiogetFSCREENINFO, unsafe.Pointer(&finfo[0])); err != nil {
		f.Close()
		return nil, fmt.Errorf("FBIOGET_FSCREENINFO: %w", err)
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
