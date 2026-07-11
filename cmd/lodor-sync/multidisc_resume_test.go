//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// Per-disc byte-level RESUME (parity with the single-file path): a disc transfer
// cut mid-stream must KEEP its partial .tmp, and the next run must complete that
// disc from the partial's end via an HTTP Range request — never re-paying the
// already-landed bytes. Driven end-to-end through downloadMultiDiscCore against
// the fake RomM: the cut is a real aborted response (promised length, half the
// body, connection dropped), the resume is asserted on the wire (the Range header
// the engine sent) and on disk (byte-exact final disc).

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"lodor/platform"
)

func TestDownloadMultiDiscCore_PerDiscByteResume(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	_, discDir, _ := mdPaths(base)
	half := len(discBytes(1)) / 2
	tmp := filepath.Join(discDir, rom.Files[0].FileName+".tmp")

	// Run 1 — disc 1 cut mid-stream: the run fails, and the partial .tmp STAYS
	// (pre-fix it was deleted, restarting the disc at byte 0 every retry).
	ms.setCut("9001", true)
	man := platform.LoadManifest()
	var st discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st); ok {
		t.Fatal("cut transfer reported success")
	}
	fi, serr := os.Stat(tmp)
	if serr != nil {
		t.Fatalf("partial .tmp not kept after the cut: %v", serr)
	}
	if fi.Size() != int64(half) {
		t.Fatalf("partial size = %d, want %d (half the disc)", fi.Size(), half)
	}

	// Run 2 — server healthy: the disc completes byte-exact, and the wire shows a
	// resume (Range: bytes=<half>-) rather than a from-zero re-fetch.
	ms.setCut("9001", false)
	man = platform.LoadManifest()
	var st2 discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st2); !ok {
		t.Fatal("recovery run failed")
	}
	for i := range rom.Files {
		assertDisc(t, discDir, rom, i, "real")
	}
	rngs := ms.rangeHeaders("9001")
	if len(rngs) != 2 {
		t.Fatalf("disc 1 content requests = %d, want 2 (cut + resume)", len(rngs))
	}
	if rngs[0] != "" {
		t.Errorf("first request carried Range %q, want none (fresh download)", rngs[0])
	}
	if want := fmt.Sprintf("bytes=%d-", half); rngs[1] != want {
		t.Errorf("resume Range = %q, want %q", rngs[1], want)
	}
	if _, serr := os.Stat(tmp); !os.IsNotExist(serr) {
		t.Errorf("completed disc left its .tmp behind")
	}
}

// TestDownloadMultiDiscCore_ResumeSelfHealsStalePartial: correctness must never
// depend on the partial being sane or the server honoring Range. A STALE partial
// LONGER than the real disc drives the 416 self-heal leg: the server rejects the
// past-EOF offset, the resume transport truncates to zero and re-fetches the whole
// disc clean — byte-exact, never a stitched corruption. (The Range-ignoring-200
// server leg is covered by the transport's own contract: it rewrites from byte 0
// on any 200.)
func TestDownloadMultiDiscCore_ResumeSelfHealsStalePartial(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	_, discDir, _ := mdPaths(base)

	// Pre-seed a STALE partial that is LONGER than the real disc: the fake answers
	// 416 (offset at/past EOF) — the self-heal path — and the transport re-fetches
	// clean from byte 0. Final bytes must be exact, never a stitched corruption.
	if err := os.MkdirAll(discDir, 0o755); err != nil {
		t.Fatal(err)
	}
	man := platform.LoadManifest()
	man.Record(discDir, platform.ManifestFolder, rom.ID)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}
	stale := append(discBytes(1), []byte("STALE OVERHANG")...)
	tmp := filepath.Join(discDir, rom.Files[0].FileName+".tmp")
	if err := os.WriteFile(tmp, stale, 0o644); err != nil {
		t.Fatal(err)
	}

	man = platform.LoadManifest()
	var st discStats
	if ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st); !ok {
		t.Fatal("download with a stale oversized partial failed")
	}
	for i := range rom.Files {
		assertDisc(t, discDir, rom, i, "real")
	}
}
