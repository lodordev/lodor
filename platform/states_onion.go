//go:build onion

package platform

// OnionOS: stock RetroArch save-states, sorted by core display name. Source-
// verified from OnionUI/Onion static/configs/RetroArch/.retroarch/retroarch.cfg:
// savestate_directory = "/mnt/SDCARD/Saves/CurrentProfile/states" (L2964) with
// sort_savestates_enable = "true" (L3003) -> per-core-display-name subfolders,
// the same shape as the battery-save tree (saves/<CoreDisplayName>/). The per-rom
// directory component (the CoreDisplayName, e.g. "mGBA") ships in the lane's
// statecores.json manifest and arrives via StateDirFor(dir). LODOR_STATE_ROOT
// still overrides for the off-hardware harness.
func stateRootDefault() string { return "/mnt/SDCARD/Saves/CurrentProfile/states" }

const stateNamingRA = true
