//go:build !muos && !knulli && !onion && !android && !lodorandroid

package platform

import "path/filepath"

// minarch hosts (LodorOS, NextUI): states live under the shared userdata tree,
// one dir per {TAG}-{core} (minarch.c:3242). RA naming off.
func stateRootDefault() string { return filepath.Join(BasePath(), ".userdata", "shared") }

const stateNamingRA = false
