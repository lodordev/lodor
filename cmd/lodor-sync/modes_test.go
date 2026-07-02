package main

import (
	"testing"

	"lodor/romm"
	"lodor/sync"
)

func TestIsBareM3U(t *testing.T) {
	cases := []struct {
		name string
		rom  romm.Rom
		want bool
	}{
		{"multi-disc m3u is NOT bare", romm.Rom{HasMultipleFiles: true, FsNameNoExt: "Game", Files: []romm.RomFile{{FileName: "Disc 1.chd"}}}, false},
		{"single-file bare m3u", romm.Rom{Files: []romm.RomFile{{FileName: "Game (USA).m3u"}}}, true},
		{"single-file bare m3u uppercase ext", romm.Rom{Files: []romm.RomFile{{FileName: "Game.M3U"}}}, true},
		{"single-file chd is not bare", romm.Rom{Files: []romm.RomFile{{FileName: "Game (USA).chd"}}}, false},
		{"no files but fs_extension m3u", romm.Rom{FsExtension: "m3u"}, true},
		{"no files but fs_extension chd", romm.Rom{FsExtension: "chd"}, false},
		{"normal gba", romm.Rom{Files: []romm.RomFile{{FileName: "Game.gba"}}}, false},
	}
	for _, c := range cases {
		if got := isBareM3U(c.rom); got != c.want {
			t.Errorf("%s: isBareM3U = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestStuckReasonTotal locks the launcher-facing stuck-save reasons: every
// non-landed outcome names its own cause (incl. the new integrity outcomes),
// and every landed outcome yields no line.
func TestStuckReasonTotal(t *testing.T) {
	cases := []struct {
		outcome   sync.PushOutcome
		wantEmpty bool
	}{
		{sync.OutcomeResolveFail, false},
		{sync.OutcomeNoLocalSave, false},
		{sync.OutcomeUploadError, false},
		{sync.OutcomeHashMismatch, false},   // verify failed → stays pending, named
		{sync.OutcomeEmptyLocalSave, false}, // ghost-proof guard → named
		{sync.OutcomePushed, true},
		{sync.OutcomeAlreadyOnServer, true},
	}
	for _, c := range cases {
		got := stuckReason(sync.PushResult{Outcome: c.outcome})
		if (got == "") != c.wantEmpty {
			t.Errorf("stuckReason(%s) = %q, wantEmpty=%v", c.outcome, got, c.wantEmpty)
		}
	}
}

// TestNewOutcomesStayStuck locks the pending-queue behavior: HashMismatch and
// EmptyLocalSave are NOT landed (the entry stays queued, never marked synced)
// and count as stuck.
func TestNewOutcomesStayStuck(t *testing.T) {
	results := []sync.PushResult{
		{Outcome: sync.OutcomeHashMismatch},
		{Outcome: sync.OutcomeEmptyLocalSave},
		{Outcome: sync.OutcomePushed},
	}
	if entryLanded(results) {
		t.Error("entryLanded = true with unverified/empty saves — they would be dequeued unsynced")
	}
	pushed, total, stuck := sync.Counts(results)
	if pushed != 1 || total != 3 || stuck != 2 {
		t.Errorf("Counts = (%d,%d,%d), want (1,3,2)", pushed, total, stuck)
	}
}
