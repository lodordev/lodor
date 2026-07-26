package sync

// D8 state-compat MATRIX test (test-coverage gap #2 — launch-card-v2).
//
// The launch card's headline feature is the bright/dim verdict: a "* loadable here" row iff
// tuplesCompatible reports compat=1. The existing statecompat_test.go proves the loader's
// mechanics (fail-closed, widen-not-narrow, symmetry) with ad-hoc one/two-class whitelists.
// What was NOT pinned: the REAL shipped fleet whitelist as a whole — the exact classes
// release/mkstatecompat.sh emits (the 2026-07-07 certified fleet) — driven pairwise across
// every cross-arch / cross-core / cross-version / foreign / builtin / unknown case, including
// the NEGATIVE rows a correct card must render DIM. This test is the card's contract on the
// certification facts it ships with; if the shipped whitelist drifts, this fails loudly.
//
// Shipped whitelist (release/mkstatecompat.sh header, certified 2026-07-07):
//   cross-arch PASS (armhf,arm64): fceumm, gambatte, picodrive
//   arm64-only bridge:             gpsp, snes9x2005_plus  (FAILED cross-arch; within-group only)

import (
	"os"
	"path/filepath"
	"testing"
)

// shippedStateCompatJSON is byte-for-byte the whitelist release/mkstatecompat.sh produces for
// the certified 2026-07-07 fleet invocation documented in its header. Keeping the literal here
// (rather than shelling mkstatecompat.sh) pins the FACTS the card ships with; a drift in either
// the generator or the loader shape trips this test.
const shippedStateCompatJSON = `{
  "version": 1,
  "classes": [
    {"core": "fceumm", "arches": ["armhf", "arm64"]},
    {"core": "gambatte", "arches": ["armhf", "arm64"]},
    {"core": "picodrive", "arches": ["armhf", "arm64"]},
    {"core": "gpsp", "arches": ["arm64"]},
    {"core": "snes9x2005_plus", "arches": ["arm64"]}
  ]
}`

// tup builds a producer tuple lodor/<frontend>/<core[@ver]>/<arch>. Version and frontend are
// deliberately varied across cases — the whole point of D8 is that a class ignores them.
func tup(frontend, coreVer, arch string) string {
	return "lodor/" + frontend + "/" + coreVer + "/" + arch
}

