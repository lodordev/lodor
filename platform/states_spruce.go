//go:build spruce

package platform

import "path/filepath"

// spruceOS: stock RetroArch writes savestates to savestate_directory with
// sort_savestates_enable=true (verified from the shipped retroarch.cfg), i.e.
// /mnt/SDCARD/Saves/states/<CoreName>/<rom-basename>.state*. The state root is the
// per-core sorted tree under BasePath; the launch wrapper pins the core via
// LODOR_SAVE_SUBDIR the same way it does for battery saves, and the daemon path scans
// all core folders. RA naming ON (RetroArch derives the state name from the ROM
// basename with its extension stripped, matching SaveFileName's rule).
func stateRootDefault() string { return filepath.Join(BasePath(), "Saves", "states") }

const stateNamingRA = true
