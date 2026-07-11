// Self-update modes (--check-update / --fetch-update) + --version.
//
// Both update modes run BEFORE config.Load(): an unpaired device must still be
// able to check for and stage updates. They read settings.conf (CWD-relative,
// beside config.json) for the update_channel toggle but NEVER write it — the
// launcher owns settings.conf writes (update_last_check / update_available /
// update_skip are stamped by the shell from this mode's RESULT line, keeping
// the single-writer discipline).
//
// Contract (BLUEPRINT §8 style):
//
//	--check-update  RESULT update=<0|1> current=<v> latest=<v> channel=<stable|beta>
//	                then, when the channel carries notes: NOTES\t<single line>
//	                exit 0 ok / 3 manifest unreachable-or-unusable (shell: silence)
//	--fetch-update  RESULT fetched=<0|1> version=<v> bytes=<n>
//	                exit 0 ok / 2 no LODOR_UPDATE_ASSET / 3 unreachable or no
//	                asset for this lane / 4 hash mismatch (staging removed)
//	                An interrupted transfer keeps its partial zip beside the
//	                staging (.update.partial, with an asset-identity file); the
//	                next --fetch-update for the SAME asset Range-resumes it
//	                (lodor#46). With --cancellable, a B-press cancel stops the
//	                transfer, keeps the partial, and reports the ADDITIVE
//	                "cancelled=1" on the RESULT line with exit 0 (the lodor#42
//	                convention — an honest stop, not an error). Parsers must key
//	                on fetched=1, never on the exit code alone.
//
// A "dev" build reports update=0 and never fetches: a binary not stamped by
// release.sh must not nag or self-clobber.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"lodor/buildinfo"
	"lodor/covercancel"
	"lodor/update"
)

// fetchUpdateTimeout bounds the whole artifact download. Update zips are
// tens of MB and device Wi-Fi is slow; ROMs already get 3600s by config —
// updates are smaller, 30 minutes is generous without being unbounded.
const fetchUpdateTimeout = 30 * time.Minute

func runVersion() {
	fmt.Printf("lodor-sync %s\n", buildinfo.Version)
	os.Exit(0)
}

func runCheckUpdate() {
	channel := updateChannel()
	if buildinfo.Version == "dev" {
		fmt.Printf("RESULT update=0 current=dev latest=- channel=%s\n", channel)
		os.Exit(0)
	}
	cur, err := update.ParseVersion(buildinfo.Version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL buildinfo: bad embedded version %q: %v\n", buildinfo.Version, err)
		os.Exit(2)
	}
	ch := fetchChannel(channel)
	latest, err := update.ParseVersion(ch.Version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check-update: manifest version %q unparseable: %v\n", ch.Version, err)
		os.Exit(3)
	}
	upd := 0
	if update.Compare(latest, cur) > 0 {
		upd = 1
	}
	fmt.Printf("RESULT update=%d current=%s latest=%s channel=%s\n", upd, buildinfo.Version, ch.Version, channel)
	if upd == 1 && ch.Notes != "" {
		fmt.Printf("NOTES\t%s\n", oneLine(ch.Notes))
	}
	os.Exit(0)
}

func runFetchUpdate(cancellable bool) {
	channel := updateChannel()
	if buildinfo.Version == "dev" {
		fmt.Fprintln(os.Stderr, "fetch-update: refusing on a dev build (no stamped version)")
		os.Exit(2)
	}
	assetKey := os.Getenv("LODOR_UPDATE_ASSET")
	if assetKey == "" {
		fmt.Fprintln(os.Stderr, "FATAL flag: --fetch-update needs LODOR_UPDATE_ASSET (the per-lane asset key, e.g. lodoros-miyoomini)")
		os.Exit(2)
	}
	ch := fetchChannel(channel)
	asset, ok := ch.Assets[assetKey]
	if !ok {
		fmt.Fprintf(os.Stderr, "fetch-update: channel %s has no asset %q\n", channel, assetKey)
		os.Exit(3)
	}
	// Bridge the update package's HONEST staging progress onto the same /tmp
	// side-channel the mirror/download-queue paths use, so the launcher renders
	// a real bar for this ~MB-over-slow-radio transfer instead of a static
	// "Downloading…". writePhase/writeProgress are best-effort (sidechannel.go).
	prog := func(phase string, pct int) {
		writePhase(phase)
		if pct >= 0 {
			writeProgress(pct)
		}
	}
	// lodor#42 pattern (armDownloadCancel), adapted: the update modes run before
	// any romm.Client exists, so the B-press sentinel is polled directly. Only an
	// INTERACTIVE caller (--cancellable) arms it — a daemon's fetch never polls
	// the shared sentinel, so a foreground B-press can't kill a background fetch.
	var cancel func() bool
	if cancellable {
		covercancel.Clear()
		cancel = covercancel.Requested
	}
	err := update.Stage(asset, update.StageDirName, ch.Version, fetchUpdateTimeout, prog, cancel)
	if err == update.ErrHashMismatch {
		fmt.Fprintln(os.Stderr, "fetch-update: artifact hash mismatch — staging removed, nothing applied")
		fmt.Printf("RESULT fetched=0 version=%s bytes=0\n", ch.Version)
		os.Exit(4)
	}
	if errors.Is(err, update.ErrCancelled) {
		// An honest user stop: nothing staged, partial kept for a Range resume.
		fmt.Fprintln(os.Stderr, "fetch-update: cancelled — partial download kept, the next attempt resumes")
		fmt.Printf("RESULT fetched=0 version=%s bytes=0 cancelled=1\n", ch.Version)
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch-update: %v (any partial download is kept — the next attempt resumes)\n", err)
		os.Exit(3)
	}
	fmt.Printf("RESULT fetched=1 version=%s bytes=%d\n", ch.Version, asset.Size)
	os.Exit(0)
}

// fetchChannel gets the manifest and resolves the channel, exiting 3 on any
// failure — to the calling shell an unreachable manifest and a useless one are
// the same "stay silent, try another day".
func fetchChannel(channel string) *update.Channel {
	m, err := update.FetchManifest(update.ManifestURL())
	if err != nil {
		fmt.Fprintf(os.Stderr, "update manifest: %v\n", err)
		os.Exit(3)
	}
	ch := m.ChannelFor(channel)
	if ch == nil || ch.Version == "" {
		fmt.Fprintf(os.Stderr, "update manifest: no usable %s channel\n", channel)
		os.Exit(3)
	}
	return ch
}

// updateChannel reads update_channel from settings.conf (CWD-relative, the
// launcher's file). Absent/unknown = stable — a background check must never
// fail over a missing toggle.
func updateChannel() string {
	data, err := os.ReadFile("settings.conf")
	if err != nil {
		return "stable"
	}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(k) == "update_channel" && strings.TrimSpace(v) == "beta" {
			return "beta"
		}
	}
	return "stable"
}

// oneLine flattens release notes for the single-line NOTES contract: parsers
// downstream are line-oriented and a multi-line stdout would corrupt them.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}
