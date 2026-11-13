package imaging

import (
	"errors"
	"math"
	"regexp"
	"strings"
)

// Error correction level of a QR code
type QRLevel uint8

// Error correction levels, each recovering about 7, 15, 25 and 30 percent of the symbol
const (
	QRLevelL QRLevel = iota
	QRLevelM
	QRLevelQ
	QRLevelH
)

// Text that needs more capacity than a version 40 symbol of the level offers
var ErrQRTooLong = errors.New("imaging: data too long for a QR code")

// Settings of one QR encoding
type QROptions struct {
	Level QRLevel
	// Light modules framing the symbol in the plain and half block renderings, the compact form keeping its own frame
	Border int
}

// Encoded symbol with the light border it renders with
type QR struct {
	modules [][]bool
	border  int
}

// Segment modes and their indicator values
const (
	qrNumber   = 1
	qrAlphaNum = 2
	qrByte     = 4
)

// Characters of the alphanumeric mode in code order
const qrAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

// One data segment in a mode
type qrSegment struct {
	data []byte
	mode int
}

// Format information value of each level, the bits the symbol stores
var qrLevelBits = [4]int{QRLevelL: 1, QRLevelM: 0, QRLevelQ: 3, QRLevelH: 2}

// Encodes the text in the smallest version holding it at the level, the mask with the lowest penalty applied
func EncodeQR(text string, o QROptions) (*QR, error) {
	level := min(int(o.Level), 3)
	segs := optimalSegments([]byte(text), 20)
	version, err := qrFit(segs, level, 1)
	if err != nil {
		return nil, err
	}
	data, err := qrCodewords(version, level, segs)
	if err != nil {
		return nil, err
	}
	best, bestScore := 0, 0
	for mask := 0; mask < 8; mask++ {
		score := qrPenalty(qrLayout(version, qrLevelBits[level], mask, data, true))
		if mask == 0 || bestScore > score {
			best, bestScore = mask, score
		}
	}
	return &QR{modules: qrLayout(version, qrLevelBits[level], best, data, false), border: max(0, o.Border)}, nil
}

// Character count indicator width of a mode at a version
func qrCountBits(mode, version int) int {
	class := 0
	switch {
	case version >= 27:
		class = 2
	case version >= 10:
		class = 1
	}
	switch mode {
	case qrNumber:
		return []int{10, 12, 14}[class]
	case qrAlphaNum:
		return []int{9, 11, 13}[class]
	}
	return []int{8, 16, 16}[class]
}

// Class of count indicator widths a version uses
func qrSizeClass(version int) int {
	switch {
	case version >= 27:
		return 2
	case version >= 10:
		return 1
	}
	return 0
}

// Growable bit buffer of codewords
type qrBits struct {
	buf []byte
	n   int
}

// Appends the low bits of a value, most significant first
func (b *qrBits) put(v, n int) {
	for i := n - 1; i >= 0; i-- {
		b.bit(v>>i&1 == 1)
	}
}

// Appends one binary digit
func (b *qrBits) bit(on bool) {
	if b.n/8 >= len(b.buf) {
		b.buf = append(b.buf, 0)
	}
	if on {
		b.buf[b.n/8] |= 0x80 >> (b.n % 8)
	}
	b.n++
}

// Writes the header and payload of every segment at a version
func writeSegments(b *qrBits, segs []qrSegment, version int) {
	for _, s := range segs {
		b.put(s.mode, 4)
		b.put(len(s.data), qrCountBits(s.mode, version))
		switch s.mode {
		case qrNumber:
			for i := 0; i < len(s.data); i += 3 {
				chunk := s.data[i:min(i+3, len(s.data))]
				v := 0
				for _, c := range chunk {
					v = v*10 + int(c-'0')
				}
				b.put(v, []int{0, 4, 7, 10}[len(chunk)])
			}
		case qrAlphaNum:
			for i := 0; i < len(s.data); i += 2 {
				if i+1 < len(s.data) {
					b.put(strings.IndexByte(qrAlphabet, s.data[i])*45+strings.IndexByte(qrAlphabet, s.data[i+1]), 11)
				} else {
					b.put(strings.IndexByte(qrAlphabet, s.data[i]), 6)
				}
			}
		default:
			for _, c := range s.data {
				b.put(int(c), 8)
			}
		}
	}
}

