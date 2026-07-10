package sync

// Unit tests for the save ledger file itself: upsert semantics, preservation
// of unknown/foreign lines, tolerant parsing, and the SaveTombstoned rule
// table. The pull/push integration is covered in pull_tombstone_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lodor/config"
	"lodor/romm"
)

func ledgerTestCfg(t *testing.T, deviceID string) *config.Config {
	t.Helper()
	t.Setenv("LODOR_PAK_DIR", t.TempDir())
	return &config.Config{Hosts: []config.Host{{DeviceID: deviceID}}}
}

func TestSaveLedgerUpsertAndLookup(t *testing.T) {
	cfg := ledgerTestCfg(t, "dev-a")
	t1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)

	if err := RecordSaveSynced(cfg, 1, "aaaa", 10, t1); err != nil {
		t.Fatal(err)
	}
	if err := RecordSaveSynced(cfg, 2, "cccc", 20, t1); err != nil {
		t.Fatal(err)
	}
	// Upsert: re-recording rom 1 REPLACES its row, never appends a second.
	if err := RecordSaveSynced(cfg, 1, "bbbb", 11, t2); err != nil {
		t.Fatal(err)
	}

	e, ok := LookupSyncedSave(cfg, 1)
	if !ok || e.MD5 != "bbbb" || e.SaveID != 11 || !e.UpdatedAt.Equal(t2) {
		t.Fatalf("rom 1 row = %+v ok=%v, want the upserted bbbb/11/t2", e, ok)
	}
	if e2, ok := LookupSyncedSave(cfg, 2); !ok || e2.MD5 != "cccc" {
		t.Fatalf("rom 2 row = %+v ok=%v, want cccc intact", e2, ok)
	}
	data, err := os.ReadFile(saveLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, line := range strings.Split(string(data), "\n") {
		if _, ok := parseSaveLedgerLine(line); ok {
			rows++
		}
	}
	if rows != 2 {
		t.Fatalf("ledger has %d parseable rows, want 2 (upsert must not append)\n%s", rows, data)
	}
}

func TestSaveLedgerPreservesUnknownAndForeignLines(t *testing.T) {
	cfg := ledgerTestCfg(t, "dev-a")
	pre := "# hand-written comment\n" +
		"999\tdev-b\tdddd\t42\t2026-07-01T00:00:00Z\n" + // foreign device — keep verbatim
		"this line does not parse\n"
	if err := os.MkdirAll(filepath.Dir(saveLedgerPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(saveLedgerPath(), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RecordSaveSynced(cfg, 1, "aaaa", 10, time.Now()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(saveLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# hand-written comment", "999\tdev-b\tdddd", "this line does not parse"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("rewrite dropped %q:\n%s", want, data)
		}
	}
	// The foreign row must not answer for THIS device.
	if _, ok := LookupSyncedSave(cfg, 999); ok {
		t.Fatal("foreign-device row answered a lookup for this device")
	}
}

func TestSaveTombstonedRules(t *testing.T) {
	tLedger := time.Date(2026, 7, 2, 14, 0, 0, 0, time.UTC)
	hash := "0123456789abcdef0123456789abcdef"
	otherHash := "fedcba9876543210fedcba9876543210"
	mk := func(h string, at time.Time) romm.Save {
		s := romm.Save{ID: 7, RomID: 5, UpdatedAt: at}
		if h != "" {
			s.ContentHash = &h
		}
		return s
	}

	cases := []struct {
		name      string
		seedMD5   string
		seedAt    time.Time
		newest    romm.Save
		tombstone bool
	}{
		{"hash match (case-insensitive) skips", strings.ToUpper(hash), tLedger, mk(hash, tLedger.Add(time.Hour)), true},
		{"equal updated_at skips", otherHash, tLedger, mk(hash, tLedger), true},
		{"older newest skips", otherHash, tLedger, mk(hash, tLedger.Add(-time.Hour)), true},
		{"strictly newer pulls", otherHash, tLedger, mk(hash, tLedger.Add(time.Hour)), false},
		{"zero ledger time + no hash match pulls", otherHash, time.Time{}, mk(hash, tLedger), false},
		{"hashless newest + newer time pulls", otherHash, tLedger, mk("", tLedger.Add(time.Hour)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ledgerTestCfg(t, "dev-a")
			seedLedger(t, cfg, 5, tc.seedMD5, 6, tc.seedAt)
			if got := SaveTombstoned(cfg, 5, tc.newest); got != tc.tombstone {
				t.Fatalf("SaveTombstoned = %v, want %v", got, tc.tombstone)
			}
		})
	}

	t.Run("no row never tombstones", func(t *testing.T) {
		cfg := ledgerTestCfg(t, "dev-a")
		if SaveTombstoned(cfg, 5, mk(hash, tLedger)) {
			t.Fatal("SaveTombstoned with no ledger row must be false")
		}
	})
}
