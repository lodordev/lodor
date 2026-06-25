// Package cover fetches a RomM box-art cover for a ROM and writes it to the NextUI
// game-artwork convention (BLUEPRINT §11): Roms/<System>/.media/<rom-basename>.png,
// where <rom-basename> is the ROM's on-disk filename WITHOUT its extension — exactly
// the basename the launcher derives from the highlighted/opened ROM. NextUI requires
// the file be a PNG; this package decodes RomM's served PNG, scales it down to a
// panel-friendly thumbnail, and re-encodes PNG.
//
// Sizing: RomM serves cover/small.png at ~282x280. On a 640x480 panel a Details-view
// cover need be no larger than ~200px on its long edge; left raw, 6,000 covers at
// ~48KB would cost ~290MB of card. We scale to a max long edge (maxEdge) preserving
// aspect, which drops each thumbnail to roughly 12-25KB and the whole library to well
// under 100MB. The scaler is a stdlib-only nearest/area box reducer (no golang.org/x,
// no CGO) — quality is fine for a thumbnail and the decode/encode use only
// image, image/png from the standard library.
//
// CGO-free, stdlib only.
package cover

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
)

// MaxEdge is the maximum length (px) of a saved cover's long edge. A cover whose long
// edge is already <= MaxEdge is written THROUGH unmodified (RomM's own PNG, ~48KB for
// the 282x280 small variant, compresses better than a Go re-encode — re-encoding it
// made files LARGER in testing). Only an oversized cover (the ~705x700 large fallback)
// is decoded, box-reduced to MaxEdge, and re-encoded. 320 keeps RomM's small.png
// (282x280) as-is while still bounding the rare large-only cover for a 640x480 panel.
const MaxEdge = 320

// coverDownloader is the one capability this package needs from *romm.Client, kept as
// an interface so the writer is testable without a live server.
type coverDownloader interface {
	DownloadCover(coverPath string) ([]byte, error)
}

// MediaPath returns the NextUI artwork path for a ROM given its absolute on-disk ROM
// path: <dir>/.media/<basename-without-ext>.png. For "Roms/GB/Tetris (USA).gb" it
// returns "Roms/GB/.media/Tetris (USA).png". A multi-file ROM's romPath is its .m3u,
// so the cover lands beside it as ".media/<m3u-stem>.png" — the same name the
// launcher computes from the visible entry.
func MediaPath(romPath string) string {
	dir := filepath.Dir(romPath)
	base := filepath.Base(romPath)
	ext := filepath.Ext(base)
	stem := base[:len(base)-len(ext)]
	return filepath.Join(dir, ".media", stem+".png")
}

// Exists reports whether a cover already exists (non-empty) at the media path for
// romPath — the skip-existing gate so a re-run never refetches.
func Exists(romPath string) bool {
	fi, err := os.Stat(MediaPath(romPath))
	return err == nil && fi.Size() > 0
}

// Outcome is the result of one FetchAndSave call, for honest progress/diagnostics.
type Outcome int

const (
	OutcomeSaved   Outcome = iota // fetched, scaled, written
	OutcomeSkipped                // already present (skip-existing)
	OutcomeNoCover                // rom has no cover (unidentified) — not an error
	OutcomeError                  // fetch/decode/write failed — non-fatal to the mirror
)

// FetchAndSave downloads romPath's cover from RomM and writes the scaled PNG to its
// .media path. It is graceful by contract: a rom with no cover returns OutcomeNoCover
// (nil err), an already-present cover returns OutcomeSkipped without a network call,
// and any fetch/decode/write failure returns OutcomeError with the error so the caller
// can count it WITHOUT aborting a 6,000-item mirror. coverPath is rom.CoverPath().
func FetchAndSave(dl coverDownloader, coverPath, romPath string) (Outcome, error) {
	if coverPath == "" {
		return OutcomeNoCover, nil
	}
	if Exists(romPath) {
		return OutcomeSkipped, nil
	}
	raw, err := dl.DownloadCover(coverPath)
	if err != nil {
		return OutcomeError, fmt.Errorf("download: %w", err)
	}
	if len(raw) == 0 {
		return OutcomeError, fmt.Errorf("empty cover body")
	}
	scaled, err := scalePNG(raw, MaxEdge)
	if err != nil {
		return OutcomeError, fmt.Errorf("scale: %w", err)
	}
	if err := writeAtomic(MediaPath(romPath), scaled); err != nil {
		return OutcomeError, fmt.Errorf("write: %w", err)
	}
	return OutcomeSaved, nil
}

