//go:build android || lodorandroid

package platform

// RetroArch Android: states live in shared storage, FLAT by default
// (savestate sorting off upstream; "Game (USA).state1" naming). The app overrides
// via LODOR_STATE_ROOT (states.go honors it) when the user's RA points elsewhere;
// statecores.json entries use dir "." for the flat root (filepath.Join cleans it
// to the root itself — pinned by test).
func stateRootDefault() string { return "/storage/emulated/0/RetroArch/states" }

const stateNamingRA = true
