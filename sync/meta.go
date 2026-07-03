package sync

// Meta-saves (task #146/#149). LodorOS rides the EXISTING RomM saves transport
// for two small cross-device sidecar records:
//
//	<rom>.lodortime     — compact per-ROM playtime record (task #146)
//	<rom>.lodorshot.png — last autosave preview screenshot (task #149)
//
// They are DATA ABOUT play, not playable save bytes — so they must be invisible
// to every consumer that treats a server save record as a game save: the
// newest-wins pull (a .lodortime must never overwrite an .sav), canonical-name
// healing, the sync feed, the recents/Continue feed, restore listings, and the
// content-hash dedup/verify checks (their bytes can never vouch for a save).
// This filter ships BEFORE any meta-save is ever pushed (frozen contract,
// lodoros-094-plan.md) so no engine in the field can mis-treat one.
//
// The playtime and preview sync paths select meta-saves EXPLICITLY (they are
// the only intended consumers) — see --sync-playtime and the preview pull.

import (
	"strings"

	"lodor/romm"
)

// metaSaveSuffixes are the meta-save filename shapes, matched case-insensitively
// against the FULL server file_name (extension fields alone are ambiguous:
// ".lodorshot.png" parses as extension "png").
var metaSaveSuffixes = []string{".lodortime", ".lodorshot.png"}

// IsMetaSaveName reports whether a save FILENAME names a Lodor meta-save.
func IsMetaSaveName(name string) bool {
	n := strings.ToLower(name)
	for _, suf := range metaSaveSuffixes {
		if strings.HasSuffix(n, suf) {
			return true
		}
	}
	return false
}

// IsMetaSave reports whether a server save record is a Lodor meta-save
// (playtime / preview sidecar riding the saves transport) — excluded from every
// real-save consumer per the 0.9.4 frozen contract.
func IsMetaSave(s romm.Save) bool {
	return IsMetaSaveName(s.FileName)
}
