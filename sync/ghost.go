package sync

// Ghost-save detection (task #123 deliverable 3, backlog #63). A "ghost save" is
// a server-side save RECORD whose stored bytes are missing or zero-length
// (file_size_bytes <= 0) — observed historically when an upload's HTTP call
// landed but the asset write did not. Ghosts must never win a newest-wins pull
// (they would overwrite a real local save with nothing), never appear in restore
// listings as flashable versions, and never satisfy an "already on server"
// dedup check. They ARE counted, so the UI can surface "N broken saves on
// server" honestly.

import "lodor/romm"

// IsGhostSave reports whether a server save record is a ghost: the record
// exists but its stored content is missing/zero-length. The server omits
// file_size_bytes for a byte-less asset, which zero-values to 0 — both shapes
// classify as ghosts.
func IsGhostSave(s romm.Save) bool {
	return s.FileSizeBytes <= 0
}

// SplitGhosts partitions server save records into the real (non-ghost) ones,
// order preserved, and a count of ghosts. Every save-list consumer (pull,
// restore listing, feed, dedup) filters through this so a ghost can never be
// chosen as "newest" or offered for restore.
//
// META-SAVES (#146): Lodor's .lodortime/.lodorshot.png sidecar records ride the
// same transport but are NOT saves — they are dropped here silently (not
// counted as ghosts: they aren't broken, just not playable). The playtime and
// preview sync paths list them explicitly and never come through this funnel.
func SplitGhosts(saves []romm.Save) (real []romm.Save, ghosts int) {
	for _, s := range saves {
		if IsMetaSave(s) {
			continue
		}
		if IsGhostSave(s) {
			ghosts++
			continue
		}
		real = append(real, s)
	}
	return real, ghosts
}
