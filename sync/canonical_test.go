package sync

import (
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// canonEnv mirrors markerSaveEnv: temp SDCARD, gba mapped, one ROM, the launcher's
// on-disk ROM path under the given mirror mode.
func canonEnv(t *testing.T, mode string) (*config.Config, romm.Rom, string) {
	t.Helper()
	base := t.TempDir()
	t.Setenv("BASE_PATH", base)
	t.Setenv("SDCARD_PATH", base)
	t.Setenv("PLATFORM", "tg5040")
	cfg := &config.Config{
		MirrorMode:        mode,
		DirectoryMappings: map[string]config.DirMapping{"gba": {RelativePath: "GBA"}},
	}
	rom := romm.Rom{
		ID:             777,
		PlatformFsSlug: "gba",
		FsName:         "Sonic (USA).gba",
		FsNameNoExt:    "Sonic (USA)",
		Files:          []romm.RomFile{{FileName: "Sonic (USA).gba"}},
	}
	return cfg, rom, platform.LocalRomPath(cfg, rom)
}

// TestCanonicalSaveUploadName_MarkerAndDisambiguatorStripped: the exact #126 field
// shape — a minarch save named after the MARKED on-card ROM must upload under the
// clean server fs_name, in own mode (marker only) AND separate mode (marker +
// " (RomM)" disambiguator). Both ✘ (fetch-on-launch, pre-reconcile) and ✓ states.
func TestCanonicalSaveUploadName_MarkerAndDisambiguatorStripped(t *testing.T) {
	for _, mode := range []string{config.MirrorModeOwn, config.MirrorModeSeparate} {
		for _, marker := range []string{platform.MarkerCloud, platform.MarkerOnDevice, "[^] ", "[v] "} {
			cfg, rom, canonical := canonEnv(t, mode)
			// minarch: "<on-disk rom filename>.sav"
			local := marker + filepath.Base(canonical) + ".sav"
			got := canonicalSaveUploadName(cfg, rom, filepath.Join("/sd/Saves/GBA", local))
			if got != "Sonic (USA).gba.sav" {
				t.Errorf("[%s %q] = %q, want %q", mode, marker, got, "Sonic (USA).gba.sav")
			}
		}
	}
}

// TestCanonicalSaveUploadName_RetroArchStyle: a ".srm" whose stem is the
// extension-less on-disk name maps to fs_name_no_ext + ".srm".
func TestCanonicalSaveUploadName_RetroArchStyle(t *testing.T) {
	cfg, rom, canonical := canonEnv(t, config.MirrorModeSeparate)
	base := filepath.Base(canonical)                             // "Sonic (USA) (RomM).gba"
	stem := base[:len(base)-len(filepath.Ext(base))]             // "Sonic (USA) (RomM)"
	local := platform.MarkerOnDevice + stem + ".srm"             // "✓ Sonic (USA) (RomM).srm"
	got := canonicalSaveUploadName(cfg, rom, filepath.Join("/sd/Saves/GBA", local))
	if got != "Sonic (USA).srm" {
		t.Errorf("= %q, want %q", got, "Sonic (USA).srm")
	}
}

// TestCanonicalSaveUploadName_CleanNameUnchanged: a LodorOS-style clean save name
// passes through byte-identical (no regression on the fleet's main path).
func TestCanonicalSaveUploadName_CleanNameUnchanged(t *testing.T) {
	cfg, rom, _ := canonEnv(t, config.MirrorModeOwn)
	got := canonicalSaveUploadName(cfg, rom, "/sd/Saves/GBA/Sonic (USA).gba.sav")
	if got != "Sonic (USA).gba.sav" {
		t.Errorf("= %q, want unchanged", got)
	}
}

// TestCanonicalSaveUploadName_UnmatchedFallsBackToStrippedBase: a staged/foreign
// filename that matches no ROM identity still gets its marker stripped, nothing else
// (never worse than the old behavior).
func TestCanonicalSaveUploadName_UnmatchedFallsBackToStrippedBase(t *testing.T) {
	cfg, rom, _ := canonEnv(t, config.MirrorModeOwn)
	got := canonicalSaveUploadName(cfg, rom, "/tmp/staged/"+platform.MarkerCloud+"weird-snapshot.sav")
	if got != "weird-snapshot.sav" {
		t.Errorf("= %q, want %q", got, "weird-snapshot.sav")
	}
}

// TestMarkerTwinIDs: only marker-named records with a verified clean-named twin
// (same non-ghost content hash, case-insensitive) are deletable.
func TestMarkerTwinIDs(t *testing.T) {
	saves := []romm.Save{
		// clean, real — vouches for hash aa
		{ID: 1, FileName: "007 (USA).gba.sav", FileSizeBytes: 131072, ContentHash: strp("AA11")},
		// marker twin of #1 (case-differing hash) — DELETABLE
		{ID: 2, FileName: "✓ 007 (USA).gba.sav", FileSizeBytes: 131072, ContentHash: strp("aa11")},
		// marker, UNIQUE bytes — kept (real history)
		{ID: 3, FileName: "✘ Emerald.gba.sav", FileSizeBytes: 131072, ContentHash: strp("bb22")},
		// marker, hash matches only a GHOST clean record — kept
		{ID: 4, FileName: "✘ Ghosty.gba.sav", FileSizeBytes: 131072, ContentHash: strp("cc33")},
		{ID: 5, FileName: "Ghosty.gba.sav", FileSizeBytes: 0, ContentHash: strp("cc33")},
		// marker, no hash — kept (cannot prove a surviving copy)
		{ID: 6, FileName: "[v] Old.gba.sav", FileSizeBytes: 100},
	}
	got := markerTwinIDs(saves)
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("markerTwinIDs = %v, want [2]", got)
	}
}

// TestMarkerTwinIDs_NoCleanRecords: an all-marker slate deletes nothing.
func TestMarkerTwinIDs_NoCleanRecords(t *testing.T) {
	saves := []romm.Save{
		{ID: 1, FileName: "✘ A.gba.sav", FileSizeBytes: 10, ContentHash: strp("aa")},
		{ID: 2, FileName: "✓ A.gba.sav", FileSizeBytes: 10, ContentHash: strp("aa")},
	}
	if got := markerTwinIDs(saves); len(got) != 0 {
		t.Errorf("markerTwinIDs = %v, want none (no clean twin exists)", got)
	}
}
