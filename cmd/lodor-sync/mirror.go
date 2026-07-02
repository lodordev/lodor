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
//	MIRROR created=%d existing=%d skipped=%d multifile=%d covers=%d adopted=%d
//
// A hard failure (couldn't list platforms / couldn't write the index) is a config/
// reachability error: exit 3 so the launcher reports "couldn't reach RomM".
func runMirrorCatalog(client *romm.Client, cfg *config.Config, coverForce bool) {
	// Start fresh: clear any stale bar/label from a prior mode so the launcher's overlay
	// opens at 0% with our first real label, not a leftover "100 / Library updated".
	writeProgress(0)
	if coverForce {
		writePhase("Rebuilding library…") // Full: re-fetch every cover
	} else {
		writePhase("Updating library…") // Update: only new games + missing covers
	}

	// Coexist mode-flip migration pre-pass (C2 §6): prompt-only (explicit mode),
	// push-before-remove, manifest-scoped — see migrate.go. Best-effort: the mirror
	// proceeds regardless, and a deferred migration retries next run.
	migrateMirrorLayoutIfNeeded(client, cfg)

	// Wire the engine's honest, real-count progress to the /tmp side-channels the
	// launcher polls (BLUEPRINT §8/§10). These writes NEVER touch stdout — the MIRROR
	// RESULT line below is the only thing on stdout.
	rep := &catalog.Reporter{Phase: writePhase, Percent: writeProgress}

	created, existing, skipped, multifile, covers, adopted, err := catalog.MirrorCatalog(client, cfg, rep, coverForce)
	if err != nil {
		// Honest failure: leave a host-free error label and reset the bar so the launcher
		// doesn't show a stuck partial bar. An expired/revoked token gets its own label
		// and the PAIRING_EXPIRED contract (exit 6) instead of "couldn't reach".
		writeProgress(0)
		noteAuthErr(err)
		if pairingExpired {
			writePhase("Pairing expired — re-pair this device")
		} else {
			writePhase("Couldn't reach RomM")
		}
		fmt.Fprintf(os.Stderr, "FATAL mirror: %s\n", safeErr(err))
		exitMode(3)
	}
	// adopted= is APPENDED so every existing field parser (the pak seds on
	// created=/…) keeps matching byte-identically; merge-mode runs report how many
	// server games were matched to the user's own files instead of stubbed.
	fmt.Printf("MIRROR created=%d existing=%d skipped=%d multifile=%d covers=%d adopted=%d\n",
		created, existing, skipped, multifile, covers, adopted)
	os.Exit(0)
}

// runMirrorCollections writes one Collections/<name>.txt per RomM collection and
// prints the §8 contract:
//
//	COLLECTIONS written=%d empty=%d total=%d
//	CONTINUE entries=%d
//
// (CONTINUE is the cross-device "0) Continue" collection, task #37 — a second,
// separate stdout line so existing COLLECTIONS parsers are untouched; entries=0
// means the feed was empty and no Continue file exists on the card.)
// The collection list comes from the network; a failure there is exit 3.
func runMirrorCollections(client *romm.Client, cfg *config.Config) {
	writeProgress(0)
	writePhase("Reading collections…")

	rep := &catalog.Reporter{Phase: writePhase, Percent: writeProgress}

	written, empty, total, cont, err := catalog.MirrorCollections(client, cfg, rep)
	if err != nil {
		writeProgress(0)
		noteAuthErr(err)
		if pairingExpired {
			writePhase("Pairing expired — re-pair this device")
		} else {
			writePhase("Couldn't reach RomM")
		}
		fmt.Fprintf(os.Stderr, "FATAL collections: %s\n", safeErr(err))
		exitMode(3)
	}
	fmt.Printf("COLLECTIONS written=%d empty=%d total=%d\n", written, empty, total)
	fmt.Printf("CONTINUE entries=%d\n", cont)
	os.Exit(0)
}

// runUninstallMirror is the "Remove Lodor from this card" engine leg (C2 §5):
// a manifest walk that deletes exactly what the mirror created — stubs, our
// covers/collections/folders — keeping downloads unless removeDownloads, never
// touching saves or any user file. Contract:
//
//	RESULT uninstalled=<0|1> removed=<N> kept_downloads=<K> skipped=<S>
//
// uninstalled=0 = the manifest was missing/corrupt/empty: ownership unknowable,
// NOTHING was removed (the pak reports honestly and may still delete its own
// pak-local state). Offline; exit 0 either way.
func runUninstallMirror(cfg *config.Config, removeDownloads bool) {
	res := catalog.UninstallMirror(cfg, removeDownloads)
	fmt.Printf("RESULT uninstalled=%d removed=%d kept_downloads=%d skipped=%d\n",
		b2i(res.Ok), res.Removed, res.KeptDownloads, res.Skipped)
	os.Exit(0)
}
