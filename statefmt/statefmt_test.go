package statefmt

// Golden-fixture and adversarial tests for the state normalizer. Fixtures are
// constructed from the source-verified specs (RA task_save.c / rzip_stream.c);
// a live-fixture validation against a real production RASTATE file is part of
// the branch's live test pass (build ledger), not this unit file.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

var payload = []byte("CORE-PAYLOAD-0123456789-not-aligned") // 35 bytes, deliberately unaligned

// buildRZIP is a test-only RZIP writer per rzip_stream.c's layout.
func buildRZIP(t *testing.T, raw []byte, chunkSize uint32) []byte {
	t.Helper()
	out := []byte{'#', 'R', 'Z', 'I', 'P', 'v', 1, '#'}
	var b4 [4]byte
	var b8 [8]byte
	binary.LittleEndian.PutUint32(b4[:], chunkSize)
	out = append(out, b4[:]...)
	binary.LittleEndian.PutUint64(b8[:], uint64(len(raw)))
	out = append(out, b8[:]...)
	for off := 0; off < len(raw); off += int(chunkSize) {
		end := off + int(chunkSize)
		if end > len(raw) {
			end = len(raw)
		}
		var zbuf bytes.Buffer
		zw := zlib.NewWriter(&zbuf)
		if _, err := zw.Write(raw[off:end]); err != nil {
			t.Fatal(err)
		}
		zw.Close()
		binary.LittleEndian.PutUint32(b4[:], uint32(zbuf.Len()))
		out = append(out, b4[:]...)
		out = append(out, zbuf.Bytes()...)
	}
	return out
}

// buildRastateWithExtras builds a container carrying ACHV and RPLY around MEM.
func buildRastateWithExtras(raw []byte) []byte {
	out := append([]byte("RASTATE"), 1)
	add := func(marker string, body []byte) {
		out = append(out, marker...)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(body)))
		out = append(out, b[:]...)
		out = append(out, body...)
		if pad := aligned(len(body)) - len(body); pad > 0 {
			out = append(out, make([]byte, pad)...)
		}
	}
	add("ACHV", []byte("cheevo-junk"))
	add("MEM ", raw)
	add("RPLY", []byte("replay-junk-longer-than-achv"))
	add("END ", nil)
	return out
}

func TestDetect(t *testing.T) {
	if k := Detect(payload); k != KindRaw {
		t.Fatalf("raw detected as %v", k)
	}
	if k := Detect(WrapRASTATE(payload)); k != KindRastate {
		t.Fatalf("rastate detected as %v", k)
	}
	if k := Detect(buildRZIP(t, payload, 16)); k != KindRzip {
		t.Fatalf("rzip detected as %v", k)
	}
}

func TestRawPassThrough(t *testing.T) {
	raw, meta, err := ExtractRaw(payload)
	if err != nil || !bytes.Equal(raw, payload) || meta.Kind != KindRaw {
		t.Fatalf("raw passthrough: %v meta=%+v", err, meta)
	}
}

func TestWrapExtractRoundTrip(t *testing.T) {
	wrapped := WrapRASTATE(payload)
	if len(wrapped)%blockAlign != 0 {
		t.Fatalf("wrapped container not 8-aligned: %d", len(wrapped))
	}
	raw, meta, err := ExtractRaw(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, payload) {
		t.Fatalf("round-trip mismatch: %q", raw)
	}
	if meta.Kind != KindRastate || meta.RastateVer != 1 || meta.HadCheevos || meta.HadReplay {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestExtrasAreNotedAndDiscarded(t *testing.T) {
	raw, meta, err := ExtractRaw(buildRastateWithExtras(payload))
	if err != nil || !bytes.Equal(raw, payload) {
		t.Fatalf("extras extract: %v", err)
	}
	if !meta.HadCheevos || !meta.HadReplay {
		t.Fatalf("extras not noted: %+v", meta)
	}
}

func TestRzipRoundTripMultiChunk(t *testing.T) {
	// chunk size 16 forces multiple chunks over the 35-byte payload
	rz := buildRZIP(t, WrapRASTATE(payload), 16)
	raw, meta, err := ExtractRaw(rz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, payload) {
		t.Fatalf("rzip(rastate) mismatch")
	}
	if meta.Kind != KindRzip || meta.Inner != KindRastate {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestRzipBareRaw(t *testing.T) {
	rz := buildRZIP(t, payload, 128)
	raw, meta, err := ExtractRaw(rz)
	if err != nil || !bytes.Equal(raw, payload) || meta.Inner != KindRaw {
		t.Fatalf("rzip(raw): %v meta=%+v", err, meta)
	}
}

func TestFailClosed(t *testing.T) {
	cases := map[string][]byte{
		"rastate-no-mem": append(append([]byte("RASTATE"), 1), []byte("END \x00\x00\x00\x00")...),
		"rastate-truncated": WrapRASTATE(payload)[:20],
		"rastate-lying-size": func() []byte {
			w := WrapRASTATE(payload)
			binary.LittleEndian.PutUint32(w[12:16], 1<<30) // MEM size beyond data
			return w
		}(),
		"rzip-truncated-header": buildRZIP(t, payload, 16)[:10],
		"rzip-lying-total": func() []byte {
			z := buildRZIP(t, payload, 16)
			binary.LittleEndian.PutUint64(z[12:20], uint64(len(payload))+7)
			return z
		}(),
		"rzip-chunk-overrun": func() []byte {
			z := buildRZIP(t, payload, 16)
			binary.LittleEndian.PutUint32(z[20:24], 1<<25)
			return z
		}(),
		"rzip-zero-chunksize": func() []byte {
			z := buildRZIP(t, payload, 16)
			binary.LittleEndian.PutUint32(z[8:12], 0)
			return z
		}(),
		"nested-rzip": buildRZIP(t, buildRZIP(t, payload, 16), 64),
	}
	for name, data := range cases {
		if _, _, err := ExtractRaw(data); err == nil {
			t.Errorf("%s: expected error, got none", name)
		}
	}
}

func TestDuplicateMemRefused(t *testing.T) {
	out := append([]byte("RASTATE"), 1)
	add := func(marker string, body []byte) {
		out = append(out, marker...)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(body)))
		out = append(out, b[:]...)
		out = append(out, body...)
		if pad := aligned(len(body)) - len(body); pad > 0 {
			out = append(out, make([]byte, pad)...)
		}
	}
	add("MEM ", payload)
	add("MEM ", []byte("second"))
	add("END ", nil)
	if _, _, err := ExtractRaw(out); err == nil {
		t.Fatal("duplicate MEM accepted")
	}
}

func TestUnknownBlockSkipped(t *testing.T) {
	out := append([]byte("RASTATE"), 1)
	add := func(marker string, body []byte) {
		out = append(out, marker...)
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(body)))
		out = append(out, b[:]...)
		out = append(out, body...)
		if pad := aligned(len(body)) - len(body); pad > 0 {
			out = append(out, make([]byte, pad)...)
		}
	}
	add("FUTR", []byte("some future block"))
	add("MEM ", payload)
	add("END ", nil)
	raw, _, err := ExtractRaw(out)
	if err != nil || !bytes.Equal(raw, payload) {
		t.Fatalf("unknown block handling: %v", err)
	}
}
