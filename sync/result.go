// Package sync implements the targeted, single-ROM save sync paths the Lodor
// engine drives (BLUEPRINT §2): push the device's local save(s) for one ROM up to
// RomM, pull the newest server save for one ROM down, and restore one explicit
// server save by id. It owes nothing to grout's code — grout is consulted only as a
// behavioral/wire oracle.
//
// Resolution of a ROM identity is done WITHOUT sqlite: the rom_id comes either from
// the catalog index (catalog.ResolveRomID over a local ROM path) or is supplied
// directly, then GetRom fetches the full record. Save discovery, hashing, and the
// non-destructive .bak/.tmp write dance are reimplemented here from first
// principles.
//
// CGO-free, stdlib only.
package sync

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"lodor/romm"
)

// ValidSaveExtensions is the set of file extensions (lower-case, dot-prefixed) that
// mark a file on the card as an emulator save eligible for sync (BLUEPRINT §2). A
// file whose extension is not in this set is never uploaded or matched to a ROM.
var ValidSaveExtensions = map[string]bool{
	".srm": true,
	".sav": true,
	".dsv": true,
	".mcr": true,
	".mcd": true,
	".brm": true,
	".eep": true,
	".sra": true,
	".fla": true,
	".mpk": true,
	".nv":  true,
}

// PushOutcome is the per-save result of a push attempt. It is the structured
// replacement for grout's bare uploaded-count return: every local save file the
// engine tried to push gets exactly one PushOutcome carrying WHY it ended where it
// did. This is the fix for the "pushed=0 total=2 stuck=2" invisibility bug — the
// --push-pending cmd mode (Phase 4) prints one line per outcome so a stuck save
// names its own cause instead of vanishing into an aggregate count.
type PushOutcome int

const (
	// OutcomeResolveFail: the ROM could not be resolved to a rom_id at all (the
	// local ROM path didn't reverse to a mapped platform, or the catalog index had
	// no entry / the server record couldn't be fetched). No save was attempted; the
	// SaveFile field is empty for this outcome.
	OutcomeResolveFail PushOutcome = iota
	// OutcomeNoLocalSave: the ROM resolved fine, but no matching save file was found
	// on the card for it. No upload was attempted. SaveFile is empty.
	OutcomeNoLocalSave
	// OutcomePushed: the save was uploaded to the server in this run (first attempt
	// or additive overwrite=true retry — Conflicted distinguishes them).
	OutcomePushed
	// OutcomeAlreadyOnServer: the upload errored, but a save with identical content
	// (MD5 == server content_hash) is already on the server, so the save is safe and
	// counts as done. This clears a "won't upload" save that's actually backed up.
	OutcomeAlreadyOnServer
	// OutcomeUploadError: the upload failed and the content is NOT already on the
	// server. Err carries the underlying error string. This is a genuinely stuck
	// save — the reason the --push-pending log can finally name.
	OutcomeUploadError
	// OutcomeHashMismatch is reserved for a future explicit local-vs-server content
	// verification step. It is defined so the cmd layer's switch is total and stable;
	// the current push path does not emit it (an unverifiable upload becomes
	// OutcomeUploadError instead).
	OutcomeHashMismatch
)

// String renders a PushOutcome as a short stable token for logs/CLI lines.
func (o PushOutcome) String() string {
	switch o {
	case OutcomeResolveFail:
		return "ResolveFail"
	case OutcomeNoLocalSave:
		return "NoLocalSave"
	case OutcomePushed:
		return "Pushed"
	case OutcomeAlreadyOnServer:
		return "AlreadyOnServer"
	case OutcomeUploadError:
		return "UploadError"
	case OutcomeHashMismatch:
		return "HashMismatch"
	default:
		return "Unknown"
	}
}