// scalePNG decodes a PNG, box-reduces it so its long edge is <= maxEdge (never
// upscaling), and re-encodes PNG. Decode/encode are stdlib (image, image/png); the
// reducer is a stdlib-only area-average over an NRGBA target — no third-party deps,
// no CGO. A non-PNG body (or a decode failure) is an error the caller treats as a
// per-item failure, not a mirror abort.
func scalePNG(raw []byte, maxEdge int) ([]byte, error) {
	// Cheap pre-check: decode only the header to learn dimensions. If the cover is
	// already within bounds, return RomM's original bytes UNCHANGED — no re-encode,
	// no bloat. This is the hot path (RomM's small.png is 282x280, under maxEdge).
	if cfg, _, derr := image.DecodeConfig(bytes.NewReader(raw)); derr == nil {
		long := cfg.Width
		if cfg.Height > long {
			long = cfg.Height
		}
		if long <= maxEdge {
			return raw, nil
		}
	}

	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, fmt.Errorf("zero-size image")
	}

	dw, dh := sw, sh
	long := sw
	if sh > long {
		long = sh
	}
	if long > maxEdge {
		// Preserve aspect; scale the long edge to maxEdge.
		if sw >= sh {
			dw = maxEdge
			dh = sh * maxEdge / sw
		} else {
			dh = maxEdge
			dw = sw * maxEdge / sh
		}
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
	}

	var out image.Image
	if dw == sw && dh == sh {
		out = src
	} else {
		out = boxReduce(src, dw, dh)
	}

	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// boxReduce downscales src to dw x dh by averaging the source pixels that map to each
// destination pixel (a simple area/box filter). Pure stdlib: it reads via the
// image.Image color model and writes NRGBA. Good enough for a small box-art
// thumbnail and entirely CGO-free.
func boxReduce(src image.Image, dw, dh int) *image.NRGBA {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	dst := image.NewNRGBA(image.Rect(0, 0, dw, dh))

	for dy := 0; dy < dh; dy++ {
		sy0 := sb.Min.Y + dy*sh/dh
		sy1 := sb.Min.Y + (dy+1)*sh/dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dw; dx++ {
			sx0 := sb.Min.X + dx*sw/dw
			sx1 := sb.Min.X + (dx+1)*sw/dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rs, gs, bs, as uint64
			var n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, b, a := src.At(sx, sy).RGBA() // 16-bit pre-multiplied
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(b)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			// RGBA() is alpha-premultiplied 16-bit; convert the averaged values back to
			// 8-bit straight alpha for NRGBA. Un-premultiply when alpha>0.
			ar := as / n
			rr := rs / n
			gg := gs / n
			bb := bs / n
			var r8, g8, b8, a8 uint8
			a8 = uint8(ar >> 8)
			if ar == 0 {
				r8, g8, b8 = 0, 0, 0
			} else {
				r8 = uint8(((rr * 0xffff) / ar) >> 8)
				g8 = uint8(((gg * 0xffff) / ar) >> 8)
				b8 = uint8(((bb * 0xffff) / ar) >> 8)
			}
			i := dst.PixOffset(dx, dy)
			dst.Pix[i+0] = r8
			dst.Pix[i+1] = g8
			dst.Pix[i+2] = b8
			dst.Pix[i+3] = a8
		}
	}
	return dst
}

// writeAtomic writes data to path via a temp file + rename so a reader (the launcher)
// never sees a partial PNG. Creates the .media parent dir as needed.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
