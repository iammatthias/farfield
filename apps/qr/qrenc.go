package main

// QR Code encoder — byte mode, versions 1-40, EC levels L/M/Q/H.
// Pure standard library; no external QR dependency. Renders to SVG.
// Following ISO/IEC 18004 (QR Code 2005).

import (
	"errors"
	"fmt"
	"strings"
)

// ECLevel is the QR error-correction level.
type ECLevel int

const (
	ECLow      ECLevel = iota // L — ~7% recovery
	ECMedium                  // M — ~15%
	ECQuartile                // Q — ~25%
	ECHigh                    // H — ~30%
)

// ParseECLevel returns the level for "L"/"M"/"Q"/"H" (case-insensitive).
func ParseECLevel(s string) (ECLevel, bool) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "L":
		return ECLow, true
	case "M", "":
		return ECMedium, true
	case "Q":
		return ECQuartile, true
	case "H":
		return ECHigh, true
	}
	return ECMedium, false
}

// String returns the single-letter form of the EC level.
func (l ECLevel) String() string {
	return [...]string{"L", "M", "Q", "H"}[l]
}

// versionInfo captures one (version, ec-level) row from Table E.1.
// blocks is a list of {count, dataPerBlock} groups; sum(count*dataPerBlock)
// is the total data codeword count for the symbol. ecPerBlock is shared by
// every block in the symbol.
type versionInfo struct {
	ecPerBlock int
	blocks     []blockSpec
}

type blockSpec struct{ count, dataPerBlock int }

// totalDataCodewords is the sum of data codewords across every block.
func (v versionInfo) totalDataCodewords() int {
	n := 0
	for _, b := range v.blocks {
		n += b.count * b.dataPerBlock
	}
	return n
}

// totalCodewords includes EC.
func (v versionInfo) totalCodewords() int {
	blocks := 0
	for _, b := range v.blocks {
		blocks += b.count
	}
	return v.totalDataCodewords() + blocks*v.ecPerBlock
}

