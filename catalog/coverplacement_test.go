// CoverPlacement decision tests: the frontend-media (ES-DE) tree wins when
// configured — dimmed for stubs, bright for real files, recorded under the matching
// manifest kind — and the NextUI .media convention is byte-identical to before when
// it is not.
package catalog

import (
	"path/filepath"
	"testing"

	"lodor/config"
	"lodor/platform"
)

func esdeCfg(dir string) *config.Config {
	return &config.Config{FrontendMedia: &config.FrontendMediaConfig{Style: "esde", Dir: dir}}
}

func TestCoverPlacementDefaultNextUI(t *testing.T) {
	p := CoverPlacement(&config.Config{}, "/sd/Roms/GB/Tetris (USA).gb", true)
	want := filepath.Join("/sd/Roms/GB", ".media", "Tetris (USA).png")
	if p.Dest != want || p.Dim {
		t.Fatalf("got %+v want dest=%q dim=false (stub state must NOT dim the NextUI convention)", p, want)
	}
}

func TestCoverPlacementESDE(t *testing.T) {
	cfg := esdeCfg("/sd/ES-DE/downloaded_media")
	stub := CoverPlacement(cfg, "/sd/ROMs/gba/Golden Sun (USA).gba", true)
	wantStub := filepath.Join("/sd/ES-DE/downloaded_media", "gba", "covers", "Golden Sun (USA).png")
	if stub.Dest != wantStub || !stub.Dim {
		t.Fatalf("stub: got %+v want dest=%q dim=true", stub, wantStub)
	}
	real := CoverPlacement(cfg, "/sd/ROMs/gba/Golden Sun (USA).gba", false)
	if real.Dest != wantStub || real.Dim {
		t.Fatalf("real: got %+v want same dest, dim=false", real)
	}
}

func TestCoverKinds(t *testing.T) {
	cfg := esdeCfg("/m")
	dim := CoverPlacement(cfg, "/sd/ROMs/gba/G.gba", true)
	bright := CoverPlacement(cfg, "/sd/ROMs/gba/G.gba", false)
	if CoverKind(dim) != platform.ManifestCoverDim || CoverKind(bright) != platform.ManifestCover {
		t.Fatalf("kind mapping wrong: dim=%q bright=%q", CoverKind(dim), CoverKind(bright))
	}
	if otherCoverKind(dim) != platform.ManifestCover || otherCoverKind(bright) != platform.ManifestCoverDim {
		t.Fatalf("otherCoverKind mapping wrong")
	}
}
