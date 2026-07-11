package syncstamp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAt(dir, 1700000000, 3, 2); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("stamp file missing: %v", err)
	}
	if got, want := string(b), "last_sync_ok=1700000000 saves=3 states=2\n"; got != want {
		t.Fatalf("stamp line = %q, want %q", got, want)
	}
	st, ok := Read(dir)
	if !ok || st.Epoch != 1700000000 || st.Saves != 3 || st.States != 2 {
		t.Fatalf("Read = %+v ok=%v", st, ok)
	}
}

func TestWriteLastWriterWins(t *testing.T) {
	dir := t.TempDir()
	if err := WriteAt(dir, 100, 1, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	if err := WriteAt(dir, 200, 0, 5); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	st, ok := Read(dir)
	if !ok || st.Epoch != 200 || st.Saves != 0 || st.States != 5 {
		t.Fatalf("Read after rewrite = %+v ok=%v", st, ok)
	}
}

func TestReadMissingAndGarbage(t *testing.T) {
	dir := t.TempDir()
	if _, ok := Read(dir); ok {
		t.Fatal("Read reported ok on a missing stamp")
	}
	for _, bad := range []string{"", "\n", "garbage\n", "last_sync_ok=zero saves=1\n", "last_sync_ok=-5\n"} {
		if err := os.WriteFile(filepath.Join(dir, FileName), []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if st, ok := Read(dir); ok {
			t.Fatalf("Read(%q) reported ok: %+v — a garbage stamp must render nothing, not a fake age", bad, st)
		}
	}
}

func TestReadIgnoresUnknownTokensAndExtraLines(t *testing.T) {
	dir := t.TempDir()
	line := "last_sync_ok=42 saves=1 states=0 future=stuff\nsecond line ignored\n"
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	st, ok := Read(dir)
	if !ok || st.Epoch != 42 || st.Saves != 1 || st.States != 0 {
		t.Fatalf("Read = %+v ok=%v", st, ok)
	}
}

func TestAge(t *testing.T) {
	for _, c := range []struct {
		delta int64
		want  string
	}{{30, "just now"}, {600, "10m ago"}, {7200, "2h ago"}, {200000, "2d ago"}, {-50, "just now"}} {
		st := Stamp{Epoch: 1000000}
		if got := st.Age(1000000 + c.delta); got != c.want {
			t.Fatalf("Age(delta=%d) = %q, want %q", c.delta, got, c.want)
		}
	}
	if !strings.HasSuffix(Path("/x"), "/x/"+FileName) {
		t.Fatalf("Path composes wrong: %s", Path("/x"))
	}
}
