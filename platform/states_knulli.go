//go:build knulli

package platform

// Knulli/Batocera: states share the per-system saves dir
// (platform_knulli.go save layout; *.state* naming — source-verified).
func stateRootDefault() string { return "/userdata/saves" }

const stateNamingRA = true
