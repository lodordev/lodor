//go:build !onion && !muos && !knulli && !android && !lodorandroid

package catalog

// Disc-1-first partial-state tolerance (lodor#7): under the hybrid design a
// downloaded multi-disc game can legitimately sit on the card as a REAL .m3u whose
// later discs are 0-byte stubs. These tests lock the two correctness touch-points
// the issue names:
//
//   1. MIRROR: a catalog refresh must treat the partial game as DOWNLOADED
//      (existing, kind=download) — never re-stub the .m3u, never touch the disc
//      folder's real bytes or its stubs.
//   2. RECONCILE: the post-launch ✘→✓ marker flip must promote a real .m3u even
//      when its disc set is incomplete (the m3u has bytes; stub discs are the
//      designed state, not corruption).

import (
	"os"
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// mdCatalogRom is the fixture server-side multi-disc game.
func mdCatalogRom() romm.Rom {
	return romm.Rom{
		ID:               77,
		PlatformFsSlug:   "psx",
		FsName:           "Chrono Cross (USA)",
		FsNameNoExt:      "Chrono Cross (USA)",
		Name:             "Chrono Cross",
		HasMultipleFiles: true,
		Files: []romm.RomFile{
			{ID: 771, FileName: "Chrono Cross (USA) (Disc 1).chd"},
			{ID: 772, FileName: "Chrono Cross (USA) (Disc 2).chd"},
		},
	}
}

// TestMirrorToleratesPartialMultiDisc: the card holds the exact disc-1-first shape
// (marked real .m3u + disc 1 real + disc 2 stub, manifest-owned), the server still
// lists the game — a mirror refresh must leave every byte of it alone and count it
// as an existing download, not a broken/missing game to re-stub.
func TestMirrorToleratesPartialMultiDisc(t *testing.T) {
	if platform.HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): marked-name expectations do not apply")
	}
	base := mergeTestEnv(t)
	mkdirAll(t, base, ".system/tg5040/paks/Emus/PS.pak") // psx launchable on this device

	rom := mdCatalogRom()
	cfg := &config.Config{
		MirrorMode: config.MirrorModeMerge,
		DirectoryMappings: map[string]config.DirMapping{
			"psx": {Slug: "psx", RelativePath: "PlayStation (PS)"},
		},
	}

	// The on-card disc-1-first state a launch left behind (post-reconcile: ✓-marked m3u).
	m3uBody := "Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd\n" +
		"Chrono Cross (USA)/Chrono Cross (USA) (Disc 2).chd\n"
	marked := writeCard(t, base, "Roms/PlayStation (PS)/"+platform.MarkerOnDevice+"Chrono Cross (USA).m3u", []byte(m3uBody))
	disc1 := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd", []byte("DISC1"))
	disc2 := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 2).chd", nil) // 0-byte stub
	discDir := filepath.Dir(disc1)

	man := platform.LoadManifest()
	man.Record(marked, platform.ManifestDownload, rom.ID)
	man.Record(discDir, platform.ManifestFolder, rom.ID)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	fake := &mergeFake{
		platforms: []romm.Platform{{ID: 9, FsSlug: "psx", Name: "PlayStation", RomCount: 1}},
		roms:      map[int][]romm.Rom{9: {rom}},
	}
	created, existing, _, multifile, _, _, err := MirrorCatalog(fake, cfg, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 || existing != 1 || multifile != 1 {
		t.Errorf("created=%d existing=%d multifile=%d, want 0/1/1 (partial multi-disc is an EXISTING download)", created, existing, multifile)
	}

	// Byte-identical card: the real m3u kept its content (not re-stubbed), disc 1
	// kept its bytes, the stub is still a 0-byte file, nothing extra appeared.
	if got, rerr := os.ReadFile(marked); rerr != nil || string(got) != m3uBody {
		t.Errorf("marked m3u changed by the mirror: err=%v content=%q", rerr, string(got))
	}
	if got, rerr := os.ReadFile(disc1); rerr != nil || string(got) != "DISC1" {
		t.Errorf("disc 1 changed by the mirror: err=%v", rerr)
	}
	if fi, serr := os.Stat(disc2); serr != nil || fi.Size() != 0 {
		t.Errorf("disc 2 stub changed by the mirror: err=%v", serr)
	}
	if _, serr := os.Stat(filepath.Join(filepath.Dir(marked), platform.MarkerCloud+"Chrono Cross (USA).m3u")); serr == nil {
		t.Errorf("mirror created a duplicate ✘ stub beside the downloaded partial game")
	}
	// Manifest still records the marked m3u as a DOWNLOAD (evict/uninstall keep working).
	man = platform.LoadManifest()
	if !man.OwnsKind(marked, platform.ManifestDownload) {
		t.Errorf("manifest no longer records the partial game's m3u as kind=download")
	}
}

