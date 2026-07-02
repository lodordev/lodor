package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// Task #135: every Continue surface must emit what the CARD says, not what the index
// remembers — the index can lag the post-launch ✘→✓ rename (the Smart Pro 2026-07-03
// head pointed at "✘ Pokemon - Emerald Version … (RomM).gba" while the card held the
// "✓ " name, and one press on Continue answered "Nothing to continue yet").
func TestResolveOnCardRel(t *testing.T) {
	sd := t.TempDir()
	dir := filepath.Join(sd, "Roms", "Game Boy Advance (GBA)")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// exact path exists -> returned verbatim
	write("✘ Zelda.gba")
	if got, ok := resolveOnCardRel(sd, "/Roms/Game Boy Advance (GBA)/✘ Zelda.gba"); !ok || got != "/Roms/Game Boy Advance (GBA)/✘ Zelda.gba" {
		t.Fatalf("exact = %q ok=%v", got, ok)
	}

	// THE FIELD CASE: index says ✘, card renamed to ✓ -> the ✓ path is emitted
	write("✓ Emerald (RomM).gba")
	got, ok := resolveOnCardRel(sd, "/Roms/Game Boy Advance (GBA)/✘ Emerald (RomM).gba")
	if !ok || got != "/Roms/Game Boy Advance (GBA)/✓ Emerald (RomM).gba" {
		t.Fatalf("marker drift = %q ok=%v, want the ✓ variant", got, ok)
	}

	// bare on-card name (legacy/unmarked mirror) resolves too
	write("Metroid.gba")
	if got, ok = resolveOnCardRel(sd, "/Roms/Game Boy Advance (GBA)/✓ Metroid.gba"); !ok || got != "/Roms/Game Boy Advance (GBA)/Metroid.gba" {
		t.Fatalf("bare variant = %q ok=%v", got, ok)
	}

	// nothing on card under any marker -> not resolvable
	if got, ok = resolveOnCardRel(sd, "/Roms/Game Boy Advance (GBA)/✘ Ghost.gba"); ok {
		t.Fatalf("ghost resolved to %q, want miss", got)
	}
}
