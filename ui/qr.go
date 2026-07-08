// Pure-Go QR Code generator (byte mode, error-correction level M, versions 1-9).
//
// CGO-free, stdlib-only — same invariant as the rest of this package (no SDL, no C).
// Written for the muOS onboarding wizard's Tailscale sign-in step, which must render the
// tailscaled login URL (~50-90 ASCII bytes) as a scannable QR on /dev/fb0. Byte mode +
// level M + versions 1-9 (8-bit character-count indicator) comfortably covers any login
// URL (v9-M holds 180 bytes); a longer input errors rather than silently truncating.
//
// The algorithm follows ISO/IEC 18004. It was cross-checked module-for-module against
// libqrencode 4.1.1 (`qrencode -8 -l M -m 0`) across many strings, and qr_test.go pins a
// hardcoded reference matrix so a regression is caught without the external tool.
package ui

import "errors"

// ---- Galois field GF(256), primitive polynomial 0x11D (α = 2) ------------------------

var qrExp [256]byte
var qrLog [256]byte

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		qrExp[i] = byte(x)
		qrLog[byte(x)] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
}

func qrMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return qrExp[(int(qrLog[a])+int(qrLog[b]))%255]
}

func qrPolyMul(a, b []byte) []byte {
	res := make([]byte, len(a)+len(b)-1)
	for i := range a {
		for j := range b {
			res[i+j] ^= qrMul(a[i], b[j])
		}
	}
	return res
}

// qrGenPoly builds the degree-n Reed-Solomon generator polynomial ∏(x - α^i).
func qrGenPoly(n int) []byte {
	g := []byte{1}
	for i := 0; i < n; i++ {
		g = qrPolyMul(g, []byte{1, qrExp[i]})
	}
	return g
}

// qrEC returns the nEC error-correction codewords for a data block (polynomial division).
func qrEC(data []byte, nEC int) []byte {
	gen := qrGenPoly(nEC)
	res := make([]byte, len(data)+nEC)
	copy(res, data)
	for i := 0; i < len(data); i++ {
		factor := res[i]
		if factor == 0 {
			continue
		}
		for j := 0; j < len(gen); j++ {
			res[i+j] ^= qrMul(gen[j], factor)
		}
	}
	return res[len(data):]
}

// ---- version tables (ECC level M) ----------------------------------------------------

// qrEccM[v] = {ecPerBlock, g1Blocks, g1Data, g2Blocks, g2Data} for level M, versions 1-9.
type qrBlockSpec struct{ ec, g1n, g1d, g2n, g2d int }

var qrEccM = map[int]qrBlockSpec{
	1: {10, 1, 16, 0, 0},
	2: {16, 1, 28, 0, 0},
	3: {26, 1, 44, 0, 0},
	4: {18, 2, 32, 0, 0},
	5: {24, 2, 43, 0, 0},
	6: {16, 4, 27, 0, 0},
	7: {18, 4, 31, 0, 0},
	8: {22, 2, 38, 2, 39},
	9: {22, 3, 36, 2, 37},
}

// alignment-pattern centre coordinates per version (v1 has none).
var qrAlign = map[int][]int{
	1: nil, 2: {6, 18}, 3: {6, 22}, 4: {6, 26}, 5: {6, 30},
	6: {6, 34}, 7: {6, 22, 38}, 8: {6, 24, 42}, 9: {6, 26, 46},
}

// 18-bit version-information strings (needed only for versions >= 7).
var qrVersionInfo = map[int]int{7: 0x07C94, 8: 0x085BC, 9: 0x09A99}

func (s qrBlockSpec) totalDataCW() int { return s.g1n*s.g1d + s.g2n*s.g2d }

// ---- bit writer ----------------------------------------------------------------------

type qrBits struct {
	bytes []byte
	nbits int
}

