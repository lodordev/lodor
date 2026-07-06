//go:build !knulli

package main

// gamelistEnabled: no non-Knulli host reads gamelist.xml (MinUI/NextUI, OnionOS
// and muOS render the filename directly), so the emitter is compiled but DEAD —
// maybeWriteGamelists is a no-op and --write-gamelists refuses. Every existing
// platform's behavior stays byte-identical.
const gamelistEnabled = false
