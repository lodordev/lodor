package sync

// The D8 certification whitelist (design lodor-statesync-design-2026-07-07.md).
//
// v1 compatibility (statepull.go tuplesCompatible) is Tier-0: two states are
// compatible only if their producer tuples are byte-equal on core@version AND
// architecture. That is deliberately narrow — it never offers a state that
// might not load. D8 WIDENS it with FACTS earned by the cross-architecture
// certification harness (release/xarch-cert/): a "compat class" declares that a
// given core's save-state format interoperates across a set of architectures,
// regardless of the version string (prior art: version strings lie; and the
// Android lane's cores are user-updated, so their version is literally
// "unknown" — a class is the only way they can ever interoperate).
//
//	<LODOR_PAK_DIR>/state-compat.json
//	{ "version": 1, "classes": [
//	    { "core": "gambatte",   "arches": ["armhf","arm64"] },  // cert: portable
//	    { "core": "snes9x2005_plus", "arches": ["arm64"] }      // cert: arm64-only
//	] }
//
// A class asserts: for THIS core, any two builds whose arches are BOTH in the
// set produce mutually-loadable states (any version, any frontend). A single
// arch in the set ("arm64" only) is the within-group / version-bridge case —
// it lets an Android "@unknown" arm64 state interoperate with a pinned arm64
// state of the same core, without crossing to armhf (which failed cert).
//
// FAIL-CLOSED: absent, unreadable, or malformed file → NO classes → the engine
// falls back to pure Tier-0 tuple-equality (current behavior). D8 only ever
// WIDENS compatibility; it can never narrow the base policy, and it can never
// make a foreign/tuple-less record compatible. Ships dark until an assembler
// (or the Android app at runtime) writes state-compat.json.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"lodor/platform"
)

// stateCompatClass is one certified-interoperable core across an arch set.
type stateCompatClass struct {
	Core   string   `json:"core"`
	Arches []string `json:"arches"`
}

type stateCompat struct {
	Version int                `json:"version"`
	Classes []stateCompatClass `json:"classes"`
}

func stateCompatPath() string {
	return filepath.Join(platform.PakDir(), "state-compat.json")
}

// loadStateCompat reads the whitelist; ok=false (no widening) when absent or
// structurally unusable — fail closed, never invent a certification.
func loadStateCompat() (stateCompat, bool) {
	var cc stateCompat
	data, err := os.ReadFile(stateCompatPath())
	if err != nil {
		return cc, false
	}
	if json.Unmarshal(data, &cc) != nil || len(cc.Classes) == 0 {
		return cc, false
	}
	return cc, true
}

// coreName strips the "@version" suffix from a tuple's core component
// ("gambatte@9d923816" → "gambatte"). The whitelist keys on the bare core.
func coreName(coreVer string) string {
	if i := strings.IndexByte(coreVer, '@'); i >= 0 {
		return coreVer[:i]
	}
	return coreVer
}

func (c stateCompatClass) hasArch(a string) bool {
	for _, x := range c.Arches {
		if x == a {
			return true
		}
	}
	return false
}

// certifiedCompatible reports whether a D8 class certifies that these two
// (core, arch) producers interoperate. Same bare core required (classes are
// per-core); both arches must be members of one class's set. Version and
// frontend are intentionally ignored — that is exactly what a certification
// asserts. No file / no matching class → false (base Tier-0 policy stands).
func certifiedCompatible(coreA, archA, coreB, archB string) bool {
	ca, cb := coreName(coreA), coreName(coreB)
	if ca != cb {
		return false
	}
	cc, ok := loadStateCompat()
	if !ok {
		return false
	}
	for _, cl := range cc.Classes {
		if cl.Core == ca && cl.hasArch(archA) && cl.hasArch(archB) {
			return true
		}
	}
	return false
}
