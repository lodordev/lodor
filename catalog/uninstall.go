// Uninstall — "Remove Lodor from this card" (C2 design §5), manifest-driven.
//
// Walks the mirror-owned manifest and removes exactly what the mirror created,
// leaving the user's tree byte-identical: stubs (re-verified 0-byte at delete
// time), our covers, our Collections files (never map.txt), and our folders
// (only-if-empty via plain os.Remove, deepest-first). DOWNLOADS ARE KEPT by
// default — they're the user's games now; the explicit removeDownloads second
// confirmation deletes them too (multi-disc discs included). SAVES ARE NEVER
// TOUCHED. No rename-back step exists because no user file was ever renamed.
//
// Fail-safe: a missing/corrupt manifest owns nothing, so uninstall removes
// nothing (ok=false) — it never guesses. Offline by design.
package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/platform"
)

// UninstallResult is the honest per-kind outcome of an uninstall pass.
type UninstallResult struct {
	Removed       int  // files/folders deleted
	KeptDownloads int  // downloads intentionally left (removeDownloads=false)
	Skipped       int  // owned paths NOT removed (state drifted / delete failed)
	Ok            bool // false = manifest unusable, nothing attempted
}

// UninstallMirror removes every manifest-owned artifact from the card.
func UninstallMirror(cfg *config.Config, removeDownloads bool) UninstallResult {
	man := platform.LoadManifest()
	if len(man.Entries) == 0 {
		// Absent, corrupt, or genuinely empty: ownership unknowable/none — remove
		// nothing (the caller reports honestly; the pak may still delete its own
		// pak-dir state).
		return UninstallResult{Ok: false}
	}
	res := UninstallResult{Ok: true}
	sd := sdcardRoot()

	// Deepest paths first so files empty their folders before the folder pass.
	rels := make([]string, 0, len(man.Entries))
	for rel := range man.Entries {
		rels = append(rels, rel)
	}
	sort.Slice(rels, func(i, j int) bool {
		di, dj := strings.Count(rels[i], "/"), strings.Count(rels[j], "/")
		if di != dj {
			return di > dj
		}
		return rels[i] < rels[j]
	})

	// Pass 1: files (folders after, when emptied).
	for _, rel := range rels {
		e := man.Entries[rel]
		abs := filepath.Join(sd, rel)
		switch e.Kind {
		case platform.ManifestStub:
			fi, err := os.Stat(abs)
			if err != nil {
				man.Forget(abs) // already gone
				continue
			}
			if fi.IsDir() || fi.Size() != 0 {
				res.Skipped++ // became real without a manifest update — not ours to judge
				continue
			}
			removeAndForget(man, abs, &res)
		case platform.ManifestDownload:
			if _, err := os.Stat(abs); err != nil {
				man.Forget(abs)
				continue
			}
			if !removeDownloads {
				res.KeptDownloads++
				continue
			}
			// Multi-disc: the .m3u's referenced disc files ARE the bytes.
			if strings.EqualFold(filepath.Ext(abs), ".m3u") {
				evictDiscFiles(abs)
			}
			removeAndForget(man, abs, &res)
		case platform.ManifestCover:
			if _, err := os.Stat(abs); err != nil {
				man.Forget(abs)
				continue
			}
			removeAndForget(man, abs, &res)
			_ = os.Remove(filepath.Dir(abs)) // .media — falls unless empty
		case platform.ManifestCollection, platform.ManifestContinue:
			if strings.EqualFold(filepath.Base(abs), "map.txt") {
				man.Forget(abs) // NextUI's file, full stop — a lying manifest can't claim it
				continue
			}
			if _, err := os.Stat(abs); err != nil {
				man.Forget(abs)
				continue
			}
			removeAndForget(man, abs, &res)
		}
	}

	// Pass 2: folders, deepest-first, only-if-empty (os.Remove semantics). An
	// owned folder still holding anything (kept downloads, user files) stays.
	for _, rel := range rels {
		e, ok := man.Entries[rel]
		if !ok || e.Kind != platform.ManifestFolder {
			continue
		}
		abs := filepath.Join(sd, rel)
		if _, err := os.Stat(abs); err != nil {
			man.Forget(abs)
			continue
		}
		_ = os.Remove(filepath.Join(abs, ".media")) // ours only if emptied by pass 1
		if os.Remove(abs) == nil {
			man.Forget(abs)
			res.Removed++
		} else {
			res.Skipped++
		}
	}

	// Retire the engine-owned state: manifest, catalog index, legacy ledger. The
	// pak's launch.sh owns its own config/settings teardown.
	_ = os.Remove(IndexPath(cfg))
	_ = os.Remove(collectionsLedgerPath())
	if err := os.Remove(platform.ManifestPath()); err != nil {
		fmt.Fprintf(os.Stderr, "UNINSTALL: manifest not removed: %v\n", err)
	}
	return res
}

func removeAndForget(man *platform.Manifest, abs string, res *UninstallResult) {
	if os.Remove(abs) == nil {
		man.Forget(abs)
		res.Removed++
	} else {
		res.Skipped++
	}
}
