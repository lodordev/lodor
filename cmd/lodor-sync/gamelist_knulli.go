//go:build knulli

package main

// gamelistEnabled: the Knulli build's EmulationStation reads per-folder
// gamelist.xml, so the emitter (gamelist.go) is LIVE — the mirror/reconcile/
// evict hooks refresh gamelists and --write-gamelists is a supported mode.
const gamelistEnabled = true
