// Package update implements the self-update check + staged fetch: a versions.json
// manifest on a static host names the newest release per channel; the engine
// compares, and (on the LodorOS lane) stages a verified zip for the shell to
// apply. Grout is consulted only as a behavioral oracle — this is a from-scratch
// implementation, stdlib only, CGO-free.
package update

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed release version: up to four dot-separated numeric
// components plus an optional prerelease tag ("0.9.4", "4.9.1.0",
// "0.9.5-beta.2"). Missing components are zero.
type Version struct {
	Nums [4]int
	Pre  string // empty = full release
}

// ParseVersion parses a version string. A leading "v" is tolerated. The first
// "-" splits numeric part from prerelease. Errors on empty/non-numeric
// components — a malformed manifest version must fail loudly, not compare as 0.
func ParseVersion(s string) (Version, error) {
	var v Version
	s = strings.TrimSpace(strings.TrimPrefix(s, "v"))
	if s == "" {
		return v, fmt.Errorf("empty version")
	}
	num, pre, _ := strings.Cut(s, "-")
	v.Pre = pre
	parts := strings.Split(num, ".")
	if len(parts) > 4 {
		return v, fmt.Errorf("version %q has more than 4 numeric components", s)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("version %q: bad component %q", s, p)
		}
		v.Nums[i] = n
	}
	return v, nil
}

// Compare returns -1 if a < b, 1 if a > b, 0 if equal. Numeric components
// compare first; on a tie, a full release outranks any prerelease of the same
// number, and two prereleases compare per semver identifier rules (numeric
// identifiers numerically, otherwise bytewise; "beta.10" > "beta.6").
func Compare(a, b Version) int {
	for i := 0; i < 4; i++ {
		if a.Nums[i] != b.Nums[i] {
			if a.Nums[i] < b.Nums[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.Pre == "" && b.Pre == "":
		return 0
	case a.Pre == "":
		return 1 // full release > prerelease
	case b.Pre == "":
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		ai, aNum := numeric(as[i])
		bi, bNum := numeric(bs[i])
		switch {
		case aNum && bNum:
			if ai != bi {
				if ai < bi {
					return -1
				}
				return 1
			}
		case aNum: // numeric identifiers sort before alphanumeric (semver §11)
			return -1
		case bNum:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	// shared prefix equal: the longer prerelease is higher (semver §11)
	switch {
	case len(as) < len(bs):
		return -1
	case len(as) > len(bs):
		return 1
	}
	return 0
}

func numeric(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}
