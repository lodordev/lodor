package sync

import (
	"os"
	"path/filepath"
	"strings"

	"lodor/catalog"
	"lodor/config"
	"lodor/platform"
	"lodor/romm"
)

// syncSlot is the stable slot every Lodor autosave pairs under. It MUST match the slot
// the push path uploads with (uploadVerified sets Slot: "autosave") and must be
// non-empty: RomM 5.0.0 treats a null slot as archival and always negotiates it as an
// upload, defeating reconcile (see romm/sync.go slot-semantics note).
const syncSlot = "autosave"

// BuildLocalSaveInventory walks the device's whole save tree ONCE and returns one
// ClientSaveState per local save file that maps to a known ROM — the client-side input
// to POST /api/sync/negotiate. It performs NO per-ROM API calls: the rom_id for each
// save is resolved locally from the catalog index (id -> on-card ROM path), reversed
// into a stem -> id lookup that mirrors how findLocalSavesForRom matches saves to ROMs
// (a save's marker-stripped, extension-stripped stem equals either the ROM basename —
// minarch ".sav" appended to the full name — or the ROM basename without extension —
// RetroArch ".srm" replacing it).
//
// A save file whose stem maps to no catalog ROM is skipped (it belongs to a ROM not in
// this device's mirror; the server can't reconcile what we can't identify locally). An
// unreadable/zero-length file is skipped too — a hash it can't produce would only
// mislead the diff.
func BuildLocalSaveInventory(cfg *config.Config) []romm.ClientSaveState {
	stemToID := buildStemIndex(cfg)
	if len(stemToID) == 0 {
		return nil
	}

	savesRoot := platform.SavesDir()
	emuDirs, err := os.ReadDir(savesRoot)
	if err != nil {
		return nil
	}

	var inv []romm.ClientSaveState
	for _, ed := range emuDirs {
		if !ed.IsDir() || strings.HasPrefix(ed.Name(), ".") {
			continue
		}
		emuDir := ed.Name()
		dir := filepath.Join(savesRoot, emuDir)
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			ext := strings.ToLower(filepath.Ext(f.Name()))
			if !ValidSaveExtensions[ext] {
				continue
			}
			stem := platform.StripLeadingMarker(strings.TrimSuffix(f.Name(), filepath.Ext(f.Name())))
			romID, ok := stemToID[stem]
			if !ok {
				continue
			}
			path := filepath.Join(dir, f.Name())
			hash, hok := fileMD5(path)
			if !hok {
				continue // unreadable/zero-length — a phantom hash would mislead the diff
			}
			fi, statErr := os.Stat(path)
			if statErr != nil {
				continue
			}
			inv = append(inv, romm.ClientSaveState{
				RomID:         romID,
				FileName:      f.Name(),
				Slot:          syncSlot,
				Emulator:      emuDir,
				ContentHash:   hash,
				UpdatedAt:     fi.ModTime().UTC(),
				FileSizeBytes: fi.Size(),
			})
		}
	}
	return inv
}

// buildStemIndex reverses the catalog id->path index into a stem->id lookup keyed by
// both the ROM basename and the basename without its extension, so a save discovered by
// either the minarch (".gba.sav") or RetroArch (".srm") naming convention resolves to
// its rom_id. On a name collision the first id wins; collisions across distinct ROMs
// sharing a basename are vanishingly rare and the launch/push paths already treat such
// twins as one save family.
func buildStemIndex(cfg *config.Config) map[string]int {
	idPath := catalog.LoadIndexIDPath(cfg)
	if len(idPath) == 0 {
		return nil
	}
	stem := make(map[string]int, len(idPath)*2)
	for id, path := range idPath {
		base := filepath.Base(path)
		if base == "" || base == "." {
			continue
		}
		if _, seen := stem[base]; !seen {
			stem[base] = id
		}
		noExt := strings.TrimSuffix(base, filepath.Ext(base))
		if noExt != base {
			if _, seen := stem[noExt]; !seen {
				stem[noExt] = id
			}
		}
	}
	return stem
}