// TestMirrorNeverRestoresFullListM3U (local-only .m3u regression guard): the card
// holds the NEW shape — a real .m3u listing ONLY the present disc, the full
// canonical list on the manifest record, a 0-byte stub disc in the folder. A
// mirror refresh must leave the playlist byte-identical (the shipped launcher
// refuses to launch past a listed stub, so "restoring" the full list would
// reintroduce the never-launches regression) and must keep the manifest's
// canonical disc list intact.
func TestMirrorNeverRestoresFullListM3U(t *testing.T) {
	if platform.HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): marked-name expectations do not apply")
	}
	base := mergeTestEnv(t)
	mkdirAll(t, base, ".system/tg5040/paks/Emus/PS.pak")

	rom := mdCatalogRom()
	cfg := &config.Config{
		MirrorMode: config.MirrorModeMerge,
		DirectoryMappings: map[string]config.DirMapping{
			"psx": {Slug: "psx", RelativePath: "PlayStation (PS)"},
		},
	}

	// LOCAL-ONLY playlist: disc 1 alone; disc 2 exists only as a folder stub.
	localBody := "Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd\n"
	canon := []string{
		"Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd",
		"Chrono Cross (USA)/Chrono Cross (USA) (Disc 2).chd",
	}
	marked := writeCard(t, base, "Roms/PlayStation (PS)/"+platform.MarkerOnDevice+"Chrono Cross (USA).m3u", []byte(localBody))
	disc1 := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd", []byte("DISC1"))
	writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 2).chd", nil) // unlisted stub

	man := platform.LoadManifest()
	man.Record(marked, platform.ManifestDownload, rom.ID)
	man.Record(filepath.Dir(disc1), platform.ManifestFolder, rom.ID)
	man.SetDiscs(marked, canon)
	if err := man.Save(); err != nil {
		t.Fatal(err)
	}

	fake := &mergeFake{
		platforms: []romm.Platform{{ID: 9, FsSlug: "psx", Name: "PlayStation", RomCount: 1}},
		roms:      map[int][]romm.Rom{9: {rom}},
	}
	if _, _, _, _, _, _, err := MirrorCatalog(fake, cfg, nil, false); err != nil {
		t.Fatal(err)
	}

	if got, rerr := os.ReadFile(marked); rerr != nil || string(got) != localBody {
		t.Errorf("mirror rewrote the local-only m3u: err=%v content=%q (must stay disc-1-only)", rerr, string(got))
	}
	man = platform.LoadManifest()
	e, ok := man.Entry(marked)
	if !ok || e.Kind != platform.ManifestDownload {
		t.Fatalf("manifest entry lost/rekinded by mirror: %+v ok=%v", e, ok)
	}
	if len(e.Discs) != 2 || e.Discs[0] != canon[0] || e.Discs[1] != canon[1] {
		t.Errorf("canonical disc list disturbed by mirror: %v", e.Discs)
	}
	// The offline census still sees the honest 1-of-2 state through the manifest.
	if total, present := RomDiscCompleteness(man, marked); total != 2 || present != 1 {
		t.Errorf("census after mirror = %d/%d, want 1/2", present, total)
	}
}

// TestReconcileToleratesStubDiscs: the post-launch ✘→✓ flip must promote a real
// (populated) .m3u whose later discs are still 0-byte stubs — the m3u itself has
// bytes, so the game is "on device" the moment disc 1 landed.
func TestReconcileToleratesStubDiscs(t *testing.T) {
	if platform.HostShowsStateNatively() {
		t.Skip("marker-less host (hard-true build tag): there is no ✘→✓ flip; reconcile is a no-op by design")
	}
	base := mergeTestEnv(t)
	cfg := &config.Config{
		MirrorMode: config.MirrorModeMerge,
		DirectoryMappings: map[string]config.DirMapping{
			"psx": {Slug: "psx", RelativePath: "PlayStation (PS)"},
		},
	}
	m3uBody := "Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd\n" +
		"Chrono Cross (USA)/Chrono Cross (USA) (Disc 2).chd\n"
	// Fetch-on-launch filled the ✘ stub in place (disc-1-first): real m3u + stub disc 2.
	cloud := writeCard(t, base, "Roms/PlayStation (PS)/"+platform.MarkerCloud+"Chrono Cross (USA).m3u", []byte(m3uBody))
	writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 1).chd", []byte("DISC1"))
	stub := writeCard(t, base, "Roms/PlayStation (PS)/Chrono Cross (USA)/Chrono Cross (USA) (Disc 2).chd", nil)

	if flipped := ReconcileAfterDownload(cfg, cloud); !flipped {
		t.Fatalf("reconcile refused to promote a real m3u with stub discs (treated partial as broken)")
	}
	dev := filepath.Join(filepath.Dir(cloud), platform.MarkerOnDevice+"Chrono Cross (USA).m3u")
	if got, rerr := os.ReadFile(dev); rerr != nil || string(got) != m3uBody {
		t.Errorf("promoted ✓ m3u missing/changed: err=%v", rerr)
	}
	if fi, serr := os.Stat(stub); serr != nil || fi.Size() != 0 {
		t.Errorf("stub disc disturbed by reconcile: err=%v", serr)
	}
}
