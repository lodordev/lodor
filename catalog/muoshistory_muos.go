//go:build muos

package catalog

// muosHistoryEnabled: this build's host IS muOS — cross-device Continue entries are
// delivered into the launcher's own History menu (muoshistory.go, task #181).
const muosHistoryEnabled = true

// hostUsesContinueFile: muOS renders no MinUI-style Collections browser — the
// Continue list rides the native History injection instead, and a written
// "0) Continue.txt" is a stray file in the user's ROMS tree (#187).
const hostUsesContinueFile = false