func (b *qrBits) add(val, n int) {
	for i := n - 1; i >= 0; i-- {
		if b.nbits%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if (val>>uint(i))&1 == 1 {
			b.bytes[b.nbits/8] |= 1 << uint(7-b.nbits%8)
		}
		b.nbits++
	}
}

// ---- public API ----------------------------------------------------------------------

// QRMatrix encodes s as a byte-mode, ECC-level-M QR code and returns the module matrix
// (m[y][x], true = dark). It auto-selects the smallest version 1-9 that fits; an input
// too long for v9-M (180 bytes) returns an error.
func QRMatrix(s string) ([][]bool, error) {
	data := []byte(s)
	v := 0
	for cand := 1; cand <= 9; cand++ {
		if qrEccM[cand].totalDataCW()-2 >= len(data) { // 2 = mode(4)+count(8)+term overhead
			v = cand
			break
		}
	}
	if v == 0 {
		return nil, errors.New("qr: data too long for v9-M (byte mode)")
	}
	spec := qrEccM[v]
	total := spec.totalDataCW()

	// data codeword stream: mode(0100) + 8-bit count + payload + terminator + pad.
	bb := &qrBits{}
	bb.add(0b0100, 4)
	bb.add(len(data), 8)
	for _, c := range data {
		bb.add(int(c), 8)
	}
	if term := total*8 - bb.nbits; term > 0 {
		if term > 4 {
			term = 4
		}
		bb.add(0, term)
	}
	for bb.nbits%8 != 0 {
		bb.add(0, 1)
	}
	pad := []byte{0xEC, 0x11}
	for i := 0; len(bb.bytes) < total; i++ {
		bb.bytes = append(bb.bytes, pad[i%2])
	}

	final := qrInterleave(bb.bytes, spec)
	return qrBuildMatrix(v, spec, final), nil
}

// qrInterleave splits data into blocks, computes each block's EC codewords, then
// interleaves data-then-EC per ISO/IEC 18004 §8.6.
func qrInterleave(dataCW []byte, spec qrBlockSpec) []byte {
	var dblocks, eblocks [][]byte
	idx := 0
	appendBlocks := func(n, size int) {
		for k := 0; k < n; k++ {
			blk := dataCW[idx : idx+size]
			idx += size
			dblocks = append(dblocks, blk)
			eblocks = append(eblocks, qrEC(blk, spec.ec))
		}
	}
	appendBlocks(spec.g1n, spec.g1d)
	appendBlocks(spec.g2n, spec.g2d)

	var out []byte
	maxData := spec.g1d
	if spec.g2d > maxData {
		maxData = spec.g2d
	}
	for i := 0; i < maxData; i++ {
		for _, blk := range dblocks {
			if i < len(blk) {
				out = append(out, blk[i])
			}
		}
	}
	for i := 0; i < spec.ec; i++ {
		for _, blk := range eblocks {
			out = append(out, blk[i])
		}
	}
	return out
}

// ---- matrix construction -------------------------------------------------------------

type qrGrid struct {
	size int
	mod  []bool // dark?
	fn   []bool // function module (finder/timing/format/version/align/dark) — not masked, not data
}

func newQRGrid(size int) *qrGrid {
	return &qrGrid{size: size, mod: make([]bool, size*size), fn: make([]bool, size*size)}
}
func (g *qrGrid) at(x, y int) bool     { return g.mod[y*g.size+x] }
func (g *qrGrid) set(x, y int, d bool) { g.mod[y*g.size+x] = d }
func (g *qrGrid) isFn(x, y int) bool   { return g.fn[y*g.size+x] }
func (g *qrGrid) setFn(x, y int, d bool) {
	g.mod[y*g.size+x] = d
	g.fn[y*g.size+x] = true
}