// Runs of twenty or more digits and of alphanumeric mode characters
var (
	qrDigitRun = regexp.MustCompile(`[0-9]{20,}`)
	qrAlphaRun = regexp.MustCompile(`[0-9A-Z $%*+\-./:]{20,}`)
	qrDigits   = regexp.MustCompile(`^[0-9]+`)
	qrAlphas   = regexp.MustCompile(`^[0-9A-Z $%*+\-./:]+`)
)

// Finder for short data matching only all of it or all but a final newline
func wholeMatch(head *regexp.Regexp) func([]byte) []int {
	return func(data []byte) []int {
		loc := head.FindIndex(data)
		if loc == nil || !(loc[1] == len(data) || (loc[1] == len(data)-1 && data[loc[1]] == '\n')) {
			return nil
		}
		return loc
	}
}

// Piece of data and whether a finder matched it
type qrPiece struct {
	match bool
	data  []byte
}

// Splits data into matched runs and the text between them
func splitRuns(data []byte, find func([]byte) []int) []qrPiece {
	var out []qrPiece
	for len(data) > 0 {
		loc := find(data)
		if loc == nil {
			break
		}
		if loc[0] > 0 {
			out = append(out, qrPiece{false, data[:loc[0]]})
		}
		out = append(out, qrPiece{true, data[loc[0]:loc[1]]})
		data = data[loc[1]:]
	}
	if len(data) > 0 {
		out = append(out, qrPiece{false, data})
	}
	return out
}

// Segments in the most compact modes, runs shorter than the minimum staying in the surrounding mode
func optimalSegments(data []byte, minimum int) []qrSegment {
	digits, alphas := qrDigitRun.FindIndex, qrAlphaRun.FindIndex
	if len(data) <= minimum {
		digits, alphas = wholeMatch(qrDigits), wholeMatch(qrAlphas)
	}
	var out []qrSegment
	for _, p := range splitRuns(data, digits) {
		if p.match {
			out = append(out, qrSegment{p.data, qrNumber})
			continue
		}
		for _, q := range splitRuns(p.data, alphas) {
			mode := qrByte
			if q.match {
				mode = qrAlphaNum
			}
			out = append(out, qrSegment{q.data, mode})
		}
	}
	return out
}

// Smallest version from start whose data capacity holds the segments, refit when the count widths change
func qrFit(segs []qrSegment, level, start int) (int, error) {
	b := &qrBits{}
	writeSegments(b, segs, start)
	version := start
	for version <= 40 && qrCapacity(version, level)*8 < b.n {
		version++
	}
	if version > 40 {
		return 0, ErrQRTooLong
	}
	if qrSizeClass(version) != qrSizeClass(start) {
		return qrFit(segs, level, version)
	}
	return version, nil
}

// Data codewords a version and level hold
func qrCapacity(version, level int) int {
	n := 0
	for _, b := range qrBlocks(version, level) {
		n += b[1]
	}
	return n
}

// Total and data codewords of every block of a version and level
func qrBlocks(version, level int) [][2]int {
	row := qrBlockTable[(version-1)*4+level]
	var out [][2]int
	for i := 0; i < len(row); i += 3 {
		for n := 0; n < row[i]; n++ {
			out = append(out, [2]int{row[i+1], row[i+2]})
		}
	}
	return out
}

// Exponent and logarithm tables of GF(256) over the QR polynomial
var gfExp, gfLog = func() ([256]int, [256]int) {
	var exp, log [256]int
	for i := 0; i < 8; i++ {
		exp[i] = 1 << i
	}
	for i := 8; i < 256; i++ {
		exp[i] = exp[i-4] ^ exp[i-5] ^ exp[i-6] ^ exp[i-8]
	}
	for i := 0; i < 255; i++ {
		log[exp[i]] = i
	}
	return exp, log
}()