// versionTable[version-1][ec-level] gives the EC blocks for that combination.
// Sourced from ISO/IEC 18004 Annex E (Table E.1).
var versionTable = [40][4]versionInfo{
	// V1
	{
		{7, []blockSpec{{1, 19}}},
		{10, []blockSpec{{1, 16}}},
		{13, []blockSpec{{1, 13}}},
		{17, []blockSpec{{1, 9}}},
	},
	// V2
	{
		{10, []blockSpec{{1, 34}}},
		{16, []blockSpec{{1, 28}}},
		{22, []blockSpec{{1, 22}}},
		{28, []blockSpec{{1, 16}}},
	},
	// V3
	{
		{15, []blockSpec{{1, 55}}},
		{26, []blockSpec{{1, 44}}},
		{18, []blockSpec{{2, 17}}},
		{22, []blockSpec{{2, 13}}},
	},
	// V4
	{
		{20, []blockSpec{{1, 80}}},
		{18, []blockSpec{{2, 32}}},
		{26, []blockSpec{{2, 24}}},
		{16, []blockSpec{{4, 9}}},
	},
	// V5
	{
		{26, []blockSpec{{1, 108}}},
		{24, []blockSpec{{2, 43}}},
		{18, []blockSpec{{2, 15}, {2, 16}}},
		{22, []blockSpec{{2, 11}, {2, 12}}},
	},
	// V6
	{
		{18, []blockSpec{{2, 68}}},
		{16, []blockSpec{{4, 27}}},
		{24, []blockSpec{{4, 19}}},
		{28, []blockSpec{{4, 15}}},
	},
	// V7
	{
		{20, []blockSpec{{2, 78}}},
		{18, []blockSpec{{4, 31}}},
		{18, []blockSpec{{2, 14}, {4, 15}}},
		{26, []blockSpec{{4, 13}, {1, 14}}},
	},
	// V8
	{
		{24, []blockSpec{{2, 97}}},
		{22, []blockSpec{{2, 38}, {2, 39}}},
		{22, []blockSpec{{4, 18}, {2, 19}}},
		{26, []blockSpec{{4, 14}, {2, 15}}},
	},
	// V9
	{
		{30, []blockSpec{{2, 116}}},
		{22, []blockSpec{{3, 36}, {2, 37}}},
		{20, []blockSpec{{4, 16}, {4, 17}}},
		{24, []blockSpec{{4, 12}, {4, 13}}},
	},
	// V10
	{
		{18, []blockSpec{{2, 68}, {2, 69}}},
		{26, []blockSpec{{4, 43}, {1, 44}}},
		{24, []blockSpec{{6, 19}, {2, 20}}},
		{28, []blockSpec{{6, 15}, {2, 16}}},
	},
	// V11
	{
		{20, []blockSpec{{4, 81}}},
		{30, []blockSpec{{1, 50}, {4, 51}}},
		{28, []blockSpec{{4, 22}, {4, 23}}},
		{24, []blockSpec{{3, 12}, {8, 13}}},
	},
	// V12
	{
		{24, []blockSpec{{2, 92}, {2, 93}}},
		{22, []blockSpec{{6, 36}, {2, 37}}},
		{26, []blockSpec{{4, 20}, {6, 21}}},
		{28, []blockSpec{{7, 14}, {4, 15}}},
	},
	// V13
	{
		{26, []blockSpec{{4, 107}}},
		{22, []blockSpec{{8, 37}, {1, 38}}},
		{24, []blockSpec{{8, 20}, {4, 21}}},
		{22, []blockSpec{{12, 11}, {4, 12}}},
	},
	// V14
	{
		{30, []blockSpec{{3, 115}, {1, 116}}},
		{24, []blockSpec{{4, 40}, {5, 41}}},
		{20, []blockSpec{{11, 16}, {5, 17}}},
		{24, []blockSpec{{11, 12}, {5, 13}}},
	},
	// V15
	{
		{22, []blockSpec{{5, 87}, {1, 88}}},
		{24, []blockSpec{{5, 41}, {5, 42}}},
		{30, []blockSpec{{5, 24}, {7, 25}}},
		{24, []blockSpec{{11, 12}, {7, 13}}},
	},
	// V16
	{
		{24, []blockSpec{{5, 98}, {1, 99}}},
		{28, []blockSpec{{7, 45}, {3, 46}}},
		{24, []blockSpec{{15, 19}, {2, 20}}},
		{30, []blockSpec{{3, 15}, {13, 16}}},
	},
	// V17
	{
		{28, []blockSpec{{1, 107}, {5, 108}}},
		{28, []blockSpec{{10, 46}, {1, 47}}},
		{28, []blockSpec{{1, 22}, {15, 23}}},
		{28, []blockSpec{{2, 14}, {17, 15}}},
	},
	// V18
	{
		{30, []blockSpec{{5, 120}, {1, 121}}},
		{26, []blockSpec{{9, 43}, {4, 44}}},
		{28, []blockSpec{{17, 22}, {1, 23}}},
		{28, []blockSpec{{2, 14}, {19, 15}}},
	},
	// V19
	{
		{28, []blockSpec{{3, 113}, {4, 114}}},
		{26, []blockSpec{{3, 44}, {11, 45}}},
		{26, []blockSpec{{17, 21}, {4, 22}}},
		{26, []blockSpec{{9, 13}, {16, 14}}},
	},
	// V20
	{
		{28, []blockSpec{{3, 107}, {5, 108}}},
		{26, []blockSpec{{3, 41}, {13, 42}}},
		{30, []blockSpec{{15, 24}, {5, 25}}},
		{28, []blockSpec{{15, 15}, {10, 16}}},
	},
	// V21
	{
		{28, []blockSpec{{4, 116}, {4, 117}}},
		{26, []blockSpec{{17, 42}}},
		{28, []blockSpec{{17, 22}, {6, 23}}},
		{30, []blockSpec{{19, 16}, {6, 17}}},
	},
	// V22
	{
		{28, []blockSpec{{2, 111}, {7, 112}}},
		{28, []blockSpec{{17, 46}}},
		{30, []blockSpec{{7, 24}, {16, 25}}},
		{24, []blockSpec{{34, 13}}},
	},
	// V23
	{
		{30, []blockSpec{{4, 121}, {5, 122}}},
		{28, []blockSpec{{4, 47}, {14, 48}}},
		{30, []blockSpec{{11, 24}, {14, 25}}},
		{30, []blockSpec{{16, 15}, {14, 16}}},
	},
	// V24
	{
		{30, []blockSpec{{6, 117}, {4, 118}}},
		{28, []blockSpec{{6, 45}, {14, 46}}},
		{30, []blockSpec{{11, 24}, {16, 25}}},
		{30, []blockSpec{{30, 16}, {2, 17}}},
	},
	// V25
	{
		{26, []blockSpec{{8, 106}, {4, 107}}},
		{28, []blockSpec{{8, 47}, {13, 48}}},
		{30, []blockSpec{{7, 24}, {22, 25}}},
		{30, []blockSpec{{22, 15}, {13, 16}}},
	},
	// V26
	{
		{28, []blockSpec{{10, 114}, {2, 115}}},
		{28, []blockSpec{{19, 46}, {4, 47}}},
		{28, []blockSpec{{28, 22}, {6, 23}}},
		{30, []blockSpec{{33, 16}, {4, 17}}},
	},
	// V27
	{
		{30, []blockSpec{{8, 122}, {4, 123}}},
		{28, []blockSpec{{22, 45}, {3, 46}}},
		{30, []blockSpec{{8, 23}, {26, 24}}},
		{30, []blockSpec{{12, 15}, {28, 16}}},
	},
	// V28
	{
		{30, []blockSpec{{3, 117}, {10, 118}}},
		{28, []blockSpec{{3, 45}, {23, 46}}},
		{30, []blockSpec{{4, 24}, {31, 25}}},
		{30, []blockSpec{{11, 15}, {31, 16}}},
	},
	// V29
	{
		{30, []blockSpec{{7, 116}, {7, 117}}},
		{28, []blockSpec{{21, 45}, {7, 46}}},
		{30, []blockSpec{{1, 23}, {37, 24}}},
		{30, []blockSpec{{19, 15}, {26, 16}}},
	},
	// V30
	{
		{30, []blockSpec{{5, 115}, {10, 116}}},
		{28, []blockSpec{{19, 47}, {10, 48}}},
		{30, []blockSpec{{15, 24}, {25, 25}}},
		{30, []blockSpec{{23, 15}, {25, 16}}},
	},
	// V31
	{
		{30, []blockSpec{{13, 115}, {3, 116}}},
		{28, []blockSpec{{2, 46}, {29, 47}}},
		{30, []blockSpec{{42, 24}, {1, 25}}},
		{30, []blockSpec{{23, 15}, {28, 16}}},
	},
	// V32
	{
		{30, []blockSpec{{17, 115}}},
		{28, []blockSpec{{10, 46}, {23, 47}}},
		{30, []blockSpec{{10, 24}, {35, 25}}},
		{30, []blockSpec{{19, 15}, {35, 16}}},
	},
	// V33
	{
		{30, []blockSpec{{17, 115}, {1, 116}}},
		{28, []blockSpec{{14, 46}, {21, 47}}},
		{30, []blockSpec{{29, 24}, {19, 25}}},
		{30, []blockSpec{{11, 15}, {46, 16}}},
	},
	// V34
	{
		{30, []blockSpec{{13, 115}, {6, 116}}},
		{28, []blockSpec{{14, 46}, {23, 47}}},
		{30, []blockSpec{{44, 24}, {7, 25}}},
		{30, []blockSpec{{59, 16}, {1, 17}}},
	},
	// V35
	{
		{30, []blockSpec{{12, 121}, {7, 122}}},
		{28, []blockSpec{{12, 47}, {26, 48}}},
		{30, []blockSpec{{39, 24}, {14, 25}}},
		{30, []blockSpec{{22, 15}, {41, 16}}},
	},
	// V36
	{
		{30, []blockSpec{{6, 121}, {14, 122}}},
		{28, []blockSpec{{6, 47}, {34, 48}}},
		{30, []blockSpec{{46, 24}, {10, 25}}},
		{30, []blockSpec{{2, 15}, {64, 16}}},
	},
	// V37
	{
		{30, []blockSpec{{17, 122}, {4, 123}}},
		{28, []blockSpec{{29, 46}, {14, 47}}},
		{30, []blockSpec{{49, 24}, {10, 25}}},
		{30, []blockSpec{{24, 15}, {46, 16}}},
	},
	// V38
	{
		{30, []blockSpec{{4, 122}, {18, 123}}},
		{28, []blockSpec{{13, 46}, {32, 47}}},
		{30, []blockSpec{{48, 24}, {14, 25}}},
		{30, []blockSpec{{42, 15}, {32, 16}}},
	},
	// V39
	{
		{30, []blockSpec{{20, 117}, {4, 118}}},
		{28, []blockSpec{{40, 47}, {7, 48}}},
		{30, []blockSpec{{43, 24}, {22, 25}}},
		{30, []blockSpec{{10, 15}, {67, 16}}},
	},
	// V40
	{
		{30, []blockSpec{{19, 118}, {6, 119}}},
		{28, []blockSpec{{18, 47}, {31, 48}}},
		{30, []blockSpec{{34, 24}, {34, 25}}},
		{30, []blockSpec{{20, 15}, {61, 16}}},
	},
}

