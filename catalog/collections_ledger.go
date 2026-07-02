// LEGACY collections ownership ledger (STEP 0b of the 2026-07-02 stabilization
// pass; C1 coexist-design audit finding V2) — READ-ONLY since C2.
//
// The 0b ledger (one collection filename per line beside catalog-index.json) was
// the minimal slice of the C1 design's mirror-owned manifest, shipped early
// because the V2 collections-deletion bug bit live cards. C2 formalized ownership
// into platform.Manifest (mirror-manifest.json — ONE mechanism, not two):
// MirrorCollections now records kind=collection/continue entries there, imports a
// legacy ledger's names ONCE on a card whose manifest has no collection entries
// yet, and retires the ledger file after the first successful manifest save.
// This file keeps only the read/path helpers that import needs.
package catalog

import (
	"os"
	"path/filepath"
	"strings"

	"lodor/platform"
)

// collectionsLedgerPath returns the ledger location: beside catalog-index.json
// in the host pak's working dir — engine-owned, outside the user's Collections/.
func collectionsLedgerPath() string {
	return filepath.Join(platform.PakDir(), "collections-owned.txt")
}

// readOwnedCollections loads the set of collection filenames the mirror wrote on
// a previous pass. Returns nil when the ledger is missing/unreadable — callers
// treat nil as "ownership unknowable: prune nothing".
func readOwnedCollections() map[string]bool {
	data, err := os.ReadFile(collectionsLedgerPath())
	if err != nil {
		return nil
	}
	owned := map[string]bool{}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		owned[line] = true
	}
	return owned
}