// Field exponent with a non negative remainder
func gexp(n int) int { return gfExp[((n%255)+255)%255] }

// Error correction codewords of one block of data
func rsRemainder(data []int, ecCount int) []int {
	gen := []int{1}
	for i := 0; i < ecCount; i++ {
		next := make([]int, len(gen)+1)
		for j, g := range gen {
			next[j] ^= g
			if g != 0 {
				next[j+1] ^= gexp(gfLog[g] + i)
			}
		}
		gen = next
	}
	rem := append(append([]int(nil), data...), make([]int, ecCount)...)
	for i := 0; i < len(data); i++ {
		c := rem[i]
		if c == 0 {
			continue
		}
		for j, g := range gen {
			if g != 0 {
				rem[i+j] ^= gexp(gfLog[g] + gfLog[c])
			}
		}
	}
	return rem[len(data):]
}

// Interleaved data and error correction codewords of the segments at a version
func qrCodewords(version, level int, segs []qrSegment) ([]int, error) {
	b := &qrBits{}
	writeSegments(b, segs, version)
	blocks := qrBlocks(version, level)
	limit := qrCapacity(version, level) * 8
	if b.n > limit {
		return nil, ErrQRTooLong
	}
	for i := 0; i < min(limit-b.n, 4); i++ {
		b.bit(false)
	}
	for b.n%8 != 0 {
		b.bit(false)
	}
	for i := 0; b.n < limit; i++ {
		b.put([]int{0xec, 0x11}[i%2], 8)
	}
	var dcs, ecs [][]int
	off, maxDC, maxEC := 0, 0, 0
	for _, blk := range blocks {
		dc := make([]int, blk[1])
		for i := range dc {
			dc[i] = int(b.buf[off+i])
		}
		off += blk[1]
		ec := rsRemainder(dc, blk[0]-blk[1])
		dcs, ecs = append(dcs, dc), append(ecs, ec)
		maxDC, maxEC = max(maxDC, len(dc)), max(maxEC, len(ec))
	}
	var out []int
	for i := 0; i < maxDC; i++ {
		for _, dc := range dcs {
			if i < len(dc) {
				out = append(out, dc[i])
			}
		}
	}
	for i := 0; i < maxEC; i++ {
		for _, ec := range ecs {
			if i < len(ec) {
				out = append(out, ec[i])
			}
		}
	}
	return out, nil
}

// Bit count of a value
func bchDigit(v int) int {
	n := 0
	for v != 0 {
		n++
		v >>= 1
	}
	return n
}

// Format information of a level and mask with its BCH code and mask pattern
func bchFormat(data int) int {
	const g15, mask = 0x537, 0x5412
	d := data << 10
	for bchDigit(d)-bchDigit(g15) >= 0 {
		d ^= g15 << (bchDigit(d) - bchDigit(g15))
	}
	return (data<<10 | d) ^ mask
}

// Version information of versions 7 and up with its BCH code
func bchVersion(data int) int {
	const g18 = 0x1f25
	d := data << 12
	for bchDigit(d)-bchDigit(g18) >= 0 {
		d ^= g18 << (bchDigit(d) - bchDigit(g18))
	}
	return data<<12 | d
}

// Mask predicate of a pattern number
func qrMask(pattern, i, j int) bool {
	switch pattern {
	case 0:
		return (i+j)%2 == 0
	case 1:
		return i%2 == 0
	case 2:
		return j%3 == 0
	case 3:
		return (i+j)%3 == 0
	case 4:
		return (i/2+j/3)%2 == 0
	case 5:
		return (i*j)%2+(i*j)%3 == 0
	case 6:
		return ((i*j)%2+(i*j)%3)%2 == 0
	}
	return ((i*j)%3+(i+j)%2)%2 == 0
}