// alignmentCenters[version-2] lists the row/col centers for alignment patterns.
// Version 1 has none. Sourced from ISO/IEC 18004 Annex E.
var alignmentCenters = [39][]int{
	{6, 18},
	{6, 22},
	{6, 26},
	{6, 30},
	{6, 34},
	{6, 22, 38},
	{6, 24, 42},
	{6, 26, 46},
	{6, 28, 50},
	{6, 30, 54},
	{6, 32, 58},
	{6, 34, 62},
	{6, 26, 46, 66},
	{6, 26, 48, 70},
	{6, 26, 50, 74},
	{6, 30, 54, 78},
	{6, 30, 56, 82},
	{6, 30, 58, 86},
	{6, 34, 62, 90},
	{6, 28, 50, 72, 94},
	{6, 26, 50, 74, 98},
	{6, 30, 54, 78, 102},
	{6, 28, 54, 80, 106},
	{6, 32, 58, 84, 110},
	{6, 30, 58, 86, 114},
	{6, 34, 62, 90, 118},
	{6, 26, 50, 74, 98, 122},
	{6, 30, 54, 78, 102, 126},
	{6, 26, 52, 78, 104, 130},
	{6, 30, 56, 82, 108, 134},
	{6, 34, 60, 86, 112, 138},
	{6, 30, 58, 86, 114, 142},
	{6, 34, 62, 90, 118, 146},
	{6, 30, 54, 78, 102, 126, 150},
	{6, 24, 50, 76, 102, 128, 154},
	{6, 28, 54, 80, 106, 132, 158},
	{6, 32, 58, 84, 110, 136, 162},
	{6, 26, 54, 82, 110, 138, 166},
	{6, 30, 58, 86, 114, 142, 170},
}