func qrBuildMatrix(v int, spec qrBlockSpec, codewords []byte) [][]bool {
	size := 17 + 4*v
	g := newQRGrid(size)

	// finder patterns + separators
	for _, p := range [][2]int{{0, 0}, {size - 7, 0}, {0, size - 7}} {
		qrPlaceFinder(g, p[0], p[1])
	}
	// timing patterns
	for i := 8; i < size-8; i++ {
		g.setFn(i, 6, i%2 == 0)
		g.setFn(6, i, i%2 == 0)
	}
	// alignment patterns
	centres := qrAlign[v]
	for _, cy := range centres {
		for _, cx := range centres {
			if (cx == 6 && cy == 6) || (cx == 6 && cy == size-7) || (cx == size-7 && cy == 6) {
				continue // overlaps a finder
			}
			qrPlaceAlignment(g, cx, cy)
		}
	}
	// dark module
	g.setFn(8, size-8, true)

	// reserve the format area; reserve AND fill the version area now (version info is a
	// fixed function pattern independent of the mask, and MUST be present during mask
	// penalty evaluation to match the reference — else v>=7 can pick a worse mask).
	qrReserveFormat(g)
	if v >= 7 {
		qrReserveVersion(g)
		qrPlaceVersion(g, v)
	}

	// place data bits (zig-zag, upward first, skipping the vertical timing column at x=6)
	qrPlaceData(g, codewords)

	// choose the mask with the lowest penalty (format info placed per candidate; version
	// info and every other function module are already on g, so penalty sees the full symbol)
	bestPenalty := 1 << 30 // max sentinel: fits a 32-bit int (armhf), dwarfs any real QR penalty
	var best *qrGrid
	for m := 0; m < 8; m++ {
		trial := qrClone(g)
		qrApplyMask(trial, m)
		qrPlaceFormat(trial, m)
		if p := qrPenalty(trial); p < bestPenalty {
			bestPenalty, best = p, trial
		}
	}

	out := make([][]bool, size)
	for y := 0; y < size; y++ {
		out[y] = make([]bool, size)
		for x := 0; x < size; x++ {
			out[y][x] = best.at(x, y)
		}
	}
	return out
}

func qrClone(g *qrGrid) *qrGrid {
	n := &qrGrid{size: g.size, mod: make([]bool, len(g.mod)), fn: make([]bool, len(g.fn))}
	copy(n.mod, g.mod)
	copy(n.fn, g.fn)
	return n
}

func qrPlaceFinder(g *qrGrid, ox, oy int) {
	for dy := -1; dy <= 7; dy++ {
		for dx := -1; dx <= 7; dx++ {
			x, y := ox+dx, oy+dy
			if x < 0 || y < 0 || x >= g.size || y >= g.size {
				continue
			}
			dark := false
			if dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6 {
				border := dx == 0 || dx == 6 || dy == 0 || dy == 6
				core := dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
				dark = border || core
			}
			g.setFn(x, y, dark)
		}
	}
}

func qrPlaceAlignment(g *qrGrid, cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			ring := dx == -2 || dx == 2 || dy == -2 || dy == 2
			centre := dx == 0 && dy == 0
			g.setFn(cx+dx, cy+dy, ring || centre)
		}
	}
}

func qrReserveFormat(g *qrGrid) {
	s := g.size
	for i := 0; i <= 8; i++ {
		if i != 6 {
			g.setFn(i, 8, false) // top-left horizontal
			g.setFn(8, i, false) // top-left vertical
		}
	}
	for i := 0; i < 8; i++ {
		g.setFn(s-1-i, 8, false) // top-right horizontal (8 modules)
	}
	for i := 0; i < 7; i++ {
		g.setFn(8, s-1-i, false) // bottom-left vertical (7 modules; leaves the dark module)
	}
}

func qrReserveVersion(g *qrGrid) {
	s := g.size
	for i := 0; i < 6; i++ {
		for j := 0; j < 3; j++ {
			g.setFn(i, s-11+j, false)
			g.setFn(s-11+j, i, false)
		}
	}
}