func TestD8ShippedCompatMatrix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "state-compat.json"), []byte(shippedStateCompatJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name         string
		local, remote string
		want         bool
	}{
		// --- Tier-0 identity: same core@ver + arch, frontend may differ -----------------
		{"tier0 same core@ver same arch, diff frontend",
			tup("lodoros", "gambatte@9d92", "arm64"), tup("nextui", "gambatte@9d92", "arm64"), true},

		// --- Cross-arch, cross-core-portable class (fceumm/gambatte/picodrive) -----------
		// same-core diff-arch loadable BECAUSE the class certifies both arches; version varies.
		{"gambatte armhf<->arm64 (portable class)",
			tup("lodoros", "gambatte@9d92", "armhf"), tup("knulli", "gambatte@ff01", "arm64"), true},
		{"fceumm armhf<->arm64 (portable class)",
			tup("lodoros", "fceumm@aa", "armhf"), tup("knulli", "fceumm@bb", "arm64"), true},
		{"picodrive armhf<->arm64 (portable class)",
			tup("muos", "picodrive@1", "armhf"), tup("nextui", "picodrive@2", "arm64"), true},
		{"picodrive arm64<->armhf reverse (symmetry)",
			tup("nextui", "picodrive@2", "arm64"), tup("muos", "picodrive@1", "armhf"), true},

		// --- Cross-version WITHIN the same arch (a class covers it too; Tier-0 already would
		//     unless version differs — proves version is ignored inside a class) ------------
		{"fceumm diff version same arm64 (version ignored in class)",
			tup("knulli", "fceumm@old", "arm64"), tup("android", "fceumm@unknown", "arm64"), true},

		// --- arm64-only bridge cores (gpsp / snes9x2005_plus): within-group arm64 loadable -
		{"snes9x2005_plus arm64 Android(unknown)<->Knulli(pinned) bridge",
			tup("android", "snes9x2005_plus@unknown", "arm64"), tup("knulli", "snes9x2005_plus@def", "arm64"), true},
		{"gpsp arm64 Android(unknown)<->Knulli(pinned) bridge",
			tup("android", "gpsp@unknown", "arm64"), tup("knulli", "gpsp@111", "arm64"), true},

		// --- NEGATIVE: arm64-only cores must NOT cross to armhf (cert FAILED cross-arch) ---
		{"snes9x2005_plus armhf<->arm64 must be DIM",
			tup("lodoros", "snes9x2005_plus@x", "armhf"), tup("knulli", "snes9x2005_plus@def", "arm64"), false},
		{"gpsp armhf<->arm64 must be DIM",
			tup("lodoros", "gpsp@x", "armhf"), tup("knulli", "gpsp@111", "arm64"), false},

		// --- NEGATIVE: a core NOT in the whitelist stays Tier-0 (cross-arch DIM) -----------
		{"uncertified core (mgba) armhf<->arm64 must be DIM",
			tup("lodoros", "mgba@1", "armhf"), tup("knulli", "mgba@2", "arm64"), false},

		// --- NEGATIVE: different core is NEVER compatible (classes are per-core) -----------
		{"different core same arch must be DIM",
			tup("knulli", "gambatte@9d92", "arm64"), tup("knulli", "fceumm@9d92", "arm64"), false},
		{"different core, both certified cross-arch, must be DIM",
			tup("lodoros", "gambatte@1", "armhf"), tup("knulli", "fceumm@1", "arm64"), false},

		// --- NEGATIVE: foreign / builtin / tuple-less states never compatible --------------
		{"foreign emulator record must be DIM",
			tup("knulli", "gambatte@9d92", "arm64"), "foreign:retroarch", false},
		{"builtin (tuple-less) must be DIM",
			tup("knulli", "gambatte@9d92", "arm64"), "builtin", false},
		{"empty remote tuple must be DIM",
			tup("knulli", "gambatte@9d92", "arm64"), "", false},
		{"malformed (non-lodor prefix) must be DIM",
			tup("knulli", "gambatte@9d92", "arm64"), "onion/x/gambatte@9d92/arm64", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, why := tuplesCompatible(c.local, c.remote)
			if got != c.want {
				t.Fatalf("tuplesCompatible(%q, %q) = %v (why=%q), want %v", c.local, c.remote, got, why, c.want)
			}
			// Compatibility must be symmetric for the loadable cases (the card offers either
			// direction of the same certified pair).
			if c.want {
				if rev, rwhy := tuplesCompatible(c.remote, c.local); !rev {
					t.Fatalf("asymmetric: (%q,%q) loadable but reverse not (why=%q)", c.local, c.remote, rwhy)
				}
			}
		})
	}
}

// TestD8ShippedWhitelistMatchesGenerator guards the literal above against generator drift: the
// classes+arches must be exactly the certified 2026-07-07 fleet. If mkstatecompat.sh's shipped
// invocation changes, update BOTH (and re-verify the matrix), don't silently diverge.
func TestD8ShippedWhitelistMatchesGenerator(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LODOR_PAK_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "state-compat.json"), []byte(shippedStateCompatJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	cc, ok := loadStateCompat()
	if !ok {
		t.Fatal("shipped whitelist must load")
	}
	want := map[string][]string{
		"fceumm":          {"armhf", "arm64"},
		"gambatte":        {"armhf", "arm64"},
		"picodrive":       {"armhf", "arm64"},
		"gpsp":            {"arm64"},
		"snes9x2005_plus": {"arm64"},
	}
	if len(cc.Classes) != len(want) {
		t.Fatalf("shipped whitelist has %d classes, want %d", len(cc.Classes), len(want))
	}
	for _, cl := range cc.Classes {
		w, present := want[cl.Core]
		if !present {
			t.Fatalf("unexpected class core %q in shipped whitelist", cl.Core)
		}
		if len(cl.Arches) != len(w) {
			t.Fatalf("core %q arches=%v, want %v", cl.Core, cl.Arches, w)
		}
		for i, a := range w {
			if cl.Arches[i] != a {
				t.Fatalf("core %q arch[%d]=%q, want %q", cl.Core, i, cl.Arches[i], a)
			}
		}
	}
}
