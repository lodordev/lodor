package sync

// Meta-save PUSH (tasks #146/#149): upload a Lodor sidecar payload (.lodortime
// playtime record / .lodorshot.png preview) for one ROM over the EXISTING RomM
// saves transport. Everything here is best-effort by contract: a meta push
// failure must never affect a save push outcome, a launch, or an exit path.
//
// Isolation from real saves:
//   - the record's FileName is the canonical "<fs_name><metaExt>" so every
//     consumer's IsMetaSave check (shipped FIRST, #146) filters it;
//   - a DEDICATED slot per meta kind ("lodortime"/"lodorshot") keeps RomM's
//     autocleanup rotation from ever competing with the real "autosave" slot's
//     25-version history;
//   - a small autocleanup limit (3) because only the newest record matters.

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// metaAutocleanupLimit caps stored versions per meta slot — only the newest
// record is ever consumed; a shallow history is kept for post-mortems.
const metaAutocleanupLimit = 3

// MetaSlotFor returns the dedicated upload slot for a meta filename
// (".lodortime" -> "lodortime", ".lodorshot.png" -> "lodorshot").
func MetaSlotFor(fileName string) string {
	n := strings.ToLower(fileName)
	switch {
	case strings.HasSuffix(n, ".lodortime"):
		return "lodortime"
	case strings.HasSuffix(n, ".lodorshot.png"):
		return "lodorshot"
	default:
		return "lodormeta"
	}
}

// PushMetaSave uploads payload as the meta-save fileName for rom. Dedup-first:
// when the server already holds a meta record with this exact name and these
// exact bytes (content_hash match), nothing is uploaded — repeated session
// pushes with unchanged playtime cost one list call. Returns nil on "already
// there" and on a landed upload; any error is the caller's to LOG AND DROP
// (best-effort contract).
func PushMetaSave(client *romm.Client, cfg *config.Config, rom romm.Rom, fileName string, payload []byte) error {
	if len(payload) == 0 {
		return syncErr("empty meta payload")
	}
	sum := md5.Sum(payload)
	payloadMD5 := hex.EncodeToString(sum[:])

	// Dedup: same name + same bytes already stored (only meta records count —
	// a real save's hash can never dedup a meta push, and vice versa).
	if saves, err := client.GetSaves(romm.SaveQuery{RomID: rom.ID}); err == nil {
		for _, s := range saves {
			if !IsMetaSave(s) || !strings.EqualFold(s.FileName, fileName) {
				continue
			}
			if s.ContentHash != nil && strings.EqualFold(*s.ContentHash, payloadMD5) && s.FileSizeBytes > 0 {
				return nil // identical record already on the server
			}
		}
	}

	// The upload API reads a file path; stage the payload in /tmp (tmpfs).
	tmp, err := os.CreateTemp("", "lodor-meta-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err = tmp.Write(payload); err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	q := romm.UploadSaveQuery{
		RomID:            rom.ID,
		DeviceID:         deviceID(cfg),
		Emulator:         "lodor",
		Slot:             MetaSlotFor(fileName),
		Autocleanup:      true,
		AutocleanupLimit: metaAutocleanupLimit,
		FileName:         fileName,
	}
	uploaded, _, err := uploadWithConflictRetry(client, &q, tmpPath)
	if err != nil {
		return err
	}
	// Light verify (tier 1 only — meta is best-effort): when the create response
	// carries a hash it must match; a hashless response is accepted.
	if uploaded.ContentHash != nil && *uploaded.ContentHash != "" &&
		!strings.EqualFold(*uploaded.ContentHash, payloadMD5) {
		return errVerifyMismatch
	}
	return nil
}

// MetaFileName builds the canonical meta-save name for a ROM: the server-side
// fs_name (device-independent, marker-free) plus the meta extension. Falls back
// to the marker-stripped local basename when fs_name is unknown.
func MetaFileName(rom romm.Rom, localRomPath, metaExt string) string {
	base := rom.FsName
	if base == "" {
		base = platform.StripLeadingMarker(filepath.Base(localRomPath))
	}
	return base + metaExt
}
