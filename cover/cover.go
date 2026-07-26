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
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"lodor/fsutil"
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

// coverDownloaderCtx is the context-aware cover fetch capability, used by
// FetchAndSaveCtx so a bulk warm can cancel an in-flight download promptly (user
// B-press) or bound each cover with a short deadline. *romm.Client satisfies it via
// DownloadCoverCtx.
type coverDownloaderCtx interface {
	DownloadCoverCtx(ctx context.Context, coverPath string) ([]byte, error)
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
	return ExistsAt(MediaPath(romPath))
}

// ExistsAt is Exists for an explicit destination path (frontend-media placements,
// where the cover does not live beside the ROM).
func ExistsAt(dest string) bool {
	fi, err := os.Stat(dest)
	return err == nil && fi.Size() > 0
}

// EsdeCoverPath returns the ES-DE placement for a ROM's cover:
// <mediaRoot>/<system-folder>/covers/<rom-stem>.png, where <system-folder> is the
// ROM's parent directory name (Lodor's ES-DE es_systems.xml names each system after
// its ROM folder, and ES-DE's media tree keys on the system name) and <rom-stem> is
// the on-disk filename without extension — ES-DE (Android) matches media strictly by
// game-file basename. A multi-disc game's romPath is its .m3u, so the cover lands
// under the .m3u stem — the entry ES-DE actually lists.
func EsdeCoverPath(mediaRoot, romPath string) string {
	base := filepath.Base(romPath)
	stem := base[:len(base)-len(filepath.Ext(base))]
	return filepath.Join(mediaRoot, filepath.Base(filepath.Dir(romPath)), "covers", stem+".png")
}

