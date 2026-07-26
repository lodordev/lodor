package main

// OFFLINE, READ-ONLY per-volume storage view (Argosy "steal", 2026-07-23): report how
// much of each mapped platform's download cache is REAL bytes on the card versus 0-byte
// cloud stubs, plus a grand total. It touches NOTHING — no delete, no truncate, no
// network, no device — so it runs before the hosts gate like the other offline modes.
//
// The report DELIBERATELY carries no "clear cache" affordance: eviction already lives in
// --evict (one ROM, saves NEVER deleted) and --uninstall-mirror --remove-downloads (the
// whole mirror). This mode only measures; the header points a curious user at --evict.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lodor/config"
	"lodor/platform"
)

// platformStorage is one mapped platform's download-cache footprint on the card.
type platformStorage struct {
	Slug            string `json:"slug"`
	RelativePath    string `json:"relative_path"`
	Downloaded      int    `json:"downloaded"`       // real, >0-byte ROM files present on the card
	DownloadedBytes int64  `json:"downloaded_bytes"` // sum of those files' sizes
	Stubs           int    `json:"stubs"`            // 0-byte cloud stubs (not downloaded)
}

// computeStorageReport walks each mapped platform's ROM folder under Roms/ and tallies
// downloaded ROMs (real bytes) vs cloud stubs (0-byte). A downloaded ROM is any regular
// file with size > 0; a stub is a 0-byte file (the MarkerCloud/0-byte scheme). Subfolders
// (multi-disc disc dirs, .media covers) and dotfiles are not cache ROM files and are
// skipped. Ownership scoping: when the mirror manifest holds any entry we scope to
// Lodor-owned files (manifest-recorded OR carrying a leading state marker) so a user's own
// ROMs don't inflate the numbers; with no manifest we report ALL ROMs under the mapped
// dirs (scopeOwned=false) and the caller says so in the header. Pure: no I/O beyond
// reads, never exits — the exiting wrapper is runStorageReport, so this stays testable.
func computeStorageReport(cfg *config.Config, man *platform.Manifest) (rows []platformStorage, totDownloaded, totStubs int, totBytes int64, scopeOwned bool) {
	scopeOwned = man != nil && len(man.Entries) > 0
	romsRoot := filepath.Join(sdRoot(), "Roms")

	// Deterministic output order.
	slugs := make([]string, 0, len(cfg.DirectoryMappings))
	for slug := range cfg.DirectoryMappings {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	for _, slug := range slugs {
		m := cfg.DirectoryMappings[slug]
		if m.RelativePath == "" {
			continue // no on-card folder to measure
		}
		row := platformStorage{Slug: slug, RelativePath: m.RelativePath}
		dir := filepath.Join(romsRoot, m.RelativePath)
		ents, err := os.ReadDir(dir)
		if err != nil {
			// Folder never mirrored / absent: a legitimate all-zero row, not an error.
			rows = append(rows, row)
			continue
		}
		for _, e := range ents {
			name := e.Name()
			if e.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			path := filepath.Join(dir, name)
			if scopeOwned && !man.Owns(path) && !platform.HasLeadingMarker(name) {
				continue // a user's own ROM — outside the Lodor download cache
			}
			fi, ierr := e.Info()
			if ierr != nil {
				continue
			}
			if fi.Size() > 0 {
				row.Downloaded++
				row.DownloadedBytes += fi.Size()
			} else {
				row.Stubs++
			}
		}
		totDownloaded += row.Downloaded
		totStubs += row.Stubs
		totBytes += row.DownloadedBytes
		rows = append(rows, row)
	}
	return rows, totDownloaded, totStubs, totBytes, scopeOwned
}

// runStorageReport prints the storage view and exits 0. --json emits the machine form;
// otherwise an aligned human table. Read-only always.
func runStorageReport(cfg *config.Config, asJSON bool) {
	man := platform.LoadManifest()
	rows, totDownloaded, totStubs, totBytes, scopeOwned := computeStorageReport(cfg, man)

	if asJSON {
		out := struct {
			Scope           string            `json:"scope"`
			Platforms       []platformStorage `json:"platforms"`
			TotalDownloaded int               `json:"total_downloaded"`
			TotalStubs      int               `json:"total_stubs"`
			TotalBytes      int64             `json:"total_bytes"`
		}{storageScopeLabel(scopeOwned), rows, totDownloaded, totStubs, totBytes}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		os.Exit(0)
	}

	// Human table: platform column widened to the longest folder name (min 24).
	nameW := 24
	for _, r := range rows {
		if l := len(r.RelativePath); l > nameW {
			nameW = l
		}
	}
	fmt.Printf("STORAGE REPORT — download cache on the card (%s; read-only, reclaim with --evict)\n",
		storageScopeLabel(scopeOwned))
	fmt.Printf("%-*s  %10s  %12s  %6s\n", nameW, "PLATFORM", "DOWNLOADED", "SIZE", "STUBS")
	for _, r := range rows {
		fmt.Printf("%-*s  %10d  %12s  %6d\n", nameW, r.RelativePath, r.Downloaded, humanBytes(r.DownloadedBytes), r.Stubs)
	}
	fmt.Printf("%-*s  %10d  %12s  %6d\n", nameW, "TOTAL", totDownloaded, humanBytes(totBytes), totStubs)
	os.Exit(0)
}

// storageScopeLabel names which files the report counted, for the header / JSON.
func storageScopeLabel(owned bool) string {
	if owned {
		return "scope=lodor-owned"
	}
	return "scope=all-roms"
}

// humanBytes renders a byte count in binary units (B/KB/MB/GB/…), one decimal place
// above bytes. Read-only formatting helper.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