// qrPlaceData walks the module grid in the standard upward/downward zig-zag of
// two-column strips (right to left, skipping the x=6 timing column) and drops the
// codeword bits (MSB first) into every non-function module.
func qrPlaceData(g *qrGrid, cw []byte) {
	s := g.size
	bit := 0
	next := func() bool {
		if bit >= len(cw)*8 {
			return false // remainder bits stay 0
		}
		b := (cw[bit/8]>>uint(7-bit%8))&1 == 1
		bit++
		return b
	}
	up := true
	for col := s - 1; col > 0; col -= 2 {
		if col == 6 {
			col-- // skip vertical timing column
		}
		for i := 0; i < s; i++ {
			y := i
			if up {
				y = s - 1 - i
			}
			for c := 0; c < 2; c++ {
				x := col - c
				if !g.isFn(x, y) {
					g.set(x, y, next())
				}
			}
		}
		up = !up
	}
}

func qrMaskBit(m, x, y int) bool {
	switch m {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return (x*y)%2+(x*y)%3 == 0
	case 6:
		return ((x*y)%2+(x*y)%3)%2 == 0
	case 7:
		return ((x+y)%2+(x*y)%3)%2 == 0
	}
	return false
}

func qrApplyMask(g *qrGrid, m int) {
	s := g.size
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			if !g.isFn(x, y) && qrMaskBit(m, x, y) {
				g.set(x, y, !g.at(x, y))
			}
		}
	}
}

// qrPlaceFormat computes the 15-bit format information for level M + mask m (BCH(15,5)
// with generator 0x537, XOR-masked with 0x5412) and writes it to both copies.
func qrPlaceFormat(g *qrGrid, mask int) {
	s := g.size
	data := (0b00 << 3) | mask // level M = 00
	// BCH(15,5) remainder over the 10-bit-shifted data, generator 0x537.
	bch := data << 10
	for i := 14; i >= 10; i-- {
		if bch&(1<<uint(i)) != 0 {
			bch ^= 0x537 << uint(i-10)
		}
	}
	bits := ((data << 10) | bch) ^ 0x5412

	// copy 1: around top-left finder
	for i := 0; i <= 5; i++ {
		g.set(8, i, bitAt(bits, i))
	}
	g.set(8, 7, bitAt(bits, 6))
	g.set(8, 8, bitAt(bits, 7))
	g.set(7, 8, bitAt(bits, 8))
	for i := 9; i <= 14; i++ {
		g.set(14-i, 8, bitAt(bits, i))
	}
	// copy 2: top-right + bottom-left
	for i := 0; i <= 7; i++ {
		g.set(s-1-i, 8, bitAt(bits, i))
	}
	for i := 8; i <= 14; i++ {
		g.set(8, s-15+i, bitAt(bits, i))
	}
}

func qrPlaceVersion(g *qrGrid, v int) {
	s := g.size
	bits := qrVersionInfo[v]
	for i := 0; i < 18; i++ {
		b := bitAt(bits, i)
		a := i / 3
		c := i % 3
		g.set(a, s-11+c, b)
		g.set(s-11+c, a, b)
	}
}

func bitAt(v, i int) bool { return (v>>uint(i))&1 == 1 }

