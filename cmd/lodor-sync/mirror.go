package main

import (
	"fmt"
	"os"

	"lodor/catalog"
	"lodor/config"
	"lodor/romm"
)

// runMirrorCatalog stubs every not-yet-downloaded single-file ROM of each mapped
// platform into its Roms/ folder and writes catalog-index.json, then prints the
// §8 contract. The heavy lifting (and the index write) lives in catalog.MirrorCatalog.
//
//	MIRROR created=%d existing=%d skipped=%d multifile=%d covers=%d
//
// A hard failure (couldn't list platforms / couldn't write the index) is a config/
// reachability error: exit 3 so the launcher reports "couldn't reach RomM".
func runMirrorCatalog(client *romm.Client, cfg *config.Config) {
	// Start fresh: clear any stale bar/label from a prior mode so the launcher's overlay
	// opens at 0% with our first real label, not a leftover "100 / Library updated".
	writeProgress(0)
	writePhase("Reading library…")

	// Wire the engine's honest, real-count progress to the /tmp side-channels the
	// launcher polls (BLUEPRINT §8/§10). These writes NEVER touch stdout — the MIRROR
	// RESULT line below is the only thing on stdout.
	rep := &catalog.Reporter{Phase: writePhase, Percent: writeProgress}

	created, existing, skipped, multifile, covers, err := catalog.MirrorCatalog(client, cfg, rep)
	if err != nil {
		// Honest failure: leave a host-free error label and reset the bar so the launcher
		// doesn't show a stuck partial bar.
		writeProgress(0)
		writePhase("Couldn't reach RomM")
		fmt.Fprintf(os.Stderr, "FATAL mirror: %s\n", safeErr(err))
		os.Exit(3)
	}
	fmt.Printf("MIRROR created=%d existing=%d skipped=%d multifile=%d covers=%d\n",
		created, existing, skipped, multifile, covers)
	os.Exit(0)
}

// runMirrorCollections writes one Collections/<name>.txt per RomM collection and
// prints the §8 contract:
//
//	COLLECTIONS written=%d empty=%d total=%d
//
// The collection list comes from the network; a failure there is exit 3.
func runMirrorCollections(client *romm.Client, cfg *config.Config) {
	writeProgress(0)
	writePhase("Reading collections…")

	rep := &catalog.Reporter{Phase: writePhase, Percent: writeProgress}

	written, empty, total, err := catalog.MirrorCollections(client, cfg, rep)
	if err != nil {
		writeProgress(0)
		writePhase("Couldn't reach RomM")
		fmt.Fprintf(os.Stderr, "FATAL collections: %s\n", safeErr(err))
		os.Exit(3)
	}
	fmt.Printf("COLLECTIONS written=%d empty=%d total=%d\n", written, empty, total)
	os.Exit(0)
}
