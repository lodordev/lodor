package sync

// Deleted-save tombstone tests (the Argosy 2.0 gap): a battery save DELETED on
// this device after a sync must not resurrect on the next pull — while a
// strictly NEWER server revision (another device advanced the game) must still
// arrive, a fresh card must behave exactly as before the ledger existed, and
// every local-file-EXISTS path must stay byte-identical to the lineage logic.
// Reuses the content-hash lineage scaffolding (lineageServer / lineageEnv):
// rom 1234 with save 2 = newest (14:00) and save 1 = older (04:00).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lodor/config"
	"lodor/romm"
)

var (
	tombNewestAt = time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC) // save 2 (lineageServer)
	tombOlderAt  = time.Date(2026, 7, 2, 4, 0, 0, 0, time.UTC)  // save 1 (lineageServer)
)

func seedLedger(t *testing.T, cfg *config.Config, romID int, md5sum string, saveID int, at time.Time) {
	t.Helper()
	if err := RecordSaveSynced(cfg, romID, md5sum, saveID, at); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
}

// (a) local delete + server newest == the ledgered revision → SKIPPED, reason
// tombstone, and nothing is written to the card.
func TestTombstoneSkipsDeletedSave(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	seedLedger(t, cfg, 1234, md5Hex(newestBytes), 2, tombNewestAt)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullTombstoned || res.Reason != "tombstone" {
		t.Fatalf("outcome=%s reason=%q, want Tombstoned/tombstone", res.Outcome, res.Reason)
	}
	if res.Pulled() {
		t.Fatal("a tombstone skip must not report pulled")
	}
	if _, err := os.Stat(localSavePath()); err == nil {
		t.Fatal("THE DELETED SAVE WAS RESURRECTED: local file exists after a tombstone skip")
	}
}

// (a, ordering rule) even when the newest revision's hash does NOT match the
// ledgered one, updated_at <= the ledgered updated_at means the server has
// nothing newer than what this device already held and deleted → skip.
func TestTombstoneUpdatedAtRuleSkips(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	seedLedger(t, cfg, 1234, "ffffffffffffffffffffffffffffffff", 2, tombNewestAt) // hash never matches; time equal

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullTombstoned || res.Reason != "tombstone" {
		t.Fatalf("outcome=%s reason=%q, want Tombstoned/tombstone via updated_at ordering", res.Outcome, res.Reason)
	}
}

// (b) local delete + server has a STRICTLY NEWER revision than the ledger →
// pull it (another device advanced the game — resurrection is the feature),
// and the ledger advances to the pulled revision.
func TestTombstoneNewerServerRevisionPulls(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	seedLedger(t, cfg, 1234, md5Hex(olderBytes), 1, tombOlderAt) // last synced = the OLDER revision

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "no-local" {
		t.Fatalf("outcome=%s reason=%q, want Written/no-local (newer remote must arrive)", res.Outcome, res.Reason)
	}
	got, err := os.ReadFile(localSavePath())
	if err != nil || string(got) != string(newestBytes) {
		t.Fatalf("local save = %q err=%v, want newest bytes", got, err)
	}
	e, ok := LookupSyncedSave(cfg, 1234)
	if !ok || e.SaveID != 2 || !strings.EqualFold(e.MD5, md5Hex(newestBytes)) || !e.UpdatedAt.Equal(tombNewestAt) {
		t.Fatalf("ledger after pull = %+v ok=%v, want save 2 / newest hash / newest time", e, ok)
	}
}

// (c) no ledger row at all (fresh card / reinstall / pre-tombstone history) →
// pull exactly as today.
func TestTombstoneNoLedgerFreshCardPulls(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "no-local" {
		t.Fatalf("outcome=%s reason=%q, want Written/no-local (fresh-card behavior unchanged)", res.Outcome, res.Reason)
	}
}

