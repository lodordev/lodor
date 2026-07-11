//go:build !onion && !muos && !knulli && !android && !lodorandroid

package main

// Real download cancel through the multi-disc core (lodor#7 follow-up). The
// launcher's B-press touches the covercancel sentinel; armDownloadCancel points
// the client's CancelCheck at it for the INTERACTIVE modes. These tests drive the
// check directly (the sentinel plumbing is covered in covercancel's own tests):
//
//   1. BETWEEN DISCS: a cancel arriving after disc 1 verified stops the run before
//      disc 2's transfer starts — disc 1 stays, is LISTED in the local-only .m3u
//      (playable now), st.cancelled=true, disc 2 never hits the server.
//   2. MID-TRANSFER: a cancel during disc 1's stream keeps the partial .tmp for
//      the Range resume (never deleted, never renamed in), reports cancelled.

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/platform"
)

func TestMultiDiscCancelBetweenDiscs(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	_, discDir, m3u := mdPaths(base)
	disc1 := filepath.Join(discDir, rom.Files[0].FileName)

	// The "B pressed while disc 1 was finishing" shape: false while disc 1 is in
	// flight (only its .tmp exists), true the moment it verified + renamed in.
	client.CancelCheck = func() bool {
		fi, serr := os.Stat(disc1)
		return serr == nil && fi.Size() > 0
	}

	man := platform.LoadManifest()
	var st discStats
	ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, -1, &st)
	if ok {
		t.Fatalf("cancelled fetch-all reported ok")
	}
	if !st.cancelled {
		t.Fatalf("st.cancelled not set on a between-disc cancel: %+v", st)
	}
	if st.fetched != 1 || st.present != 1 {
		t.Errorf("stats = %+v, want fetched:1 present:1 (disc 1 landed, then stop)", st)
	}
	assertDisc(t, discDir, rom, 0, "real")
	// Disc 1 is listed — the cancel left a PLAYABLE local-only playlist.
	if data, rerr := os.ReadFile(m3u); rerr != nil || string(data) != canonDiscLines[0]+"\n" {
		t.Errorf("m3u after cancel = %q (err %v), want disc 1 alone", string(data), rerr)
	}
	// Disc 2's transfer never started.
	if got := ms.hitCount("9002"); got != 0 {
		t.Errorf("disc 2 hit the server %d times after the cancel, want 0", got)
	}
	assertCanonDiscs(t, m3u) // canonical list recorded despite the cancel
}

func TestMultiDiscCancelMidTransferKeepsPartial(t *testing.T) {
	ms := multiDiscServer(t)
	defer ms.srv.Close()
	cfg, client, base := newMultiDiscEnv(t, ms.srv)

	rom, err := client.GetRom(900)
	if err != nil {
		t.Fatalf("GetRom: %v", err)
	}
	_, discDir, _ := mdPaths(base)

	// Call 1 is the between-disc gate (still armed = false); call 2 is the copy
	// loop's pre-chunk check inside disc 1's transfer — the B-press lands there.
	polls := 0
	client.CancelCheck = func() bool {
		polls++
		return polls > 1
	}

	man := platform.LoadManifest()
	var st discStats
	ok := downloadMultiDiscCore(client, cfg, rom, rom.Name, man, 1, &st)
	if ok || !st.cancelled {
		t.Fatalf("mid-transfer cancel: ok=%v st=%+v, want ok=false cancelled=true", ok, st)
	}
	if st.fetched != 0 {
		t.Errorf("fetched = %d, want 0 (the transfer was cut)", st.fetched)
	}
	// The partial .tmp STAYS for the Range resume; the disc was never renamed in.
	tmp := filepath.Join(discDir, rom.Files[0].FileName+".tmp")
	if _, serr := os.Stat(tmp); serr != nil {
		t.Errorf("partial .tmp missing after cancel (must be kept for resume): %v", serr)
	}
	assertDisc(t, discDir, rom, 0, "absent")
}
