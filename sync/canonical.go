package sync

// Canonical server save names + the marker-twin heal (task #126).
//
// FIELD EVIDENCE (2026-07-02, TrimUI Smart Pro): server saves landed as
// "✘ Pokemon - Emerald Version (…).gba [ts].sav" and "✓ 007 - Everything or
// Nothing (…).gba [ts].sav". NextUI's minarch derives the save filename from the
// on-card ROM filename, which the Lodor mirror prefixes with the ✘/✓ download-state
// marker (and, in separate/merge coexist modes, suffixes with " (RomM)"). Uploading
// that basename verbatim put a DEVICE-LOCAL DISPLAY ARTIFACT into the server's data
// model: the same game produced different server save names depending on which
// device pushed and what download state it was in, splitting one game's timeline
// into per-name families (LodorOS devices push clean names).
//
// THE BOUNDARY FIX: the on-card name keeps its markers (they are the only zero-fork
// download-state display), and the ENGINE normalizes at the wire:
//
//   - UPLOAD: every push sends the canonical, server-derived filename
//     (canonicalSaveUploadName) via UploadSaveQuery.FileName — markers and the
//     coexist disambiguator never reach the server.
//   - MATCH/PULL: already name-free — pull/restore/verify all key on rom_id +
//     content_hash, so historical marker-named records still round-trip.
//   - HEAL: after a verified push, marker-named records for the same ROM whose
//     bytes verifiably survive under a clean-named record (same non-ghost
//     content_hash) are deleted (markerTwinIDs → DeleteSaves). A marker-named
//     record with UNIQUE bytes is NEVER touched — it is real history.

import (
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// canonicalSaveUploadName maps a local on-card save filename to the canonical,
// device-independent name to store on the server. It reverses the two device-local
// decorations the mirror bakes into the on-disk ROM name (and therefore into the
// emulator-derived save name):
//
//	leading state marker  "✘ " / "✓ " (and legacy "[^] "/"[v] ")  → stripped
//	coexist disambiguator "<name> (RomM)"                          → server fs_name
//
// The mapping anchors on the ROM's server identity: a save stem equal to the local
// on-disk ROM basename (full or extension-less — minarch appends ".sav" to the full
// filename, RetroArch replaces the extension) canonicalizes to rom.FsName /
// rom.FsNameNoExt respectively. A stem that matches neither (e.g. a staged snapshot
// file) falls back to the marker-stripped basename — never worse than the old
// behavior.
func canonicalSaveUploadName(cfg *config.Config, rom romm.Rom, savePath string) string {
	base := filepath.Base(savePath)
	stripped := platform.StripLeadingMarker(base)
	saveExt := filepath.Ext(stripped)
	stem := strings.TrimSuffix(stripped, saveExt)

	localBase := ""
	if p := platform.LocalRomPath(cfg, rom); p != "" {
		localBase = filepath.Base(p)
	}
	localNoExt := strings.TrimSuffix(localBase, filepath.Ext(localBase))

	switch {
	case rom.FsName != "" && (stem == rom.FsName || (localBase != "" && stem == localBase)):
		return rom.FsName + saveExt // minarch style: "<rom filename>.sav"
	case rom.FsNameNoExt != "" && (stem == rom.FsNameNoExt || (localNoExt != "" && stem == localNoExt)):
		return rom.FsNameNoExt + saveExt // RetroArch style: "<rom name>.srm"
	default:
		// Unanchored stem (e.g. a staged snapshot for a ROM whose local path can't be
		// rebuilt): still strip the coexist disambiguator textually — like the leading
		// marker it is a device-local display artifact that must never reach the
		// server's data model (shared strip set: platform.StripRomMTag, A1).
		if platform.HasRomMTag(stem) {
			return platform.StripRomMTag(stem) + saveExt
		}
		return stripped
	}
}

// markerTwinIDs returns the ids of marker-named save records that are safe to
// delete: records whose file_name carries a leading state marker AND whose
// content_hash matches a NON-GHOST record whose file_name is clean (unmarked).
// Pure over its input so the heal decision unit-tests without a server.
//
// Safety properties:
//   - a marker-named record with unique bytes is kept (no clean twin vouches for it);
//   - a ghost twin never vouches (its hash can't guarantee stored bytes);
//   - hash comparison is case-insensitive (RomM emits lowercase MD5, compared the
//     same way as AlreadyOnServer).
func markerTwinIDs(saves []romm.Save) []int {
	// hashes verifiably held under a clean (unmarked) name
	clean := map[string]bool{}
	for _, s := range saves {
		if IsGhostSave(s) || IsMetaSave(s) || platform.HasLeadingMarker(s.FileName) {
			continue // a meta record (#146) never vouches for save bytes
		}
		if s.ContentHash != nil && *s.ContentHash != "" {
			clean[strings.ToLower(*s.ContentHash)] = true
		}
	}
	var ids []int
	for _, s := range saves {
		if IsMetaSave(s) || !platform.HasLeadingMarker(s.FileName) {
			continue // meta records (#146) are never heal candidates
		}
		if s.ContentHash == nil || *s.ContentHash == "" {
			continue // no hash — cannot prove a surviving copy; keep
		}
		if clean[strings.ToLower(*s.ContentHash)] {
			ids = append(ids, s.ID)
		}
	}
	return ids
}

// healMarkerTwins deletes marker-named duplicate save records for romID whose bytes
// verifiably survive under a clean-named record (see markerTwinIDs). Best-effort and
// silent: it runs opportunistically after a verified push and must never affect the
// push outcome. Returns how many records were deleted (0 on any error).
func healMarkerTwins(client *romm.Client, romID int) int {
	saves, err := client.GetSaves(romm.SaveQuery{RomID: romID})
	if err != nil {
		return 0
	}
	ids := markerTwinIDs(saves)
	if len(ids) == 0 {
		return 0
	}
	if err := client.DeleteSaves(ids); err != nil {
		return 0
	}
	return len(ids)
}