// charCountBits returns the byte-mode character count indicator width.
func charCountBits(version int) int {
	if version <= 9 {
		return 8
	}
	return 16
}

// pickVersion returns the smallest version 1-40 whose data capacity fits
// dataLen bytes at the given EC level, accounting for the mode and
// length-indicator overhead.
func pickVersion(dataLen int, ec ECLevel) (int, error) {
	for v := 1; v <= 40; v++ {
		dataCw := versionTable[v-1][ec].totalDataCodewords()
		// 4 bits mode + N bits char count + 8*dataLen + 4 bits terminator,
		// padded to a byte boundary. Use a ceiling on the bit count.
		bits := 4 + charCountBits(v) + 8*dataLen
		needBytes := (bits + 4 + 7) / 8 // +terminator
		if needBytes <= dataCw {
			return v, nil
		}
	}
	return 0, fmt.Errorf("payload too large: %d bytes exceeds version 40 capacity at level %s", dataLen, ec)
}

// ── bit buffer ─────────────────────────────────────────────────────────────

type bitBuf struct {
	data []byte
	nBit int // total bits written
}

func (b *bitBuf) appendBits(val uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		bit := (val >> uint(i)) & 1
		if b.nBit%8 == 0 {
			b.data = append(b.data, 0)
		}
		if bit == 1 {
			b.data[b.nBit/8] |= 1 << uint(7-(b.nBit%8))
		}
		b.nBit++
	}
}

// padToByte pads the buffer so its length is a whole number of bytes.
func (b *bitBuf) padToByte() {
	for b.nBit%8 != 0 {
		b.appendBits(0, 1)
	}
}