// qrPenalty scores a masked matrix by the four ISO/IEC 18004 rules; lower is better.
// Ported to match the Nayuki qrcodegen reference exactly (run-history finder detection
// for rule 3), so the mask this encoder selects is byte-identical to qrcodegen's.
func qrPenalty(g *qrGrid) int {
	s := g.size
	result := 0

	// rules 1 + 3, rows
	for y := 0; y < s; y++ {
		runColor := false
		runLen := 0
		var hist [7]int
		for x := 0; x < s; x++ {
			if g.at(x, y) == runColor {
				runLen++
				if runLen == 5 {
					result += 3
				} else if runLen > 5 {
					result++
				}
			} else {
				qrAddHistory(runLen, &hist, s)
				if !runColor {
					result += qrCountFinders(&hist) * 40
				}
				runColor = g.at(x, y)
				runLen = 1
			}
		}
		result += qrTermCount(runColor, runLen, &hist, s) * 40
	}
	// rules 1 + 3, columns
	for x := 0; x < s; x++ {
		runColor := false
		runLen := 0
		var hist [7]int
		for y := 0; y < s; y++ {
			if g.at(x, y) == runColor {
				runLen++
				if runLen == 5 {
					result += 3
				} else if runLen > 5 {
					result++
				}
			} else {
				qrAddHistory(runLen, &hist, s)
				if !runColor {
					result += qrCountFinders(&hist) * 40
				}
				runColor = g.at(x, y)
				runLen = 1
			}
		}
		result += qrTermCount(runColor, runLen, &hist, s) * 40
	}
	// rule 2: 2x2 blocks of one colour
	for y := 0; y < s-1; y++ {
		for x := 0; x < s-1; x++ {
			c := g.at(x, y)
			if g.at(x+1, y) == c && g.at(x, y+1) == c && g.at(x+1, y+1) == c {
				result += 3
			}
		}
	}
	// rule 4: dark/light balance
	dark := 0
	for i := range g.mod {
		if g.mod[i] {
			dark++
		}
	}
	total := s * s
	d := dark*20 - total*10
	if d < 0 {
		d = -d
	}
	k := (d+total-1)/total - 1
	result += k * 10
	return result
}

// qrAddHistory shifts a new run length into the 7-slot finder run-history (Nayuki
// _finder_penalty_add_history); the first run of a line gets the implicit light border.
func qrAddHistory(runLen int, hist *[7]int, size int) {
	if hist[0] == 0 {
		runLen += size
	}
	for i := 6; i > 0; i-- {
		hist[i] = hist[i-1]
	}
	hist[0] = runLen
}

// qrCountFinders returns how many 1:1:3:1:1-with-4-light finder-like patterns end at the
// current run-history position (Nayuki _finder_penalty_count_patterns).
func qrCountFinders(hist *[7]int) int {
	n := hist[1]
	core := n > 0 && hist[2] == n && hist[3] == n*3 && hist[4] == n && hist[5] == n
	c := 0
	if core && hist[0] >= n*4 && hist[6] >= n {
		c++
	}
	if core && hist[6] >= n*4 && hist[0] >= n {
		c++
	}
	return c
}

// qrTermCount terminates a line's run history (adding the trailing light border) and
// counts any finder pattern that closes it out (Nayuki _finder_penalty_terminate_and_count).
func qrTermCount(runColor bool, runLen int, hist *[7]int, size int) int {
	if runColor {
		qrAddHistory(runLen, hist, size)
		runLen = 0
	}
	runLen += size
	qrAddHistory(runLen, hist, size)
	return qrCountFinders(hist)
}

// ---- rendering -----------------------------------------------------------------------

// DrawQR renders the module matrix m into the box (bx,by,bw,bh), integer-scaled as large
// as fits with a 4-module quiet zone, centred, dark modules in dark on a light field.
// Returns the drawn side length in pixels (0 if m is empty or the box is too small).
func (c *Canvas) DrawQR(bx, by, bw, bh int, m [][]bool, dark, light Color) int {
	n := len(m)
	if n == 0 {
		return 0
	}
	const quiet = 4
	total := n + 2*quiet
	scale := bw / total
	if h := bh / total; h < scale {
		scale = h
	}
	if scale < 1 {
		return 0
	}
	side := total * scale
	ox := bx + (bw-side)/2
	oy := by + (bh-side)/2
	c.FillRect(ox, oy, side, side, light) // quiet zone + background
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if m[y][x] {
				c.FillRect(ox+(x+quiet)*scale, oy+(y+quiet)*scale, scale, scale, dark)
			}
		}
	}
	return side
}