// Module matrix of a version with function patterns, format and version information and the masked data
func qrLayout(version, levelBits, mask int, data []int, test bool) [][]bool {
	n := version*4 + 17
	cells := make([][]int8, n)
	for r := range cells {
		cells[r] = make([]int8, n)
		for c := range cells[r] {
			cells[r][c] = -1
		}
	}
	set := func(r, c int, on bool) {
		cells[r][c] = 0
		if on {
			cells[r][c] = 1
		}
	}
	for _, at := range [][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} {
		for r := -1; r < 8; r++ {
			for c := -1; c < 8; c++ {
				rr, cc := at[0]+r, at[1]+c
				if rr <= -1 || rr >= n || cc <= -1 || cc >= n {
					continue
				}
				set(rr, cc, (0 <= r && r <= 6 && (c == 0 || c == 6)) || (0 <= c && c <= 6 && (r == 0 || r == 6)) || (2 <= r && r <= 4 && 2 <= c && c <= 4))
			}
		}
	}
	pos := qrAlignments[version-1]
	for _, row := range pos {
		for _, col := range pos {
			if cells[row][col] != -1 {
				continue
			}
			for r := -2; r < 3; r++ {
				for c := -2; c < 3; c++ {
					set(row+r, col+c, r == -2 || r == 2 || c == -2 || c == 2 || (r == 0 && c == 0))
				}
			}
		}
	}
	for i := 8; i < n-8; i++ {
		if cells[i][6] == -1 {
			set(i, 6, i%2 == 0)
		}
		if cells[6][i] == -1 {
			set(6, i, i%2 == 0)
		}
	}
	bits := bchFormat(levelBits<<3 | mask)
	for i := 0; i < 15; i++ {
		on := !test && bits>>i&1 == 1
		switch {
		case i < 6:
			set(i, 8, on)
		case i < 8:
			set(i+1, 8, on)
		default:
			set(n-15+i, 8, on)
		}
	}
	for i := 0; i < 15; i++ {
		on := !test && bits>>i&1 == 1
		switch {
		case i < 8:
			set(8, n-i-1, on)
		case i < 9:
			set(8, 15-i, on)
		default:
			set(8, 15-i-1, on)
		}
	}
	set(n-8, 8, !test)
	if version >= 7 {
		vb := bchVersion(version)
		for i := 0; i < 18; i++ {
			on := !test && vb>>i&1 == 1
			set(i/3, i%3+n-11, on)
			set(i%3+n-11, i/3, on)
		}
	}
	inc, row, bitIndex, byteIndex := -1, n-1, 7, 0
	for pair := n - 1; pair > 0; pair -= 2 {
		col := pair
		if col <= 6 {
			col--
		}
		for {
			for _, c := range [2]int{col, col - 1} {
				if cells[row][c] != -1 {
					continue
				}
				dark := byteIndex < len(data) && data[byteIndex]>>bitIndex&1 == 1
				set(row, c, dark != qrMask(mask, row, c))
				bitIndex--
				if bitIndex == -1 {
					byteIndex++
					bitIndex = 7
				}
			}
			row += inc
			if row < 0 || row >= n {
				row -= inc
				inc = -inc
				break
			}
		}
	}
	out := make([][]bool, n)
	for r := range cells {
		out[r] = make([]bool, n)
		for c, v := range cells[r] {
			out[r][c] = v == 1
		}
	}
	return out
}

// Penalty of a matrix under the four rules of the QR standard
func qrPenalty(m [][]bool) int {
	n := len(m)
	lost := 0
	runs := make([]int, n+1)
	count := func(at func(i, j int) bool) {
		for i := 0; i < n; i++ {
			prev, length := at(i, 0), 0
			for j := 0; j < n; j++ {
				if at(i, j) == prev {
					length++
					continue
				}
				if length >= 5 {
					runs[length]++
				}
				length, prev = 1, at(i, j)
			}
			if length >= 5 {
				runs[length]++
			}
		}
	}
	count(func(i, j int) bool { return m[i][j] })
	count(func(i, j int) bool { return m[j][i] })
	for l := 5; l <= n; l++ {
		lost += runs[l] * (l - 2)
	}
	for r := 0; r < n-1; r++ {
		for c := 0; c < n-1; c++ {
			tr := m[r][c+1]
			if tr != m[r+1][c+1] {
				c++
				continue
			}
			if tr == m[r][c] && tr == m[r+1][c] {
				lost += 3
			}
		}
	}
	finder := func(at func(k int) bool) bool {
		return !at(1) && at(4) && !at(5) && at(6) && !at(9) &&
			(at(0) && at(2) && at(3) && !at(7) && !at(8) && !at(10) ||
				!at(0) && !at(2) && !at(3) && at(7) && at(8) && at(10))
	}
	for r := 0; r < n; r++ {
		for c := 0; c < n-10; c++ {
			if finder(func(k int) bool { return m[r][c+k] }) {
				lost += 40
			}
			if m[r][c+10] {
				c++
			}
		}
	}
	for c := 0; c < n; c++ {
		for r := 0; r < n-10; r++ {
			if finder(func(k int) bool { return m[r+k][c] }) {
				lost += 40
			}
			if m[r+10][c] {
				r++
			}
		}
	}
	dark := 0
	for _, row := range m {
		for _, v := range row {
			if v {
				dark++
			}
		}
	}
	percent := float64(dark) / float64(n*n)
	return lost + int(math.Abs(percent*100-50)/5)*10
}

