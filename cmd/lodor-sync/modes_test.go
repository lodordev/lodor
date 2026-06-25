package main

import (
	"testing"

	"lodor/romm"
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
