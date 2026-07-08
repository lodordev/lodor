//go:build muos

package platform

// muOS: RetroArch states, sorted by core display name (MustardOS
// retroarch.default.cfg: savestate_directory=/run/muos/storage/save/state,
// sort_savestates_enable=true — source-verified 2026-07-06).
func stateRootDefault() string { return "/run/muos/storage/save/state" }

const stateNamingRA = true