// ── GF(256) for Reed-Solomon ───────────────────────────────────────────────

var (
	gfExp [512]byte
	gfLog [256]byte
)

func init() {
	x := byte(1)
	for i := 0; i < 255; i++ {
		gfExp[i] = x
		gfLog[x] = byte(i)
		hi := x & 0x80
		x <<= 1
		if hi != 0 {
			x ^= 0x1D // primitive polynomial 0x11D, low 8 bits
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// rsGenerator returns the generator polynomial of degree n+1, coefficients
// stored most-significant-first.
func rsGenerator(n int) []byte {
	g := []byte{1}
	for i := 0; i < n; i++ {
		// multiply g by (x - α^i)
		ng := make([]byte, len(g)+1)
		for j, c := range g {
			ng[j] ^= c
			ng[j+1] ^= gfMul(c, gfExp[i])
		}
		g = ng
	}
	return g
}

// rsEncode returns the n EC codewords for data using polynomial division.
func rsEncode(data []byte, n int) []byte {
	gen := rsGenerator(n)
	// result starts as data shifted left by n zeros
	buf := make([]byte, len(data)+n)
	copy(buf, data)
	for i := 0; i < len(data); i++ {
		coef := buf[i]
		if coef == 0 {
			continue
		}
		for j, gc := range gen {
			buf[i+j] ^= gfMul(gc, coef)
		}
	}
	return buf[len(data):]
}

// ── codeword interleaving ──────────────────────────────────────────────────

// buildCodewords returns the final stream: interleaved data + interleaved EC,
// followed by the remainder zero bits (added by the matrix placer).
func buildCodewords(data []byte, ec ECLevel, version int) []byte {
	info := versionTable[version-1][ec]
	// split data into blocks per the spec
	dataBlocks := make([][]byte, 0)
	ecBlocks := make([][]byte, 0)
	i := 0
	for _, bs := range info.blocks {
		for k := 0; k < bs.count; k++ {
			db := data[i : i+bs.dataPerBlock]
			i += bs.dataPerBlock
			dataBlocks = append(dataBlocks, db)
			ecBlocks = append(ecBlocks, rsEncode(db, info.ecPerBlock))
		}
	}
	// interleave data by column
	maxData := 0
	for _, b := range dataBlocks {
		if len(b) > maxData {
			maxData = len(b)
		}
	}
	out := make([]byte, 0)
	for col := 0; col < maxData; col++ {
		for _, b := range dataBlocks {
			if col < len(b) {
				out = append(out, b[col])
			}
		}
	}
	for col := 0; col < info.ecPerBlock; col++ {
		for _, b := range ecBlocks {
			out = append(out, b[col])
		}
	}
	return out
}

// ── matrix construction ───────────────────────────────────────────────────

// matrix holds the QR module grid. Use bool grid for modules and a separate
// reserved grid to skip during data placement.
type matrix struct {
	size     int
	mod      [][]byte // 1 = dark, 0 = light
	reserved [][]bool
}

func newMatrix(size int) *matrix {
	m := &matrix{size: size}
	m.mod = make([][]byte, size)
	m.reserved = make([][]bool, size)
	for i := range m.mod {
		m.mod[i] = make([]byte, size)
		m.reserved[i] = make([]bool, size)
	}
	return m
}

func (m *matrix) set(x, y int, v byte) {
	m.mod[y][x] = v
}

func (m *matrix) get(x, y int) byte {
	return m.mod[y][x]
}

func (m *matrix) reserve(x, y int) {
	m.reserved[y][x] = true
}

// placeFinder places one 7x7 finder pattern at (x,y) — top-left corner.
func (m *matrix) placeFinder(x, y int) {
	for dy := -1; dy <= 7; dy++ {
		for dx := -1; dx <= 7; dx++ {
			xx, yy := x+dx, y+dy
			if xx < 0 || yy < 0 || xx >= m.size || yy >= m.size {
				continue
			}
			m.reserve(xx, yy)
			// outer ring (frame) and inner block
			if dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6 {
				if dx == 0 || dx == 6 || dy == 0 || dy == 6 {
					m.set(xx, yy, 1)
				} else if dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4 {
					m.set(xx, yy, 1)
				} else {
					m.set(xx, yy, 0)
				}
			} else {
				// separator — always light
				m.set(xx, yy, 0)
			}
		}
	}
}

// placeAlignment puts a 5x5 alignment pattern centered at (cx, cy).
func (m *matrix) placeAlignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			x, y := cx+dx, cy+dy
			m.reserve(x, y)
			if dx == -2 || dx == 2 || dy == -2 || dy == 2 || (dx == 0 && dy == 0) {
				m.set(x, y, 1)
			} else {
				m.set(x, y, 0)
			}
		}
	}
}