// Module rows framed by the border, true for dark
func (q *QR) matrix() [][]bool {
	n := len(q.modules) + 2*q.border
	out := make([][]bool, n)
	for r := range out {
		out[r] = make([]bool, n)
		if mr := r - q.border; mr >= 0 && mr < len(q.modules) {
			copy(out[r][q.border:], q.modules[mr])
		}
	}
	return out
}

// Half block rendering two module rows per line with no break spaces for light pairs, inverted to draw light modules
func (q *QR) ASCII(invert bool) string {
	codes := []string{" ", "▀", "▄", "█"}
	if invert {
		codes = []string{"█", "▄", "▀", " "}
	}
	n := len(q.modules)
	module := func(x, y int) int {
		if invert && q.border > 0 && max(x, y) >= n+q.border {
			return 1
		}
		if min(x, y) < 0 || max(x, y) >= n {
			return 0
		}
		if q.modules[x][y] {
			return 1
		}
		return 0
	}
	var b strings.Builder
	for r := -q.border; r < n+q.border; r += 2 {
		for c := -q.border; c < n+q.border; c++ {
			b.WriteString(codes[module(r, c)+module(r+1, c)<<1])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Half block rendering of the bordered matrix, dark modules inked, each line after the prefix and lines joined by newlines
func (q *QR) HalfBlocks(prefix string) string {
	m := q.matrix()
	glyph := map[[2]bool]string{{true, true}: "█", {true, false}: "▀", {false, true}: "▄", {false, false}: " "}
	lines := make([]string, 0, (len(m)+1)/2)
	for r := 0; r < len(m); r += 2 {
		var b strings.Builder
		b.WriteString(prefix)
		for c := range m[r] {
			bottom := false
			if r+1 < len(m) {
				bottom = m[r+1][c]
			}
			b.WriteString(glyph[[2]bool{m[r][c], bottom}])
		}
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n")
}

// Half block rendering with light modules drawn as full blocks inside a one module light frame
func (q *QR) Compact() string {
	n := len(q.modules)
	rows := q.modules
	if n%2 == 1 {
		rows = append(append([][]bool(nil), rows...), make([]bool, n))
	}
	var b strings.Builder
	b.WriteString(strings.Repeat("▄", n+2))
	b.WriteByte('\n')
	for r := 0; r < n; r += 2 {
		b.WriteString("█")
		for c := 0; c < n; c++ {
			top, bottom := rows[r][c], rows[r+1][c]
			switch {
			case !top && !bottom:
				b.WriteString("█")
			case !top && bottom:
				b.WriteString("▀")
			case top && !bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("█\n")
	}
	if n%2 == 0 {
		b.WriteString(strings.Repeat("▀", n+2))
	}
	return b.String()
}

// Alignment pattern centres per version
var qrAlignments = [][]int{
	{}, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
	{6, 30, 54}, {6, 32, 58}, {6, 34, 62}, {6, 26, 46, 66}, {6, 26, 48, 70}, {6, 26, 50, 74}, {6, 30, 54, 78},
	{6, 30, 56, 82}, {6, 30, 58, 86}, {6, 34, 62, 90}, {6, 28, 50, 72, 94}, {6, 26, 50, 74, 98},
	{6, 30, 54, 78, 102}, {6, 28, 54, 80, 106}, {6, 32, 58, 84, 110}, {6, 30, 58, 86, 114}, {6, 34, 62, 90, 118},
	{6, 26, 50, 74, 98, 122}, {6, 30, 54, 78, 102, 126}, {6, 26, 52, 78, 104, 130}, {6, 30, 56, 82, 108, 134},
	{6, 34, 60, 86, 112, 138}, {6, 30, 58, 86, 114, 142}, {6, 34, 62, 90, 118, 146}, {6, 30, 54, 78, 102, 126, 150},
	{6, 24, 50, 76, 102, 128, 154}, {6, 28, 54, 80, 106, 132, 158}, {6, 32, 58, 84, 110, 136, 162},
	{6, 26, 54, 82, 110, 138, 166}, {6, 30, 58, 86, 114, 142, 170},
}

// Block count, total and data codewords per version and level in L, M, Q, H order
var qrBlockTable = [][]int{
	{1, 26, 19}, {1, 26, 16}, {1, 26, 13}, {1, 26, 9},
	{1, 44, 34}, {1, 44, 28}, {1, 44, 22}, {1, 44, 16},
	{1, 70, 55}, {1, 70, 44}, {2, 35, 17}, {2, 35, 13},
	{1, 100, 80}, {2, 50, 32}, {2, 50, 24}, {4, 25, 9},
	{1, 134, 108}, {2, 67, 43}, {2, 33, 15, 2, 34, 16}, {2, 33, 11, 2, 34, 12},
	{2, 86, 68}, {4, 43, 27}, {4, 43, 19}, {4, 43, 15},
	{2, 98, 78}, {4, 49, 31}, {2, 32, 14, 4, 33, 15}, {4, 39, 13, 1, 40, 14},
	{2, 121, 97}, {2, 60, 38, 2, 61, 39}, {4, 40, 18, 2, 41, 19}, {4, 40, 14, 2, 41, 15},
	{2, 146, 116}, {3, 58, 36, 2, 59, 37}, {4, 36, 16, 4, 37, 17}, {4, 36, 12, 4, 37, 13},
	{2, 86, 68, 2, 87, 69}, {4, 69, 43, 1, 70, 44}, {6, 43, 19, 2, 44, 20}, {6, 43, 15, 2, 44, 16},
	{4, 101, 81}, {1, 80, 50, 4, 81, 51}, {4, 50, 22, 4, 51, 23}, {3, 36, 12, 8, 37, 13},
	{2, 116, 92, 2, 117, 93}, {6, 58, 36, 2, 59, 37}, {4, 46, 20, 6, 47, 21}, {7, 42, 14, 4, 43, 15},
	{4, 133, 107}, {8, 59, 37, 1, 60, 38}, {8, 44, 20, 4, 45, 21}, {12, 33, 11, 4, 34, 12},
	{3, 145, 115, 1, 146, 116}, {4, 64, 40, 5, 65, 41}, {11, 36, 16, 5, 37, 17}, {11, 36, 12, 5, 37, 13},
	{5, 109, 87, 1, 110, 88}, {5, 65, 41, 5, 66, 42}, {5, 54, 24, 7, 55, 25}, {11, 36, 12, 7, 37, 13},
	{5, 122, 98, 1, 123, 99}, {7, 73, 45, 3, 74, 46}, {15, 43, 19, 2, 44, 20}, {3, 45, 15, 13, 46, 16},
	{1, 135, 107, 5, 136, 108}, {10, 74, 46, 1, 75, 47}, {1, 50, 22, 15, 51, 23}, {2, 42, 14, 17, 43, 15},
	{5, 150, 120, 1, 151, 121}, {9, 69, 43, 4, 70, 44}, {17, 50, 22, 1, 51, 23}, {2, 42, 14, 19, 43, 15},
	{3, 141, 113, 4, 142, 114}, {3, 70, 44, 11, 71, 45}, {17, 47, 21, 4, 48, 22}, {9, 39, 13, 16, 40, 14},
	{3, 135, 107, 5, 136, 108}, {3, 67, 41, 13, 68, 42}, {15, 54, 24, 5, 55, 25}, {15, 43, 15, 10, 44, 16},
	{4, 144, 116, 4, 145, 117}, {17, 68, 42}, {17, 50, 22, 6, 51, 23}, {19, 46, 16, 6, 47, 17},
	{2, 139, 111, 7, 140, 112}, {17, 74, 46}, {7, 54, 24, 16, 55, 25}, {34, 37, 13},
	{4, 151, 121, 5, 152, 122}, {4, 75, 47, 14, 76, 48}, {11, 54, 24, 14, 55, 25}, {16, 45, 15, 14, 46, 16},
	{6, 147, 117, 4, 148, 118}, {6, 73, 45, 14, 74, 46}, {11, 54, 24, 16, 55, 25}, {30, 46, 16, 2, 47, 17},
	{8, 132, 106, 4, 133, 107}, {8, 75, 47, 13, 76, 48}, {7, 54, 24, 22, 55, 25}, {22, 45, 15, 13, 46, 16},
	{10, 142, 114, 2, 143, 115}, {19, 74, 46, 4, 75, 47}, {28, 50, 22, 6, 51, 23}, {33, 46, 16, 4, 47, 17},
	{8, 152, 122, 4, 153, 123}, {22, 73, 45, 3, 74, 46}, {8, 53, 23, 26, 54, 24}, {12, 45, 15, 28, 46, 16},
	{3, 147, 117, 10, 148, 118}, {3, 73, 45, 23, 74, 46}, {4, 54, 24, 31, 55, 25}, {11, 45, 15, 31, 46, 16},
	{7, 146, 116, 7, 147, 117}, {21, 73, 45, 7, 74, 46}, {1, 53, 23, 37, 54, 24}, {19, 45, 15, 26, 46, 16},
	{5, 145, 115, 10, 146, 116}, {19, 75, 47, 10, 76, 48}, {15, 54, 24, 25, 55, 25}, {23, 45, 15, 25, 46, 16},
	{13, 145, 115, 3, 146, 116}, {2, 74, 46, 29, 75, 47}, {42, 54, 24, 1, 55, 25}, {23, 45, 15, 28, 46, 16},
	{17, 145, 115}, {10, 74, 46, 23, 75, 47}, {10, 54, 24, 35, 55, 25}, {19, 45, 15, 35, 46, 16},
	{17, 145, 115, 1, 146, 116}, {14, 74, 46, 21, 75, 47}, {29, 54, 24, 19, 55, 25}, {11, 45, 15, 46, 46, 16},
	{13, 145, 115, 6, 146, 116}, {14, 74, 46, 23, 75, 47}, {44, 54, 24, 7, 55, 25}, {59, 46, 16, 1, 47, 17},
	{12, 151, 121, 7, 152, 122}, {12, 75, 47, 26, 76, 48}, {39, 54, 24, 14, 55, 25}, {22, 45, 15, 41, 46, 16},
	{6, 151, 121, 14, 152, 122}, {6, 75, 47, 34, 76, 48}, {46, 54, 24, 10, 55, 25}, {2, 45, 15, 64, 46, 16},
	{17, 152, 122, 4, 153, 123}, {29, 74, 46, 14, 75, 47}, {49, 54, 24, 10, 55, 25}, {24, 45, 15, 46, 46, 16},
	{4, 152, 122, 18, 153, 123}, {13, 74, 46, 32, 75, 47}, {48, 54, 24, 14, 55, 25}, {42, 45, 15, 32, 46, 16},
	{20, 147, 117, 4, 148, 118}, {40, 75, 47, 7, 76, 48}, {43, 54, 24, 22, 55, 25}, {10, 45, 15, 67, 46, 16},
	{19, 148, 118, 6, 149, 119}, {18, 75, 47, 31, 76, 48}, {34, 54, 24, 34, 55, 25}, {20, 45, 15, 61, 46, 16},
}