// (f) a corrupt ledger file degrades to no-record → pull as today (fail-open
// toward current behavior), never to a wrong skip or an error.
func TestTombstoneCorruptLedgerFailsOpen(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	if err := os.MkdirAll(filepath.Dir(saveLedgerPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	garbage := []byte("\x00\xff\xfenot a ledger\n1234\tbroken\nrubbish line with no tabs\n")
	if err := os.WriteFile(saveLedgerPath(), garbage, 0o644); err != nil {
		t.Fatal(err)
	}

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "no-local" {
		t.Fatalf("outcome=%s reason=%q, want Written/no-local (corrupt ledger must fail open)", res.Outcome, res.Reason)
	}
}

// Ledger rows are DEVICE-scoped: another profile's/device's row must never
// tombstone this device's pull.
func TestTombstoneOtherDeviceRowIgnored(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	other := &config.Config{Hosts: []config.Host{{DeviceID: "someone-elses-handheld"}}}
	seedLedger(t, other, 1234, md5Hex(newestBytes), 2, tombNewestAt)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "no-local" {
		t.Fatalf("outcome=%s reason=%q, want Written/no-local (foreign-device row must not tombstone)", res.Outcome, res.Reason)
	}
}

// --include-deleted (PullOptions.IncludeDeleted): the explicit escape hatch
// bypasses an armed tombstone.
func TestIncludeDeletedBypassesTombstone(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	seedLedger(t, cfg, 1234, md5Hex(newestBytes), 2, tombNewestAt)

	res := PullSaveDirectOpts(client, cfg, romPath, PullOptions{IncludeDeleted: true})
	if res.Outcome != PullWritten || res.Reason != "no-local" {
		t.Fatalf("outcome=%s reason=%q, want Written/no-local under IncludeDeleted", res.Outcome, res.Reason)
	}
	got, _ := os.ReadFile(localSavePath())
	if string(got) != string(newestBytes) {
		t.Fatalf("local save = %q, want newest bytes", got)
	}
}

// (e) the explicit restore path never consults the tombstone — user intent
// resurrects — and the restored revision becomes the new ledger baseline.
func TestRestoreSaveBypassesTombstone(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	seedLedger(t, cfg, 1234, md5Hex(newestBytes), 2, tombNewestAt)

	nh := md5Hex(newestBytes)
	chosen := romm.Save{ID: 2, RomID: 1234, FileName: "Sonic (USA).gba.sav", FileExtension: "gba.sav",
		FileSizeBytes: int64(len(newestBytes)), ContentHash: &nh, UpdatedAt: tombNewestAt}
	res := RestoreSave(client, cfg, romPath, chosen)
	if res.Outcome != PullWritten {
		t.Fatalf("outcome=%s, want Written (explicit restore must bypass the tombstone)", res.Outcome)
	}
	got, err := os.ReadFile(localSavePath())
	if err != nil || string(got) != string(newestBytes) {
		t.Fatalf("local save = %q err=%v, want restored newest bytes", got, err)
	}
	e, ok := LookupSyncedSave(cfg, 1234)
	if !ok || e.SaveID != 2 {
		t.Fatalf("ledger after restore = %+v ok=%v, want the restored revision recorded", e, ok)
	}
}

// (d, pull leg) a successful pull writes the ledger row.
func TestLedgerWrittenOnPull(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)

	if res := PullSaveDirect(client, cfg, romPath); res.Outcome != PullWritten {
		t.Fatalf("outcome=%s, want Written", res.Outcome)
	}
	e, ok := LookupSyncedSave(cfg, 1234)
	if !ok {
		t.Fatal("no ledger row after a successful pull")
	}
	if e.SaveID != 2 || !strings.EqualFold(e.MD5, md5Hex(newestBytes)) || !e.UpdatedAt.Equal(tombNewestAt) {
		t.Fatalf("ledger row = %+v, want save 2 / newest hash / newest updated_at", e)
	}
}

// An in-sync no-op is a synced moment too: the ledger refreshes so a later
// delete tombstones against the revision the device verifiably held.
func TestLedgerWrittenOnInSync(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	writeLocalSave(t, newestBytes)

	if res := PullSaveDirect(client, cfg, romPath); res.Outcome != PullInSync {
		t.Fatalf("outcome=%s, want InSync", res.Outcome)
	}
	if e, ok := LookupSyncedSave(cfg, 1234); !ok || e.SaveID != 2 {
		t.Fatalf("ledger row = %+v ok=%v, want the in-sync newest revision recorded", e, ok)
	}
}

// tombPushServer: one rom, ZERO existing saves; POST /api/saves accepts the
// upload and answers with a tier-1-verifiable record (content_hash of the
// uploaded local bytes + non-zero size), so uploadVerified reaches
// OutcomePushed without extra fetches.
func tombPushServer(t *testing.T, localHash string, size int64) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/roms/1234", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(romm.Rom{
			ID: 1234, PlatformFsSlug: "gba",
			FsName: "Sonic (USA).gba", FsNameNoExt: "Sonic (USA)",
			Files: []romm.RomFile{{FileName: "Sonic (USA).gba"}},
		})
	})
	mux.HandleFunc("/api/saves", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(romm.Save{
				ID: 99, RomID: 1234, FileName: "Sonic (USA).gba.sav", FileExtension: "gba.sav",
				FileSizeBytes: size, ContentHash: &localHash, UpdatedAt: tombNewestAt.Add(2 * time.Hour),
			})
			return
		}
		_ = json.NewEncoder(w).Encode([]romm.Save{})
	})
	return httptest.NewServer(mux)
}

