package update

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.9.4", "0.9.4", 0},
		{"0.9.4", "0.9.5", -1},
		{"0.9.5", "0.9.4", 1},
		{"0.9.4", "0.10.0", -1},
		{"1.0.0", "0.9.9", 1},
		{"4.9.1.0", "4.9.1", 0},   // missing 4th component = 0
		{"4.9.1.1", "4.9.1.0", 1}, // 4-component build bump
		{"v0.9.4", "0.9.4", 0},    // leading v tolerated
		// prerelease ordering (semver §11)
		{"0.9.5-beta", "0.9.5", -1},    // full release beats its prerelease
		{"0.9.5", "0.9.5-beta", 1},
		{"0.9.5-beta", "0.9.4", 1},     // prerelease of NEXT version beats current full
		{"0.9.5-beta.2", "0.9.5-beta.10", -1}, // numeric identifiers compare numerically
		{"0.9.5-beta.10", "0.9.5-beta.6", 1},
		{"0.9.5-alpha", "0.9.5-beta", -1},     // alphanumeric identifiers bytewise
		{"0.9.5-beta", "0.9.5-beta.1", -1},    // longer prerelease wins on shared prefix
		{"0.9.5-1", "0.9.5-beta", -1},         // numeric identifier sorts before alphanumeric
	}
	for _, c := range cases {
		av, err := ParseVersion(c.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.a, err)
		}
		bv, err := ParseVersion(c.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", c.b, err)
		}
		if got := Compare(av, bv); got != c.want {
			t.Errorf("Compare(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseVersionRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "abc", "1.x.3", "1.2.3.4.5", "-beta", "1..2"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) accepted garbage", s)
		}
	}
}
