//go:build !onion && !muos

// The LodorOS (native-state host) leg of the ReclaimableStub triple gate. Lives in its
// own !onion && !muos file: HostShowsStateNatively is hard-false on the onion/muos
// builds (their stock launchers can never dim), so the LODOR_HOST_OS=lodoros scenario
// this test drives is unreachable there by construction.

package platform

import (
	"os"
	"path/filepath"
	"testing"
)

// TestReclaimableStubLodorOS: on a native-state host (canonical names, no markers)
// the marker leg is skipped — 0-byte + resolves is the gate.
func TestReclaimableStubLodorOS(t *testing.T) {
	base := manifestTestEnv(t)
	t.Setenv("LODOR_HOST_OS", "lodoros")
	dir := filepath.Join(base, "Roms", "GBA")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "Foo.gba")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !ReclaimableStub(p, func(string) bool { return true }) {
		t.Error("LodorOS canonical 0-byte resolving stub not reclaimable")
	}
	if err := os.WriteFile(p, []byte("REAL"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ReclaimableStub(p, func(string) bool { return true }) {
		t.Error("LodorOS real file reclaimed — never reclaim real bytes")
	}
}
