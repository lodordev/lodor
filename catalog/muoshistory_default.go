//go:build !muos

package catalog

// muosHistoryEnabled gates the muOS native-History injection (muoshistory.go) the
// same way gamelistEnabled gates the Knulli gamelist emitter: only muOS's launcher
// renders history from info/history pointer files, so every other build compiles
// the injector as dead code and never writes one.
const muosHistoryEnabled = false

// hostUsesContinueFile: MinUI-family hosts render Collections/"0) Continue.txt"
// as the Continue collection — keep writing it everywhere muOS isn't (#187).
const hostUsesContinueFile = true