// PushResult is one row of a PushSaveDirect return: the outcome of a single local
// save file (or, for the resolve/no-save cases, of the ROM as a whole). The cmd
// layer prints one human line per PushResult.
//
//   - Outcome     — the enum above (always set).
//   - SaveFile    — basename of the local save file this row is about; empty for
//     OutcomeResolveFail and OutcomeNoLocalSave (no specific file).
//   - Emulator    — the emulator/save folder the file was found in (e.g. "GBA"),
//     empty when no file is involved.
//   - Conflicted  — true when OutcomePushed was reached via the additive
//     overwrite=true retry (a foreign-device save existed; ours was added alongside,
//     theirs preserved). Signals the caller may want to offer a pull.
//   - Err         — the underlying error string for OutcomeUploadError; empty
//     otherwise. Never contains the host (errors from the romm client are already
//     host-stripped at the call sites that matter; see cleanErr).
type PushResult struct {
	Outcome    PushOutcome
	SaveFile   string
	Emulator   string
	Conflicted bool
	Err        string
}

// Counts summarizes a slice of PushResults the way the --push-pending RESULT line
// needs it: pushed (OutcomePushed or OutcomeAlreadyOnServer — anything now safely on
// the server) vs total attempted save files vs stuck (everything not safe). The
// ResolveFail / NoLocalSave rows count toward total and stuck so a ROM that resolved
// but has no save, or didn't resolve at all, is visibly unaccounted-for rather than
// silently dropped.
func Counts(results []PushResult) (pushed, total, stuck int) {
	for _, r := range results {
		total++
		switch r.Outcome {
		case OutcomePushed, OutcomeAlreadyOnServer:
			pushed++
		default:
			stuck++
		}
	}
	return pushed, total, stuck
}

// Line renders a single PushResult as a one-line, host-free log entry suitable for
// the --push-pending cmd mode. Example:
//
//	Banjo-Pilot.gba.sav [GBA]: UploadError: network error — try again
//	Game (USA).gba.sav [GBA]: Pushed
func (r PushResult) Line() string {
	var b strings.Builder
	if r.SaveFile != "" {
		b.WriteString(r.SaveFile)
		if r.Emulator != "" {
			fmt.Fprintf(&b, " [%s]", r.Emulator)
		}
		b.WriteString(": ")
	}
	b.WriteString(r.Outcome.String())
	if r.Conflicted && r.Outcome == OutcomePushed {
		b.WriteString(" (conflict-resolved)")
	}
	if r.Err != "" {
		b.WriteString(": ")
		b.WriteString(r.Err)
	}
	return b.String()
}

// fileMD5 returns the lower-case hex MD5 of a file's bytes, matching RomM's
// content_hash. ok is false if the file can't be read.
func fileMD5(path string) (sum string, ok bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", false
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// isSaveConflict reports whether an upload error is RomM's foreign-device slot
// interlock — a newer save exists in the slot that this device hasn't pulled — where
// an additive overwrite=true retry is the right move (BLUEPRINT §2). A
// *romm.ConflictError always qualifies; otherwise the message is sniffed for the
// known fragments.
func isSaveConflict(err error) bool {
	if _, ok := err.(*romm.ConflictError); ok {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "newer save") ||
		strings.Contains(s, "conflict") ||
		strings.Contains(s, "409")
}

// cleanErr maps a raw client error to a short user-facing reason. Network/transport
// failures (the common case on flaky handheld WiFi) collapse to a generic message so
// the raw Go error and full URL — which could include the host — never reach a log
// line. Other errors are truncated to their first line. This keeps the host out of
// PushResult.Err.
func cleanErr(err error) string {
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"failed to execute request", "execute request", "connection",
		"timeout", "no such host", "eof", "reset", "refused", "deadline", "tls",
	} {
		if strings.Contains(s, frag) {
			return "network error — try again"
		}
	}
	return firstLine(err.Error())
}

// firstLine returns the first line of s, truncated to 80 runes-ish (bytes) to keep a
// log line bounded.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
