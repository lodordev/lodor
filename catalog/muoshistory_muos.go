//go:build muos

package catalog

// muosHistoryEnabled: this build's host IS muOS — cross-device Continue entries are
// delivered into the launcher's own History menu (muoshistory.go, task #181).
const muosHistoryEnabled = true
