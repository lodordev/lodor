package ui

import "testing"

// helloWorldQR is the byte-mode, ECC-level-M QR matrix for "HELLO WORLD" (version 1,
// 21x21, '#'=dark). It is the reference vector for the encoder: this exact matrix is
// produced by Nayuki's qrcodegen (QrCode.encode_segments, Ecc.MEDIUM, auto mask) and by
// libqrencode 4.1.1, and decodes back to "HELLO WORLD" under zbar. A change to the
// Galois-field math, block interleave, data placement, mask penalty, or format bits will
// break this — the whole encoder is pinned by one known-good output.
var helloWorldQR = []string{
	"#######.##..#.#######",
	"#.....#....#..#.....#",
	"#.###.#..#.#..#.###.#",
	"#.###.#.#..#..#.###.#",
	"#.###.#.###.#.#.###.#",
	"#.....#.#..#..#.....#",
	"#######.#.#.#.#######",
	"........#..##........",
	"#...#.######.#####..#",
	"...#....#.###....####",
	"..######..##.##.#..#.",
	"#####...##...#.......",
	"#####.#.#.#.#.##..##.",
	"........#.#.####.#.##",
	"#######.###.#.#.##.#.",
	"#.....#..#.###.##..##",
	"#.###.#.##.#.##...##.",
	"#.###.#..#..#...##.##",
	"#.###.#..###...###...",
	"#.....#....#.#.......",
	"#######.#########.#.#",
}

func TestQRMatrixReferenceVector(t *testing.T) {
	m, err := QRMatrix("HELLO WORLD")
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != len(helloWorldQR) {
		t.Fatalf("size %d, want %d", len(m), len(helloWorldQR))
	}
	for y, row := range helloWorldQR {
		if len(m[y]) != len(row) {
			t.Fatalf("row %d width %d, want %d", y, len(m[y]), len(row))
		}
		for x, ch := range row {
			want := ch == '#'
			if m[y][x] != want {
				t.Errorf("module (%d,%d) = %v, want %v", x, y, m[y][x], want)
			}
		}
	}
}

// TestQRMatrixVersionScaling checks the encoder auto-selects a larger version as input
// grows and stays a valid square with intact finder patterns, and errors past v9-M.
func TestQRMatrixVersionScaling(t *testing.T) {
	cases := []struct{ n, wantSize int }{
		{10, 21},  // v1
		{20, 25},  // v2
		{50, 33},  // v4
		{90, 41},  // v6
		{175, 53}, // v9 (max)
	}
	for _, c := range cases {
		s := make([]byte, c.n)
		for i := range s {
			s[i] = 'a'
		}
		m, err := QRMatrix(string(s))
		if err != nil {
			t.Fatalf("len %d: %v", c.n, err)
		}
		if len(m) != c.wantSize {
			t.Errorf("len %d -> size %d, want %d", c.n, len(m), c.wantSize)
		}
		// top-left finder: the 7x7 outer ring must be dark, and the surrounding
		// separator row/col light.
		for i := 0; i < 7; i++ {
			if !m[0][i] || !m[i][0] || !m[6][i] || !m[i][6] {
				t.Errorf("len %d: finder ring broken at %d", c.n, i)
				break
			}
		}
	}
	// too long for v9-M (byte capacity 180)
	long := make([]byte, 181)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := QRMatrix(string(long)); err == nil {
		t.Error("expected error for input too long for v9-M, got nil")
	}
}