// placeTiming places the two timing-pattern rows.
func (m *matrix) placeTiming() {
	for i := 8; i < m.size-8; i++ {
		v := byte(0)
		if i%2 == 0 {
			v = 1
		}
		m.set(i, 6, v)
		m.set(6, i, v)
		m.reserve(i, 6)
		m.reserve(6, i)
	}
}

// reserveFormat marks the format-info modules around each finder.
func (m *matrix) reserveFormat() {
	// horizontal under top-left finder and right side of top-right
	for i := 0; i < 9; i++ {
		m.reserve(i, 8)
		m.reserve(8, i)
	}
	for i := 0; i < 8; i++ {
		m.reserve(m.size-1-i, 8)
		m.reserve(8, m.size-1-i)
	}
	// the always-dark module
	m.set(8, m.size-8, 1)
	m.reserve(8, m.size-8)
}

// reserveVersion marks the version-info blocks (only for v7+).
func (m *matrix) reserveVersion(version int) {
	if version < 7 {
		return
	}
	for y := 0; y < 6; y++ {
		for x := m.size - 11; x < m.size-8; x++ {
			m.reserve(x, y)
		}
	}
	for x := 0; x < 6; x++ {
		for y := m.size - 11; y < m.size-8; y++ {
			m.reserve(x, y)
		}
	}
}

// placeData walks the data bit stream into unreserved modules using QR's
// zig-zag pattern. data is the byte stream (already padded with the
// terminator and pad bytes).
func (m *matrix) placeData(data []byte) {
	bitIdx := 0
	totalBits := len(data) * 8
	x := m.size - 1
	for x > 0 {
		if x == 6 {
			// skip the vertical timing column
			x--
		}
		// two columns at a time
		for y := 0; y < m.size; y++ {
			row := y
			if (m.size-1-x)/2%2 == 0 {
				row = m.size - 1 - y // upward
			}
			for dx := 0; dx < 2; dx++ {
				xx := x - dx
				if m.reserved[row][xx] {
					continue
				}
				var bit byte
				if bitIdx < totalBits {
					bit = (data[bitIdx/8] >> uint(7-(bitIdx%8))) & 1
				}
				m.set(xx, row, bit)
				bitIdx++
			}
		}
		x -= 2
	}
}

// applyMask XORs the mask onto non-reserved modules.
func (m *matrix) applyMask(mask int) {
	for y := 0; y < m.size; y++ {
		for x := 0; x < m.size; x++ {
			if m.reserved[y][x] {
				continue
			}
			if maskAt(mask, x, y) {
				m.mod[y][x] ^= 1
			}
		}
	}
}

