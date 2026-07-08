package sync

// D8 certification whitelist: it only widens, never narrows; fail-closed when
// absent/malformed; encodes the real cross-bitness cert facts (gambatte crosses
// armhf↔arm64; snes9x2005_plus is arm64-only — the Android/within-group bridge).

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCompat(t *testing.T, json string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", dir)
	if json != "" {
		if err := os.WriteFile(filepath.Join(dir, "state-compat.json"), []byte(json), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	gbArmhf   = "lodor/lodoros/gambatte@9d923816/armhf"
	gbArm64   = "lodor/knulli/gambatte@9d923816/arm64"
	snArmhf   = "lodor/lodoros/snes9x2005_plus@abc/armhf"
	snArm64K  = "lodor/knulli/snes9x2005_plus@def/arm64"
	snArm64A  = "lodor/android/snes9x2005_plus@unknown/arm64"
	gpArm64K  = "lodor/knulli/gpsp@111/arm64"
	gpArm64A  = "lodor/android/gpsp@unknown/arm64"
)

func TestCompatFailClosedNoFile(t *testing.T) {
	writeCompat(t, "")
	// no whitelist → base Tier-0 only. Same core+ver+arch = compatible.
	if ok, _ := tuplesCompatible(gbArm64, "lodor/nextui/gambatte@9d923816/arm64"); !ok {
		t.Fatal("Tier-0 same core@ver/arch must be compatible")
	}
	// cross-arch same core = NOT compatible without a class.
	if ok, why := tuplesCompatible(gbArmhf, gbArm64); ok {
		t.Fatalf("cross-arch must be incompatible with no whitelist (why=%q)", why)
	}
}

func TestCompatFailClosedMalformed(t *testing.T) {
	writeCompat(t, "{ this is not json")
	if ok, _ := tuplesCompatible(gbArmhf, gbArm64); ok {
		t.Fatal("malformed whitelist must fall back to Tier-0 (incompatible cross-arch)")
	}
}

func TestCompatCertCashIn(t *testing.T) {
	// tonight's cert: gambatte portable armhf↔arm64.
	writeCompat(t, `{"version":1,"classes":[{"core":"gambatte","arches":["armhf","arm64"]}]}`)
	if ok, why := tuplesCompatible(gbArmhf, gbArm64); !ok {
		t.Fatalf("certified gambatte armhf↔arm64 must be compatible (why=%q)", why)
	}
	// reverse direction identical.
	if ok, _ := tuplesCompatible(gbArm64, gbArmhf); !ok {
		t.Fatal("compat must be symmetric")
	}
	// a DIFFERENT core not in the whitelist stays incompatible cross-arch.
	if ok, _ := tuplesCompatible(snArmhf, snArm64K); ok {
		t.Fatal("snes not in whitelist → must stay incompatible cross-arch")
	}
}

func TestCompatWithinArm64Bridge(t *testing.T) {
	// snes9x2005_plus is arm64-ONLY per cert — the within-group / Android
	// (@unknown) version bridge, but must NOT cross to armhf.
	writeCompat(t, `{"version":1,"classes":[{"core":"snes9x2005_plus","arches":["arm64"]}]}`)
	// Android @unknown ↔ Knulli @def, both arm64 → certified compatible.
	if ok, why := tuplesCompatible(snArm64A, snArm64K); !ok {
		t.Fatalf("arm64 snes bridge (unknown↔pinned) must be compatible (why=%q)", why)
	}
	// armhf ↔ arm64 snes → armhf NOT in the arm64-only class → incompatible.
	if ok, _ := tuplesCompatible(snArmhf, snArm64K); ok {
		t.Fatal("snes armhf↔arm64 must stay incompatible (cert FAILED cross-arch)")
	}
}

func TestCompatMultipleClasses(t *testing.T) {
	writeCompat(t, `{"version":1,"classes":[
		{"core":"gambatte","arches":["armhf","arm64"]},
		{"core":"gpsp","arches":["arm64"]}
	]}`)
	// gpsp arm64 bridge (Android↔Knulli) works; gambatte crosses arch.
	if ok, _ := tuplesCompatible(gpArm64A, gpArm64K); !ok {
		t.Fatal("gpsp arm64 bridge must be compatible")
	}
	if ok, _ := tuplesCompatible(gbArmhf, gbArm64); !ok {
		t.Fatal("gambatte cross-arch must be compatible")
	}
	// gpsp does NOT cross to armhf (only arm64 in its class) — GBA within-group.
	if ok, _ := tuplesCompatible("lodor/lodoros/gpsp@111/armhf", gpArm64K); ok {
		t.Fatal("gpsp armhf↔arm64 must stay incompatible (cert FAILED)")
	}
}

func TestCompatNeverMakesForeignCompatible(t *testing.T) {
	// a whitelist can't rescue a tuple-less/foreign record — that gate is in
	// ListStates/PullState (prefix "lodor/"), upstream of tuplesCompatible.
	writeCompat(t, `{"version":1,"classes":[{"core":"gambatte","arches":["armhf","arm64"]}]}`)
	if ok, _ := tuplesCompatible("builtin", gbArm64); ok {
		t.Fatal("unparseable/foreign tuple must never be compatible")
	}
}
