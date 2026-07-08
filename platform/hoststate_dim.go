//go:build !onion && !muos && !knulli && !android && !lodorandroid

package platform

import (
	"os"
	"strings"
)

// HostShowsStateNatively reports whether the running host's OWN launcher renders the
// cloud-vs-on-device state itself. LodorOS ships a forked minui.c that dims 0-byte cloud
// stubs by file size (minui.c: lodor_on_device = st.st_size>0), so on LodorOS the engine
// must NOT bake the ✘/✓ markers into ROM filenames — they would pollute the launcher's
// own display and, worse, leak into the derived save filename and strand uploads (the
// save resolves under a marked name the server never matches).
//
// Non-fork hosts (NextUI, muOS) keep markers — their stock launchers cannot dim — so the
// gate is an EXPLICIT POSITIVE: only a host that announces LODOR_HOST_OS=lodoros (the
// LodorOS Lodor.pak exports it) is treated as native-state. Empty or any other value
// keeps markers, preserving every non-LodorOS host's current behavior with zero risk.
// (The OnionOS and muOS builds supply their own HostShowsStateNatively -> false via
// build tag: their stock launchers can never dim, so the gate is hard-false there.)
func HostShowsStateNatively() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LODOR_HOST_OS"))) {
	case "lodoros", "lodor", "minui":
		return true
	default:
		return false
	}
}