// (d, push leg) a VERIFIED upload writes the ledger row with the created
// server revision.
func TestLedgerWrittenOnPush(t *testing.T) {
	hash := md5Hex(localOnly)
	srv := tombPushServer(t, hash, int64(len(localOnly)))
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	writeLocalSave(t, localOnly)

	results := PushSaveDirect(client, cfg, romPath)
	if len(results) != 1 || results[0].Outcome != OutcomePushed {
		t.Fatalf("results = %+v, want exactly one Pushed", results)
	}
	e, ok := LookupSyncedSave(cfg, 1234)
	if !ok || e.SaveID != 99 || !strings.EqualFold(e.MD5, hash) {
		t.Fatalf("ledger row = %+v ok=%v, want the created save 99 with the local hash", e, ok)
	}
}

// (d, push leg — dedup flavor) an AlreadyOnServer dedup is a synced moment:
// the matched (newest) server revision seeds the ledger without any upload.
func TestLedgerWrittenOnDedupPush(t *testing.T) {
	srv := lineageServer(t) // no POST handler — an actual upload would 404
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	writeLocalSave(t, newestBytes)

	results := PushSaveDirect(client, cfg, romPath)
	if len(results) != 1 || results[0].Outcome != OutcomeAlreadyOnServer {
		t.Fatalf("results = %+v, want exactly one AlreadyOnServer", results)
	}
	e, ok := LookupSyncedSave(cfg, 1234)
	if !ok || e.SaveID != 2 || !strings.EqualFold(e.MD5, md5Hex(newestBytes)) {
		t.Fatalf("ledger row = %+v ok=%v, want the matched newest revision (save 2)", e, ok)
	}
}

// (g) LOCAL FILE EXISTS — the tombstone must never change any of these paths.
// An armed tombstone row + a local file matching the OLDER revision must still
// produce today's older-lineage pull, byte for byte (.bak kept).
func TestTombstoneLocalOlderLineageUnchanged(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	seedLedger(t, cfg, 1234, md5Hex(newestBytes), 2, tombNewestAt) // armed — must be ignored
	p := writeLocalSave(t, olderBytes)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullWritten || res.Reason != "older-lineage" {
		t.Fatalf("outcome=%s reason=%q, want Written/older-lineage (local-exists path unchanged)", res.Outcome, res.Reason)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(newestBytes) {
		t.Fatalf("local save = %q, want newest bytes", got)
	}
	bak, err := os.ReadFile(p + ".bak")
	if err != nil || string(bak) != string(olderBytes) {
		t.Fatalf(".bak = %q err=%v, want the pre-pull local bytes", bak, err)
	}
}

// (g) unpushed local progress with an armed tombstone row: still never
// overwritten — identical to today.
func TestTombstoneLocalUnpushedUnchanged(t *testing.T) {
	srv := lineageServer(t)
	defer srv.Close()
	client, cfg, romPath := lineageEnv(t, srv.URL)
	seedLedger(t, cfg, 1234, md5Hex(newestBytes), 2, tombNewestAt) // armed — must be ignored
	p := writeLocalSave(t, localOnly)

	res := PullSaveDirect(client, cfg, romPath)
	if res.Outcome != PullLocalUnpushed || res.Reason != "unpushed-local" {
		t.Fatalf("outcome=%s reason=%q, want LocalUnpushed/unpushed-local (local-exists path unchanged)", res.Outcome, res.Reason)
	}
	got, _ := os.ReadFile(p)
	if string(got) != string(localOnly) {
		t.Fatalf("UNPUSHED LOCAL PROGRESS WAS OVERWRITTEN: %q", got)
	}
}
