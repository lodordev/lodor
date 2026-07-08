package platform

// Save-state locations and naming per host (Handoff v1 — design §3, all paths
// source-verified 2026-07-06/07):
//
//	default (minarch: LodorOS/NextUI): {SDCARD}/.userdata/shared/{TAG}-{core}/
//	    files "{rom-filename-with-ext}.st{0-9}"   (.st9 = auto-resume)
//	muos:    /run/muos/storage/save/state/{CoreDisplayName}/
//	knulli:  /userdata/saves/{system}/            (shared with battery saves)
//	    RA naming: "{rom-stem}.state", ".state{N}", ".state.auto"
//	onion:   v1-unsupported (root ""), modes no-op honestly.
//
// The per-rom DIRECTORY COMPONENT (TAG-core / CoreDisplayName / system slug) is
// lane-specific knowledge the engine cannot derive — it ships in the lane's
// statecores.json manifest (design D7) and arrives here as `dir`. This file only
// knows the ROOT and the FILE NAMING; the per-tag vars live in states_*.go.
//
// LODOR_STATE_ROOT overrides the root for tests and harness rigs on every tag.

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// StateFile is one on-card save-state file mapped to the canonical slot space
// ("0".."8", "auto" — design D5).
type StateFile struct {
	Path string
	Slot string
}

// StateRoot returns the host's state root directory ("" = states unsupported on
// this host in v1 — callers no-op honestly).
func StateRoot() string {
	if r := os.Getenv("LODOR_STATE_ROOT"); r != "" {
		return r
	}
	return stateRootDefault()
}

// StatesUseRANaming reports the host frontend's file naming scheme.
func StatesUseRANaming() bool { return stateNamingRA }

// StateDirFor joins the host root with the lane-specific directory component
// from statecores.json. Empty when states are unsupported here.
func StateDirFor(dir string) string {
	root := StateRoot()
	if root == "" || dir == "" {
		return ""
	}
	return filepath.Join(root, dir)
}

// StateFilesForRom enumerates the rom's existing state files in dir, newest
// naming first is NOT guaranteed — callers order by mtime if they care. romBase
// is the rom's on-disk filename INCLUDING extension (minarch keys on the full
// name; RA on the stem).
func StateFilesForRom(dir, romBase string) []StateFile {
	if dir == "" || romBase == "" {
		return nil
	}
	var out []StateFile
	if stateNamingRA {
		stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
		if p := filepath.Join(dir, stem+".state"); fileExists(p) {
			out = append(out, StateFile{Path: p, Slot: "0"})
		}
		for n := 1; n <= 9; n++ {
			if p := filepath.Join(dir, stem+".state"+strconv.Itoa(n)); fileExists(p) {
				out = append(out, StateFile{Path: p, Slot: strconv.Itoa(n)})
			}
		}
		if p := filepath.Join(dir, stem+".state.auto"); fileExists(p) {
			out = append(out, StateFile{Path: p, Slot: "auto"})
		}
	} else {
		for n := 0; n <= 9; n++ {
			p := filepath.Join(dir, romBase+".st"+strconv.Itoa(n))
			if !fileExists(p) {
				continue
			}
			slot := strconv.Itoa(n)
			if n == 9 { // minarch auto-resume slot
				slot = "auto"
			}
			out = append(out, StateFile{Path: p, Slot: slot})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out
}

// StateFileForSlot returns the placement path for a canonical slot on this host.
// "" when the slot has no representation here (e.g. RA slots >9 unsupported).
func StateFileForSlot(dir, romBase, slot string) string {
	if dir == "" || romBase == "" {
		return ""
	}
	if stateNamingRA {
		stem := strings.TrimSuffix(romBase, filepath.Ext(romBase))
		switch slot {
		case "auto":
			return filepath.Join(dir, stem+".state.auto")
		case "0":
			return filepath.Join(dir, stem+".state")
		default:
			if _, err := strconv.Atoi(slot); err != nil {
				return ""
			}
			return filepath.Join(dir, stem+".state"+slot)
		}
	}
	switch slot {
	case "auto":
		return filepath.Join(dir, romBase+".st9")
	default:
		n, err := strconv.Atoi(slot)
		if err != nil || n < 0 || n > 8 {
			return ""
		}
		return filepath.Join(dir, romBase+".st"+slot)
	}
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}
