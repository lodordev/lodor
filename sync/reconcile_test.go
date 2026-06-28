package sync

import "testing"

// TestReconcileTable exercises every branch of the 3-way decision table with explicit,
// human-checked expectations. Hashes are single letters standing for distinct content;
// "" means absent.
func TestReconcileTable(t *testing.T) {
	tests := []struct {
		name string
		in   ReconcileInput
		want Decision
	}{
		// Override signals (highest priority).
		{"explicit-restore beats everything", ReconcileInput{LocalHash: "A", ServerHash: "B", AnchorHash: "B", ExplicitRestore: true}, DecisionKeepLocal},
		{"pending-upload beats server-moved", ReconcileInput{LocalHash: "A", ServerHash: "B", AnchorHash: "A", PendingUpload: true}, DecisionKeepLocal},
		{"pending-upload with no server save", ReconcileInput{LocalHash: "A", PendingUpload: true}, DecisionKeepLocal},

		// Absence cases.
		{"nothing anywhere", ReconcileInput{}, DecisionInSync},
		{"local only -> push", ReconcileInput{LocalHash: "A"}, DecisionKeepLocal},
		{"server only -> pull", ReconcileInput{ServerHash: "A"}, DecisionKeepServer},
		{"local only, anchor stale", ReconcileInput{LocalHash: "A", AnchorHash: "X"}, DecisionKeepLocal},
		{"server only, anchor stale", ReconcileInput{ServerHash: "A", AnchorHash: "X"}, DecisionKeepServer},

		// Identical.
		{"identical no anchor", ReconcileInput{LocalHash: "A", ServerHash: "A"}, DecisionInSync},
		{"identical with anchor", ReconcileInput{LocalHash: "A", ServerHash: "A", AnchorHash: "A"}, DecisionInSync},
		{"identical, anchor differs (both moved to same)", ReconcileInput{LocalHash: "A", ServerHash: "A", AnchorHash: "B"}, DecisionInSync},

		// Both present, different, with anchor.
		{"local==anchor, server moved -> pull", ReconcileInput{LocalHash: "A", ServerHash: "B", AnchorHash: "A"}, DecisionKeepServer},
		{"server==anchor, local moved -> push", ReconcileInput{LocalHash: "A", ServerHash: "B", AnchorHash: "B"}, DecisionKeepLocal},
		{"both moved differently -> conflict", ReconcileInput{LocalHash: "A", ServerHash: "B", AnchorHash: "C"}, DecisionConflict},

		// Both present, different, NO anchor -> conflict (never clobber).
		{"different, no anchor -> conflict", ReconcileInput{LocalHash: "A", ServerHash: "B"}, DecisionConflict},

		// Case-insensitive hash handling.
		{"case-insensitive equal", ReconcileInput{LocalHash: "abc", ServerHash: "ABC"}, DecisionInSync},
		{"case-insensitive anchor match -> pull", ReconcileInput{LocalHash: "abc", ServerHash: "B", AnchorHash: "ABC"}, DecisionKeepServer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Reconcile(tt.in); got != tt.want {
				t.Errorf("Reconcile(%+v) = %s, want %s", tt.in, got, tt.want)
			}
		})
	}
}

// TestReconcileNeverClobberInvariant is the load-bearing safety proof: across EVERY
// combination of local/server/anchor content (no override signals), the only time the
// reconciler returns KEEP_SERVER — the decision that overwrites the local save file —
// is when the local content is NOT a unique un-synced state, i.e. local is absent, OR
// local equals the anchor (unchanged since last sync), OR local equals the server
// (already identical). It must NEVER overwrite local work that diverged from both the
// anchor and the server.
func TestReconcileNeverClobberInvariant(t *testing.T) {
	alphabet := []string{"", "A", "B", "C"}
	for _, local := range alphabet {
		for _, server := range alphabet {
			for _, anchor := range alphabet {
				in := ReconcileInput{LocalHash: local, ServerHash: server, AnchorHash: anchor}
				d := Reconcile(in)
				if d == DecisionKeepServer {
					safe := local == "" || eqHash(local, anchor) || eqHash(local, server)
					if !safe {
						t.Errorf("CLOBBER RISK: Reconcile(local=%q server=%q anchor=%q) = KeepServer but local is a unique un-synced state", local, server, anchor)
					}
				}
				// Symmetric sanity: a genuine two-sided divergence must surface as conflict,
				// never silently resolve.
				if local != "" && server != "" && !eqHash(local, server) {
					localMoved := !eqHash(local, anchor)
					serverMoved := !eqHash(server, anchor)
					if localMoved && serverMoved && d != DecisionConflict {
						t.Errorf("Reconcile(local=%q server=%q anchor=%q) = %s, want Conflict (both moved)", local, server, anchor, d)
					}
				}
			}
		}
	}
}

// TestBootstrapAnchorHash covers promoting the already-on-server signal into an anchor.
func TestBootstrapAnchorHash(t *testing.T) {
	tests := []struct {
		name    string
		local   string
		history []string
		want    string
	}{
		{"local in history -> anchor is local", "A", []string{"X", "A", "Y"}, "A"},
		{"local is newest -> anchor is local", "A", []string{"A"}, "A"},
		{"local not in history -> no anchor", "A", []string{"X", "Y"}, ""},
		{"no local -> no anchor", "", []string{"A"}, ""},
		{"case-insensitive membership", "abc", []string{"ABC"}, "abc"},
		{"empty history -> no anchor", "A", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BootstrapAnchorHash(tt.local, tt.history); got != tt.want {
				t.Errorf("BootstrapAnchorHash(%q,%v) = %q, want %q", tt.local, tt.history, got, tt.want)
			}
		})
	}
}

// TestBootstrapPreventsFirstSyncConflict shows the bootstrap fix in action: an existing
// card whose local save matches an OLDER server revision (server advanced) must PULL,
// not CONFLICT, even with no stored anchor.
func TestBootstrapPreventsFirstSyncConflict(t *testing.T) {
	local := "OLD"
	serverNewest := "NEW"
	history := []string{"NEW", "OLD"} // server kept the old revision the device still holds
	anchor := BootstrapAnchorHash(local, history)
	got := Reconcile(ReconcileInput{LocalHash: local, ServerHash: serverNewest, AnchorHash: anchor})
	if got != DecisionKeepServer {
		t.Fatalf("bootstrap first-sync: got %s, want KeepServer (server merely advanced past a held revision)", got)
	}
	// Whereas a TRULY divergent local (never on server) with no anchor must conflict.
	anchor2 := BootstrapAnchorHash("MINE", history)
	if got := Reconcile(ReconcileInput{LocalHash: "MINE", ServerHash: serverNewest, AnchorHash: anchor2}); got != DecisionConflict {
		t.Fatalf("divergent first-sync: got %s, want Conflict", got)
	}
}
