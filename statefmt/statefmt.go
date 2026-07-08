// Package statefmt normalizes libretro save-state containers (Handoff v1).
//
// Two container formats exist in the fleet, both read from source 2026-07-06/07
// (design: vault lodor-statesync-design-2026-07-07.md, formats doc, RA task_save.c
// and rzip_stream.c read at master):
//
//   - minarch (LodorOS/NextUI): RAW core payload — retro_serialize() bytes, no
//     header, no compression. The canonical fleet format: everything stored on the
//     server is raw.
//   - RetroArch ≥1.9.11 (muOS/Knulli/OnionOS): RASTATE container — 8-byte header
//     "RASTATE"+version, then 8-byte-aligned blocks of [4B marker][4B size LE]
//     [payload]: "MEM " (the core payload, required), "ACHV" (cheevos, optional),
//     "RPLY" (replay, optional), "END ". Optionally the WHOLE stream is wrapped in
//     RZIP: 8-byte magic '#RZIPv'+<version byte>+'#', u32 LE chunk size, u64 LE
//     uncompressed size, then chunks of [u32 LE compressed length][zlib stream].
//     muOS ships savestate_file_compression=false (verified); Knulli's flag is
//     unconfirmed — the reader handles both regardless.
//
// Normalization contract (design D9): ingest ANY of the above → raw payload;
// deliver raw to minarch lanes verbatim, deliver WrapRASTATE(raw) to RA lanes
// (RA's loader has a raw fallback, source-verified, but we wrap so we never
// depend on it). ACHV/RPLY are noted and DISCARDED — they are device-local
// frontend state, never fleet state.
//
// Everything here fails closed: any structural anomaly is an error, and an
// artifact that fails to parse is never delivered (invariant 7.4). Pure package:
// no I/O, no logging, fully unit-tested against golden fixtures.
//
// ENDIANNESS [certain]: RASTATE block sizes are little-endian BY CONSTRUCTION on
// every platform — RA's content_write_block_header writes explicit LE byte stores
// (task_save.c:425-432, read 2026-07-07: output[4]=len&0xFF; output[5]=len>>8; …).
// RZIP fields likewise (rzip_stream.c header parse, LE composition). A production
// fixture check also confirmed the RAW path end-to-end: Grout's "builtin" emulator
// uploads BARE payloads (no container) — state 618, 202816B gambatte, passes
// through ExtractRaw untouched.
package statefmt

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Kind is the detected container shape of an ingested state file.
type Kind int

const (
	KindRaw     Kind = iota // no known container: treated as bare core payload
	KindRastate             // RASTATE container, uncompressed
	KindRzip                // RZIP-compressed stream (inner shape in Meta.Inner)
)

func (k Kind) String() string {
	switch k {
	case KindRaw:
		return "raw"
	case KindRastate:
		return "rastate"
	case KindRzip:
		return "rzip"
	default:
		return "unknown"
	}
}

// Meta describes what ExtractRaw found and discarded — recorded in the state
// ledger for honesty ("this state carried cheevos data we dropped"), never
// needed to reconstruct the payload.
type Meta struct {
	Kind        Kind
	Inner       Kind // for KindRzip: shape found inside after inflation
	RastateVer  byte
	HadCheevos  bool // ACHV block present (discarded)
	HadReplay   bool // RPLY block present (discarded)
	RawSize     int  // size of the extracted core payload
}

// Hard limits (fail closed — a lying header must never allocate the moon).
// RZIP chunk cap mirrors RA's own sanity cap; total cap is far above any real
// console state (PSX ~4-8MiB, N64 ~16-25MiB) while blocking absurdity.
const (
	maxRzipChunk = 64 << 20  // 64 MiB per chunk
	maxTotal     = 512 << 20 // 512 MiB total uncompressed
	rzipHeader   = 20
	rastateHdr   = 8
	blockHdr     = 8 // 4B marker + 4B size
	blockAlign   = 8
)

var (
	rzipMagicPre  = []byte{'#', 'R', 'Z', 'I', 'P', 'v'} // + version byte + '#'
	rastateMagic  = []byte("RASTATE")
	markerMem     = [4]byte{'M', 'E', 'M', ' '}
	markerCheevos = [4]byte{'A', 'C', 'H', 'V'}
	markerReplay  = [4]byte{'R', 'P', 'L', 'Y'}
	markerEnd     = [4]byte{'E', 'N', 'D', ' '}
)

// ErrNoMem is returned when a RASTATE container carries no MEM block — a state
// with no core payload is not a state.
var ErrNoMem = errors.New("statefmt: RASTATE container has no MEM block")

// Detect reports the outermost container shape without parsing further.
func Detect(data []byte) Kind {
	// Magic match is on the 8-byte prefix alone: a file that CLAIMS to be RZIP but
	// is too short for the full header must classify as RZIP and then FAIL in
	// rzipInflate — degrading it to "raw" would deliver garbage as a core payload.
	if len(data) >= 8 && bytes.HasPrefix(data, rzipMagicPre) && data[7] == '#' {
		return KindRzip
	}
	if len(data) >= rastateHdr && bytes.HasPrefix(data, rastateMagic) {
		return KindRastate
	}
	return KindRaw
}

