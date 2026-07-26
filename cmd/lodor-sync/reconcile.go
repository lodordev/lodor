package main

import (
	"fmt"
	"os"

	"lodor/config"
	"lodor/romm"
	"lodor/sync"
)

// runReconcileLibrary drives ONE managed-sync reconcile pass (decision 2026-07-16): it
// builds the whole-library local save inventory, negotiates a sync session with RomM
// 5.0.0, executes the returned plan against the verified push/pull primitives, and
// closes the session with the achieved counts. This is the on-power background reconcile
// verb the CFW syncd loop and the Android foreground service will call; running it by
// hand is also the live smoke test for the managed-sync stack.
//
// Prints: RESULT reconciled=<completed> planned=<P> failed=<F> conflicts=<C>
// skipped=<S> session=<id>. Exit codes mirror the other server-touching modes: 0 ok,
// 2 server too old for managed sync, 3 unreachable/negotiate error, 6 pairing expired.
//
// With dryRun set, it stops after negotiate: it builds the inventory, opens a session,
// prints the plan (one PLAN line per operation + the server totals), then closes the
// session with a 0/0 complete WITHOUT moving a single save byte. The live smoke test for
// the managed-sync stack — proves auth, the wire, and that a real card produces a sane
// plan, betting nothing.
func runReconcileLibrary(client *romm.Client, cfg *config.Config, dryRun bool) {
	writeProgress(0)
	writePhase("Reconciling saves…")

	if !client.SupportsManagedSync() {
		writeProgress(100)
		writePhase("Server too old for managed sync")
		fmt.Printf("RESULT reconciled=0 planned=0 failed=0 conflicts=0 skipped=0 session=0 supported=0 version=%s\n", client.ServerVersion())
		exitMode(2)
	}

	if dryRun {
		runReconcileDryRun(client, cfg)
		return
	}

	res, err := sync.ReconcileLibrary(client, cfg)
	if err != nil {
		noteAuthErr(err)
		writeProgress(0)
		if pairingExpired {
			writePhase("Pairing expired — re-pair this device")
			fmt.Fprintf(os.Stderr, "FATAL reconcile-library: %s\n", safeErr(err))
			// Partial counts may still be worth reporting when execution aborted mid-run.
			fmt.Printf("RESULT reconciled=%d planned=%d failed=%d conflicts=%d skipped=%d session=%d\n",
				res.Completed, res.Planned, res.Failed, res.Conflicts, res.Skipped, res.SessionID)
			exitMode(6)
		}
		writePhase("Couldn't complete sync")
		fmt.Fprintf(os.Stderr, "FATAL reconcile-library: %s\n", safeErr(err))
		exitMode(3)
	}

	for _, n := range res.Notes {
		fmt.Fprintf(os.Stderr, "reconcile-library: %s\n", n)
	}
	writeProgress(100)
	writePhase("Saves reconciled")
	fmt.Printf("RESULT reconciled=%d planned=%d failed=%d conflicts=%d skipped=%d session=%d\n",
		res.Completed, res.Planned, res.Failed, res.Conflicts, res.Skipped, res.SessionID)
	exitMode(0)
}

// runReconcileDryRun negotiates a plan and prints it without executing, then closes the
// opened session 0/0 so it never dangles server-side. Moves no save bytes.
func runReconcileDryRun(client *romm.Client, cfg *config.Config) {
	resp, invCount, err := sync.NegotiatePlan(client, cfg)
	if err != nil {
		noteAuthErr(err)
		writeProgress(0)
		if pairingExpired {
			writePhase("Pairing expired — re-pair this device")
			fmt.Fprintf(os.Stderr, "FATAL reconcile-library --dry-run: %s\n", safeErr(err))
			exitMode(6)
		}
		writePhase("Couldn't reach RomM")
		fmt.Fprintf(os.Stderr, "FATAL reconcile-library --dry-run: %s\n", safeErr(err))
		exitMode(3)
	}

	// One PLAN line per operation (to stderr so RESULT stays the single stdout summary).
	for _, op := range resp.Operations {
		fmt.Fprintf(os.Stderr, "PLAN %s rom=%d save=%s slot=%s file=%q reason=%q\n",
			op.Action, op.RomID, saveIDStr(op.SaveID), ptrStr(op.Slot), op.FileName, op.Reason)
	}

	// Close the session we opened; a 0/0 complete is the "did nothing" report and moves
	// no bytes. Best-effort — a failure here doesn't invalidate the dry run's findings.
	closed := 1
	if _, cerr := client.CompleteSyncSession(resp.SessionID, 0, 0); cerr != nil {
		closed = 0
		fmt.Fprintf(os.Stderr, "reconcile-library --dry-run: session close failed: %s\n", safeErr(cerr))
	}

	writeProgress(100)
	writePhase("Dry run complete")
	fmt.Printf("RESULT dryrun=1 inventory=%d planned=%d upload=%d download=%d conflict=%d noop=%d session=%d closed=%d\n",
		invCount, resp.TotalUpload+resp.TotalDownload+resp.TotalConflict,
		resp.TotalUpload, resp.TotalDownload, resp.TotalConflict, resp.TotalNoOp, resp.SessionID, closed)
	exitMode(0)
}

func saveIDStr(p *int) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *p)
}

func ptrStr(p *string) string {
	if p == nil || *p == "" {
		return "-"
	}
	return *p
}
