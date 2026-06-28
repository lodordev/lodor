package sync

import "strings"

// Anchor-based 3-way save reconciliation (Argosy research #1) — the save-sync MOAT.
//
// The deployed engine pulls newest-wins (server updated_at vs local mtime, see
// PullSaveDirect), which can SILENTLY OVERWRITE a locally-advanced save when the server
// copy merely has a later timestamp. This reconciler replaces that with a git-style
// 3-way compare against a per-rom ANCHOR — the content state local and server agreed on
// at the last successful sync (the common ancestor). The cardinal rule: when BOTH sides
// advanced from the anchor to DIFFERENT content, we NEVER pick a winner — we surface a
// conflict and overwrite nothing.

// Decision is the outcome of a 3-way reconcile for ONE rom.
type Decision int

const (
	// DecisionInSync: local and server hold identical content (or both are empty) —
	// no transfer needed; the caller refreshes the anchor.
	DecisionInSync Decision = iota
	// DecisionKeepLocal: the local save is authoritative — PUSH it to the server.
	DecisionKeepLocal
	// DecisionKeepServer: the server save is authoritative — PULL it to the device.
	DecisionKeepServer
	// DecisionConflict: both sides advanced from the common ancestor to DIFFERENT
	// content. Never auto-resolved — surfaced to the user; nothing is overwritten.
	DecisionConflict
)

// String renders a Decision as a short stable token for logs/contracts.
func (d Decision) String() string {
	switch d {
	case DecisionInSync:
		return "InSync"
	case DecisionKeepLocal:
		return "KeepLocal"
	case DecisionKeepServer:
		return "KeepServer"
	case DecisionConflict:
		return "Conflict"
	default:
		return "Unknown"
	}
}

// ReconcileInput is the full state a 3-way reconcile needs for ONE rom. All hashes are
// lower-case MD5 of the raw save bytes (== RomM content_hash == local fileMD5). An
// empty hash means "absent" (no local save / no server save / no anchor).
type ReconcileInput struct {
	LocalHash  string // MD5 of the bytes on the card; "" = no local save
	ServerHash string // MD5 of the newest server save; "" = no server save
	AnchorHash string // MD5 recorded at the last successful sync; "" = no anchor

	// PendingUpload: this rom is in our pending-saves.txt — a local change we have not
	// yet pushed. By our own record the local bytes are newer; forces KEEP_LOCAL.
	PendingUpload bool
	// ExplicitRestore: the user explicitly chose a flashback/restore of these local
	// bytes — KEEP_LOCAL (they deliberately picked this version).
	ExplicitRestore bool
}

// Reconcile applies the 3-way decision table. It is a PURE function (no I/O) so every
// branch — especially the never-clobber invariant — is unit-testable in isolation.
//
// Decision table (first matching rule wins):
//
//	explicit-restore OR pending-upload .................... KEEP_LOCAL
//	no local AND no server ............................... IN_SYNC
//	local only (no server) ............................... KEEP_LOCAL   (push; destroys nothing)
//	server only (no local) ............................... KEEP_SERVER  (pull; destroys nothing)
//	local == server ...................................... IN_SYNC
//	-- both present and different, decide via the anchor --
//	no anchor ............................................ CONFLICT     (unknown ancestor; never clobber)
//	local == anchor, server moved ........................ KEEP_SERVER  (server is ahead)
//	server == anchor, local moved ........................ KEEP_LOCAL   (local is ahead)
//	both moved (to different content) .................... CONFLICT
func Reconcile(in ReconcileInput) Decision {
	// (1) Explicit local-authority signals override the hash comparison entirely.
	if in.ExplicitRestore || in.PendingUpload {
		return DecisionKeepLocal
	}

	local := in.LocalHash != ""
	server := in.ServerHash != ""

	// (2) Absence cases — pushing/pulling into emptiness can never destroy data.
	switch {
	case !local && !server:
		return DecisionInSync
	case local && !server:
		return DecisionKeepLocal
	case !local && server:
		return DecisionKeepServer
	}

	// (3) Both present and identical → already in sync.
	if eqHash(in.LocalHash, in.ServerHash) {
		return DecisionInSync
	}

	// (4) Both present and DIFFERENT → use the anchor (common ancestor) to decide.
	hasAnchor := in.AnchorHash != ""
	localMoved := !eqHash(in.LocalHash, in.AnchorHash)
	serverMoved := !eqHash(in.ServerHash, in.AnchorHash)

	switch {
	case !hasAnchor:
		// No common ancestor and the two sides differ: we cannot prove which derives
		// from which, so we NEVER clobber — surface a conflict. (This is stricter than
		// newest-wins on purpose: a divergent state with no anchor is exactly where
		// timestamp-wins silently destroyed the loser.)
		return DecisionConflict
	case !localMoved && serverMoved:
		return DecisionKeepServer
	case localMoved && !serverMoved:
		return DecisionKeepLocal
	default:
		// Both moved away from the anchor to different content → real conflict.
		return DecisionConflict
	}
}

// eqHash reports case-insensitive equality of two NON-EMPTY hashes. An empty hash never
// equals anything (absence is not a match).
func eqHash(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(a, b)
}

// containsFold reports whether set contains h (case-insensitive, non-empty).
func containsFold(set []string, h string) bool {
	if h == "" {
		return false
	}
	for _, s := range set {
		if strings.EqualFold(s, h) {
			return true
		}
	}
	return false
}

// BootstrapAnchorHash derives an effective anchor for a rom that has NO stored anchor
// yet, using the server's save HISTORY as evidence (promoting our existing
// already-on-server signal into the anchor model — research #1). When the local bytes
// match ANY server revision (current or older), those bytes are provably a previously
// synced state, i.e. a valid common ancestor — so we return the local hash as the
// anchor. This lets the reconciler safely pick KEEP_SERVER when the server has merely
// advanced past a revision the device already holds, instead of crying conflict on the
// very first sync of an existing card.
//
// Returns "" when no bootstrap ancestor can be established (the local bytes were never
// on the server), in which case a divergent server save correctly yields a conflict.
func BootstrapAnchorHash(localHash string, serverHistory []string) string {
	if localHash != "" && containsFold(serverHistory, localHash) {
		return localHash
	}
	return ""
}
