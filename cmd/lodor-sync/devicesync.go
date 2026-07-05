package main

// Native-menu modes for the device_save_sync explicit user control (task #176):
// --track-save / --untrack-save. These are the "resume / stop syncing this save on
// this device" toggle the per-game menu will eventually surface (menu wire-up is a
// follow-up; this delivers the engine capability + honest output now).
//
// Contract (RESULT-printing modes, so exitMode applies the PAIRING_EXPIRED tail):
//
//	--track-save   <romPath>  → RESULT tracked=<0|1> reason=<token>
//	--untrack-save <romPath>  → RESULT untracked=<0|1> reason=<token>
//
// reason tokens: ok | unsupported (RomM < 4.9.0) | nodevice (device not registered) |
// resolve (unreachable/auth while resolving) | nosave (no eligible server save) |
// notracked (track only: no existing sync row to resume) | autherr | error.
//
// Both are NON-BLOCKING and touch no save bytes — a failure is reported, never fatal.

import (
	"fmt"

	"lodor/config"
	"lodor/romm"
	"lodor/sync"
)

// runTrackSave resumes device-side sync for one ROM's newest server save.
func runTrackSave(client *romm.Client, cfg *config.Config, romPath string) {
	res := sync.TrackSaveForRom(client, cfg, romPath)
	if res.AuthExpired {
		pairingExpired = true
	}
	fmt.Printf("RESULT tracked=%s reason=%s\n", boolFlag(res.OK), res.Reason)
	exitMode(0)
}

// runUntrackSave stops device-side sync for one ROM's newest server save.
func runUntrackSave(client *romm.Client, cfg *config.Config, romPath string) {
	res := sync.UntrackSaveForRom(client, cfg, romPath)
	if res.AuthExpired {
		pairingExpired = true
	}
	fmt.Printf("RESULT untracked=%s reason=%s\n", boolFlag(res.OK), res.Reason)
	exitMode(0)
}

// boolFlag renders a bool as the 0/1 the launcher parses.
func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