func maskAt(mask, x, y int) bool {
	switch mask {
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

// ── format & version info ──────────────────────────────────────────────────

// ecBits is the 2-bit encoding of an EC level used in the format string.
var ecBits = [4]uint32{
	ECLow:      0b01,
	ECMedium:   0b00,
	ECQuartile: 0b11,
	ECHigh:     0b10,
}

const formatMask = 0x5412

// formatInfo returns the 15 bits (mask-XORed) for (ec, maskID).
func formatInfo(ec ECLevel, mask int) uint32 {
	data := ecBits[ec]<<3 | uint32(mask)
	d := data << 10
	gen := uint32(0x537)
	for i := 4; i >= 0; i-- {
		if d&(1<<uint(i+10)) != 0 {
			d ^= gen << uint(i)
		}
	}
	return (data<<10 | d) ^ formatMask
}

// writeFormat stamps the format-info bits around each finder. Must be called
// after applyMask, since reserveFormat marks these modules and applyMask
// skips them.
func (m *matrix) writeFormat(ec ECLevel, mask int) {
	bits := formatInfo(ec, mask)
	// top-left vertical (bits 0..5, skip timing at 6, then 7..8)
	for i := 0; i < 6; i++ {
		m.mod[i][8] = byte((bits >> uint(i)) & 1)
	}
	m.mod[7][8] = byte((bits >> 6) & 1)
	m.mod[8][8] = byte((bits >> 7) & 1)
	m.mod[8][7] = byte((bits >> 8) & 1)
	for i := 9; i < 15; i++ {
		m.mod[8][14-i] = byte((bits >> uint(i)) & 1)
	}
	// Copy 2: bits 0..7 horizontal (row 8, going LEFT from col size-1),
	// then bits 8..14 vertical (col 8, going DOWN from row size-7).
	for i := 0; i < 8; i++ {
		m.mod[8][m.size-1-i] = byte((bits >> uint(i)) & 1)
	}
	for i := 8; i < 15; i++ {
		m.mod[m.size-15+i][8] = byte((bits >> uint(i)) & 1)
	}
}

// writeVersion stamps the 18-bit version info (v7+ only).
func (m *matrix) writeVersion(version int) {
	if version < 7 {
		return
	}
	bits := versionInfoBits(version)
	for i := 0; i < 18; i++ {
		row := i / 3
		col := m.size - 11 + i%3
		bit := byte((bits >> uint(i)) & 1)
		m.mod[row][col] = bit
		m.mod[col][row] = bit
	}
}

func versionInfoBits(version int) uint32 {
	d := uint32(version) << 12
	gen := uint32(0x1F25)
	for i := 5; i >= 0; i-- {
		if d&(1<<uint(i+12)) != 0 {
			d ^= gen << uint(i)
		}
	}
	return uint32(version)<<12 | d
}

// ── mask penalty (ISO/IEC 18004 §8.8.2) ────────────────────────────────────

func (m *matrix) penalty() int {
	n := m.size
	pen := 0

	// Rule 1: runs of 5+ same-color modules in rows and columns
	for y := 0; y < n; y++ {
		runColor, run := m.mod[y][0], 1
		for x := 1; x < n; x++ {
			if m.mod[y][x] == runColor {
				run++
			} else {
				if run >= 5 {
					pen += 3 + (run - 5)
				}
				runColor = m.mod[y][x]
				run = 1
			}
		}
		if run >= 5 {
			pen += 3 + (run - 5)
		}
	}
	for x := 0; x < n; x++ {
		runColor, run := m.mod[0][x], 1
		for y := 1; y < n; y++ {
			if m.mod[y][x] == runColor {
				run++
			} else {
				if run >= 5 {
					pen += 3 + (run - 5)
				}
				runColor = m.mod[y][x]
				run = 1
			}
		}
		if run >= 5 {
			pen += 3 + (run - 5)
		}
	}

	// Rule 2: 2x2 blocks of same color
	for y := 0; y < n-1; y++ {
		for x := 0; x < n-1; x++ {
			c := m.mod[y][x]
			if m.mod[y][x+1] == c && m.mod[y+1][x] == c && m.mod[y+1][x+1] == c {
				pen += 3
			}
		}
	}

	// Rule 3: finder-like 1:1:3:1:1 ratio patterns with light surround
	const a, b = 1, 0
	patterns := [][]byte{
		{a, b, a, a, a, b, a, b, b, b, b},
		{b, b, b, b, a, b, a, a, a, b, a},
	}
	for y := 0; y < n; y++ {
		for x := 0; x <= n-11; x++ {
			for _, p := range patterns {
				match := true
				for k := 0; k < 11; k++ {
					if m.mod[y][x+k] != p[k] {
						match = false
						break
					}
				}
				if match {
					pen += 40
				}
			}
		}
	}
	for x := 0; x < n; x++ {
		for y := 0; y <= n-11; y++ {
			for _, p := range patterns {
				match := true
				for k := 0; k < 11; k++ {
					if m.mod[y+k][x] != p[k] {
						match = false
						break
					}
				}
				if match {
					pen += 40
				}
			}
		}
	}

	// Rule 4: dark-module ratio
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if m.mod[y][x] == 1 {
				dark++
			}
		}
	}
	total := n * n
	pct := dark * 100 / total
	step := abs((pct/5)*5-50) / 5
	prev := abs(((pct/5)*5+5)-50) / 5
	if prev < step {
		step = prev
	}
	pen += step * 10
	return pen
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// ── public Encode entry point ──────────────────────────────────────────────

// Encode builds a QR matrix for the given payload at the requested EC level.
// Returns the matrix as a 2D byte slice (1 = dark, 0 = light) and the chosen
// version. Errors only when payload exceeds version 40 capacity.
func Encode(payload []byte, ec ECLevel) ([][]byte, int, error) {
	if len(payload) == 0 {
		return nil, 0, errors.New("empty payload")
	}
	version, err := pickVersion(len(payload), ec)
	if err != nil {
		return nil, 0, err
	}
	info := versionTable[version-1][ec]

	// 1. Build bit stream: mode 0100, char count, payload, terminator, pad.
	var bb bitBuf
	bb.appendBits(0b0100, 4) // byte mode
	bb.appendBits(uint32(len(payload)), charCountBits(version))
	for _, b := range payload {
		bb.appendBits(uint32(b), 8)
	}
	// terminator: up to 4 zero bits, but not more than fits in capacity
	capBits := info.totalDataCodewords() * 8
	termBits := capBits - bb.nBit
	if termBits > 4 {
		termBits = 4
	}
	if termBits > 0 {
		bb.appendBits(0, termBits)
	}
	bb.padToByte()
	// pad to capacity with 0xEC, 0x11 alternating
	for len(bb.data) < info.totalDataCodewords() {
		bb.data = append(bb.data, 0xEC)
		if len(bb.data) < info.totalDataCodewords() {
			bb.data = append(bb.data, 0x11)
		}
	}

	// 2. EC + interleaving.
	codewords := buildCodewords(bb.data, ec, version)

	// 3. Build matrix structure.
	size := 21 + 4*(version-1)
	m := newMatrix(size)
	m.placeFinder(0, 0)
	m.placeFinder(size-7, 0)
	m.placeFinder(0, size-7)
	// alignment patterns (skip any overlapping the finders)
	if version >= 2 {
		centers := alignmentCenters[version-2]
		for _, cy := range centers {
			for _, cx := range centers {
				if (cx == 6 && cy == 6) ||
					(cx == 6 && cy == centers[len(centers)-1]) ||
					(cx == centers[len(centers)-1] && cy == 6) {
					continue
				}
				m.placeAlignment(cx, cy)
			}
		}
	}
	m.placeTiming()
	m.reserveFormat()
	m.reserveVersion(version)
	m.placeData(codewords)

	// 4. Try every mask, score, pick the best.
	bestMask := 0
	bestPen := -1
	var bestMod [][]byte
	for mk := 0; mk < 8; mk++ {
		trial := newMatrix(size)
		copyMatrix(trial, m)
		trial.applyMask(mk)
		trial.writeFormat(ec, mk)
		trial.writeVersion(version)
		p := trial.penalty()
		if bestPen < 0 || p < bestPen {
			bestPen = p
			bestMask = mk
			bestMod = make([][]byte, size)
			for i := range trial.mod {
				bestMod[i] = make([]byte, size)
				copy(bestMod[i], trial.mod[i])
			}
		}
	}
	_ = bestMask
	return bestMod, version, nil
}

func copyMatrix(dst, src *matrix) {
	for y := 0; y < src.size; y++ {
		copy(dst.mod[y], src.mod[y])
		copy(dst.reserved[y], src.reserved[y])
	}
}

// RenderSVG converts a QR module grid to a self-contained SVG string. A
// 4-module quiet zone is included, per the spec.
func RenderSVG(mod [][]byte) string {
	n := len(mod)
	const quiet = 4
	full := n + quiet*2
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, full, full)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/>`, full, full)
	b.WriteString(`<path fill="#0a0a0a" d="`)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if mod[y][x] == 1 {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}

// EncodeSVG is the combined helper: encode payload + render.
func EncodeSVG(payload []byte, ec ECLevel) (string, int, error) {
	mod, version, err := Encode(payload, ec)
	if err != nil {
		return "", 0, err
	}
	return RenderSVG(mod), version, nil
}