// Placement says where a fetched cover goes and how it is styled. Dim is the
// stub treatment for frontends with no native cloud-state cue (ES-DE): the cover is
// darkened so a not-yet-downloaded game visibly reads as remote next to a bright,
// on-device one. The NextUI convention never dims (markers/size carry that state).
type Placement struct {
	Dest string
	Dim  bool
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
// (nil err), an already-present cover returns OutcomeSkipped without a network call
// (UNLESS force=true — a "Full" refresh re-fetches even existing covers), and any
// fetch/decode/write failure returns OutcomeError with the error so the caller can count
// it WITHOUT aborting a 6,000-item mirror. coverPath is rom.CoverPath().
func FetchAndSave(dl coverDownloader, coverPath, romPath string, force bool) (Outcome, error) {
	return FetchAndPlace(dl, coverPath, Placement{Dest: MediaPath(romPath)}, force)
}

// FetchAndPlace is FetchAndSave for an explicit Placement — same grace contract,
// caller-chosen destination and dim styling.
func FetchAndPlace(dl coverDownloader, coverPath string, p Placement, force bool) (Outcome, error) {
	if coverPath == "" {
		return OutcomeNoCover, nil
	}
	if !force && ExistsAt(p.Dest) {
		return OutcomeSkipped, nil
	}
	raw, err := dl.DownloadCover(coverPath)
	if err != nil {
		return OutcomeError, fmt.Errorf("download: %w", err)
	}
	return savePlaced(raw, p)
}

// FetchAndSaveCtx is FetchAndSave with a context threaded into the network fetch, so an
// in-flight cover download aborts the instant ctx is cancelled (user B-press) or its
// per-cover deadline fires — the fix for the "Cancelling" hang on the slow Miyoo radio
// (#26). Identical grace contract: no-cover / skip-existing short-circuit BEFORE any
// network call, and a cancelled/timed-out download is a per-item OutcomeError the caller
// counts without aborting the mirror. Callers that want prompt cancel should ALSO check
// their cancel signal between covers so the whole pass stops, not just the current fetch.
func FetchAndSaveCtx(ctx context.Context, dl coverDownloaderCtx, coverPath, romPath string, force bool) (Outcome, error) {
	return FetchAndPlaceCtx(ctx, dl, coverPath, Placement{Dest: MediaPath(romPath)}, force)
}

// FetchAndPlaceCtx is FetchAndSaveCtx for an explicit Placement — identical cancel
// and grace contract, caller-chosen destination and dim styling.
func FetchAndPlaceCtx(ctx context.Context, dl coverDownloaderCtx, coverPath string, p Placement, force bool) (Outcome, error) {
	if coverPath == "" {
		return OutcomeNoCover, nil
	}
	if !force && ExistsAt(p.Dest) {
		return OutcomeSkipped, nil
	}
	raw, err := dl.DownloadCoverCtx(ctx, coverPath)
	if err != nil {
		return OutcomeError, fmt.Errorf("download: %w", err)
	}
	return savePlaced(raw, p)
}

// savePlaced validates, scales, styles, and atomically writes fetched cover bytes to
// the placement. Shared tail of every fetch variant so all apply the identical
// scale + dim + atomic-write path.
func savePlaced(raw []byte, p Placement) (Outcome, error) {
	if len(raw) == 0 {
		return OutcomeError, fmt.Errorf("empty cover body")
	}
	scaled, err := scalePNG(raw, MaxEdge)
	if err != nil {
		return OutcomeError, fmt.Errorf("scale: %w", err)
	}
	if p.Dim {
		if scaled, err = dimPNG(scaled); err != nil {
			return OutcomeError, fmt.Errorf("dim: %w", err)
		}
	}
	if err := writeAtomic(p.Dest, scaled); err != nil {
		return OutcomeError, fmt.Errorf("write: %w", err)
	}
	return OutcomeSaved, nil
}

// dimFactorPct is how much of each color channel a dimmed stub cover keeps.
// 45% reads unmistakably "inactive" on ES-DE's dark theme while leaving the art
// recognizable at thumbnail size.
const dimFactorPct = 45

// dimPNG darkens a PNG by scaling every color channel to dimFactorPct, preserving
// alpha. Stdlib-only (image, image/png), straight-alpha NRGBA math.
func dimPNG(raw []byte) ([]byte, error) {
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bb, a := src.At(x, y).RGBA() // 16-bit premultiplied
			i := dst.PixOffset(x-b.Min.X, y-b.Min.Y)
			a8 := uint8(a >> 8)
			if a == 0 {
				dst.Pix[i+0], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = 0, 0, 0, 0
				continue
			}
			// Un-premultiply to straight alpha, then dim the color channels only.
			dst.Pix[i+0] = uint8((((r * 0xffff) / a) >> 8) * dimFactorPct / 100)
			dst.Pix[i+1] = uint8((((g * 0xffff) / a) >> 8) * dimFactorPct / 100)
			dst.Pix[i+2] = uint8((((bb * 0xffff) / a) >> 8) * dimFactorPct / 100)
			dst.Pix[i+3] = a8
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DimFileInPlace dims an already-placed cover file (evict: a downloaded game
// returning to stub state, offline — no refetch available or needed). Atomic
// rewrite; callers gate on manifest state so a dim cover is never double-dimmed.
func DimFileInPlace(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dimmed, err := dimPNG(raw)
	if err != nil {
		return err
	}
	return writeAtomic(path, dimmed)
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

// Dim returns a darkened, desaturated copy of a PNG cover — the "not downloaded yet"
// treatment for a cloud stub in a host-frontend grid. It is the download-state cue that
// replaces the ✘ marker once the marker is stripped from the visible title: a greyed,
// dimmed cover reads as "unavailable / not here", a normal cover as "on device". Pure
// stdlib (image/png), CGO-free. Best-effort: any decode/encode failure returns the input
// bytes unchanged (a normal cover beats no cover).
func Dim(raw []byte) []byte {
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw
	}
	sb := src.Bounds()
	dst := image.NewNRGBA(sb)
	for y := sb.Min.Y; y < sb.Max.Y; y++ {
		for x := sb.Min.X; x < sb.Max.X; x++ {
			r, g, b, a := src.At(x, y).RGBA() // 16-bit, alpha-premultiplied
			var r8, g8, b8, a8 uint8
			a8 = uint8(a >> 8)
			if a == 0 {
				r8, g8, b8 = 0, 0, 0
			} else {
				r8 = uint8(((r * 0xffff) / a) >> 8)
				g8 = uint8(((g * 0xffff) / a) >> 8)
				b8 = uint8(((b * 0xffff) / a) >> 8)
			}
			// Rec.601 luma; mix toward grey (retain ~40% chroma), then darken to ~45%.
			lum := (uint32(r8)*77 + uint32(g8)*150 + uint32(b8)*29) >> 8
			mix := func(c uint8) uint8 {
				v := (uint32(c)*40 + lum*60) / 100 // desaturate toward luma
				return uint8(v * 45 / 100)         // darken
			}
			dst.SetNRGBA(x, y, color.NRGBA{R: mix(r8), G: mix(g8), B: mix(b8), A: a8})
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return raw
	}
	return buf.Bytes()
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
	// FAT32-atomic: temp + fsync + rename + dir fsync (fsutil).
	return fsutil.WriteFileAtomic(path, data, 0o644)
}