// ExtractRaw normalizes any supported container to the bare core payload.
// Raw input passes through untouched (zero-copy slice).
func ExtractRaw(data []byte) ([]byte, Meta, error) {
	meta := Meta{Kind: Detect(data)}
	switch meta.Kind {
	case KindRzip:
		inflated, err := rzipInflate(data)
		if err != nil {
			return nil, meta, err
		}
		meta.Inner = Detect(inflated)
		if meta.Inner == KindRzip {
			// Nested RZIP is not a thing RA produces; refuse rather than recurse.
			return nil, meta, errors.New("statefmt: nested RZIP refused")
		}
		if meta.Inner == KindRastate {
			raw, ver, achv, rply, err := rastateExtract(inflated)
			if err != nil {
				return nil, meta, err
			}
			meta.RastateVer, meta.HadCheevos, meta.HadReplay = ver, achv, rply
			meta.RawSize = len(raw)
			return raw, meta, nil
		}
		meta.RawSize = len(inflated)
		return inflated, meta, nil
	case KindRastate:
		raw, ver, achv, rply, err := rastateExtract(data)
		if err != nil {
			return nil, meta, err
		}
		meta.RastateVer, meta.HadCheevos, meta.HadReplay = ver, achv, rply
		meta.RawSize = len(raw)
		return raw, meta, nil
	default:
		meta.RawSize = len(data)
		return data, meta, nil
	}
}

// WrapRASTATE builds a minimal RASTATE v1 container around a raw core payload:
// header, one MEM block, END. Exactly what RA's loader needs, nothing that
// isn't ours to ship (never ACHV/RPLY — design D9).
func WrapRASTATE(raw []byte) []byte {
	memPadded := aligned(len(raw))
	out := make([]byte, 0, rastateHdr+blockHdr+memPadded+blockHdr)
	out = append(out, rastateMagic...)
	out = append(out, 1) // RASTATE_VERSION
	out = appendBlockHeader(out, markerMem, uint32(len(raw)))
	out = append(out, raw...)
	out = append(out, make([]byte, memPadded-len(raw))...)
	out = appendBlockHeader(out, markerEnd, 0)
	return out
}

func appendBlockHeader(out []byte, marker [4]byte, size uint32) []byte {
	out = append(out, marker[:]...)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], size)
	return append(out, b[:]...)
}

func aligned(n int) int {
	if r := n % blockAlign; r != 0 {
		return n + (blockAlign - r)
	}
	return n
}

// rastateExtract walks the block chain and returns the MEM payload.
func rastateExtract(data []byte) (raw []byte, ver byte, achv, rply bool, err error) {
	if len(data) < rastateHdr+blockHdr {
		return nil, 0, false, false, errors.New("statefmt: RASTATE truncated before first block")
	}
	ver = data[7]
	pos := rastateHdr
	for {
		if pos+blockHdr > len(data) {
			return nil, ver, achv, rply, errors.New("statefmt: RASTATE ran out of bytes before END block")
		}
		var marker [4]byte
		copy(marker[:], data[pos:pos+4])
		size := binary.LittleEndian.Uint32(data[pos+4 : pos+8])
		pos += blockHdr
		if size > uint32(maxTotal) || pos+int(size) > len(data) {
			return nil, ver, achv, rply, fmt.Errorf("statefmt: RASTATE block %q size %d exceeds data", marker, size)
		}
		switch marker {
		case markerMem:
			if raw != nil {
				return nil, ver, achv, rply, errors.New("statefmt: duplicate MEM block")
			}
			raw = data[pos : pos+int(size)]
		case markerCheevos:
			achv = true
		case markerReplay:
			rply = true
		case markerEnd:
			if raw == nil {
				return nil, ver, achv, rply, ErrNoMem
			}
			return raw, ver, achv, rply, nil
		default:
			// Unknown block: skip by declared size (forward-compat), same as RA.
		}
		pos += aligned(int(size))
	}
}

// rzipInflate decompresses an RZIP stream: 20-byte header (magic, u32 LE chunk
// size, u64 LE total size), then [u32 LE compressed length][zlib bytes] chunks.
func rzipInflate(data []byte) ([]byte, error) {
	if len(data) < rzipHeader {
		return nil, errors.New("statefmt: RZIP truncated header")
	}
	chunkSize := binary.LittleEndian.Uint32(data[8:12])
	total := binary.LittleEndian.Uint64(data[12:20])
	if chunkSize == 0 || chunkSize > maxRzipChunk {
		return nil, fmt.Errorf("statefmt: RZIP chunk size %d out of bounds", chunkSize)
	}
	if total > maxTotal {
		return nil, fmt.Errorf("statefmt: RZIP declared size %d exceeds cap", total)
	}
	out := make([]byte, 0, int(total))
	pos := rzipHeader
	for pos < len(data) {
		if pos+4 > len(data) {
			return nil, errors.New("statefmt: RZIP truncated chunk header")
		}
		clen := binary.LittleEndian.Uint32(data[pos : pos+4])
		pos += 4
		if clen == 0 || pos+int(clen) > len(data) {
			return nil, errors.New("statefmt: RZIP chunk length exceeds data")
		}
		zr, err := zlib.NewReader(bytes.NewReader(data[pos : pos+int(clen)]))
		if err != nil {
			return nil, fmt.Errorf("statefmt: RZIP chunk zlib: %w", err)
		}
		chunk, err := io.ReadAll(io.LimitReader(zr, int64(chunkSize)+1))
		zr.Close()
		if err != nil {
			return nil, fmt.Errorf("statefmt: RZIP chunk inflate: %w", err)
		}
		if len(chunk) > int(chunkSize) {
			return nil, errors.New("statefmt: RZIP chunk inflated past declared chunk size")
		}
		if len(out)+len(chunk) > int(total) {
			return nil, errors.New("statefmt: RZIP inflated past declared total")
		}
		out = append(out, chunk...)
		pos += int(clen)
	}
	if uint64(len(out)) != total {
		return nil, fmt.Errorf("statefmt: RZIP inflated %d bytes, header declared %d", len(out), total)
	}
	return out, nil
}
