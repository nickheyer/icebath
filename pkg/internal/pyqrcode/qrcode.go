package pyqrcode

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Error correction levels of qrcode.constants
const (
	ErrorCorrectL = 1
	ErrorCorrectM = 0
	ErrorCorrectQ = 3
	ErrorCorrectH = 2
)

// Encoding modes of qrcode.util
const (
	modeNumber   = 1 << 0
	modeAlphaNum = 1 << 1
	mode8BitByte = 1 << 2
	modeKanji    = 1 << 3
)

// Character count indicator widths per mode for versions 1 to 9, 10 to 26 and 27 to 40
var (
	modeSizeSmall  = map[int]int{modeNumber: 10, modeAlphaNum: 9, mode8BitByte: 8, modeKanji: 8}
	modeSizeMedium = map[int]int{modeNumber: 12, modeAlphaNum: 11, mode8BitByte: 16, modeKanji: 10}
	modeSizeLarge  = map[int]int{modeNumber: 14, modeAlphaNum: 13, mode8BitByte: 16, modeKanji: 12}
)

const alphaNum = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ $%*+-./:"

// Bit widths of a one, two or three digit numeric group
var numberLength = map[int]int{3: 10, 2: 7, 1: 4}

var patternPositionTable = [][]int{
	{}, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50},
	{6, 30, 54}, {6, 32, 58}, {6, 34, 62}, {6, 26, 46, 66}, {6, 26, 48, 70}, {6, 26, 50, 74}, {6, 30, 54, 78},
	{6, 30, 56, 82}, {6, 30, 58, 86}, {6, 34, 62, 90}, {6, 28, 50, 72, 94}, {6, 26, 50, 74, 98},
	{6, 30, 54, 78, 102}, {6, 28, 54, 80, 106}, {6, 32, 58, 84, 110}, {6, 30, 58, 86, 114}, {6, 34, 62, 90, 118},
	{6, 26, 50, 74, 98, 122}, {6, 30, 54, 78, 102, 126}, {6, 26, 52, 78, 104, 130}, {6, 30, 56, 82, 108, 134},
	{6, 34, 60, 86, 112, 138}, {6, 30, 58, 86, 114, 142}, {6, 34, 62, 90, 118, 146}, {6, 30, 54, 78, 102, 126, 150},
	{6, 24, 50, 76, 102, 128, 154}, {6, 28, 54, 80, 106, 132, 158}, {6, 32, 58, 84, 110, 136, 162},
	{6, 26, 54, 82, 110, 138, 166}, {6, 30, 58, 86, 114, 142, 170},
}

const (
	g15     = 1<<10 | 1<<8 | 1<<5 | 1<<4 | 1<<2 | 1<<1 | 1<<0
	g18     = 1<<12 | 1<<11 | 1<<10 | 1<<9 | 1<<8 | 1<<5 | 1<<2 | 1<<0
	g15Mask = 1<<14 | 1<<12 | 1<<10 | 1<<4 | 1<<1
	pad0    = 0xEC
	pad1    = 0x11
)

// Error a data set too large for the chosen version raises, the text qrcode prints
type DataOverflowError struct{ Msg string }

func (e *DataOverflowError) Error() string {
	if e.Msg == "" {
		return "DataOverflowError"
	}
	return "DataOverflowError: " + e.Msg
}

// ValueError of a version outside 1 to 40, what best_fit raises once the data needs more than version 40
type VersionError struct{ Version int }

func (e *VersionError) Error() string {
	return fmt.Sprintf("ValueError: Invalid version (was %d, expected 1 to 40)", e.Version)
}

var expTable, logTable = func() ([256]int, [256]int) {
	var exp, log [256]int
	for i := range log {
		log[i] = i
	}
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

// GF(256) logarithm, the ValueError of qrcode for zero
func glog(n int) (int, error) {
	if n < 1 {
		return 0, fmt.Errorf("ValueError: glog(%d)", n)
	}
	return logTable[n], nil
}

// GF(256) exponent of n modulo 255 with Python's non negative remainder
func gexp(n int) int { return expTable[((n%255)+255)%255] }

// Coefficients of a qrcode Polynomial, leading zeros dropped and shift zeros appended
type polynomial []int

// Polynomial of num shifted, keeping the last coefficient when every one is zero like the Python loop
func newPolynomial(num []int, shift int) (polynomial, error) {
	if len(num) == 0 {
		return nil, fmt.Errorf("Exception: %d/%d", len(num), shift)
	}
	offset := 0
	for offset = 0; offset < len(num); offset++ {
		if num[offset] != 0 {
			break
		}
	}
	if offset == len(num) {
		offset = len(num) - 1
	}
	out := append([]int{}, num[offset:]...)
	return append(out, make([]int, shift)...), nil
}

// Product of two polynomials over GF(256)
func (p polynomial) mul(other polynomial) (polynomial, error) {
	num := make([]int, len(p)+len(other)-1)
	for i, item := range p {
		for j, otherItem := range other {
			a, err := glog(item)
			if err != nil {
				return nil, err
			}
			b, err := glog(otherItem)
			if err != nil {
				return nil, err
			}
			num[i+j] ^= gexp(a + b)
		}
	}
	return newPolynomial(num, 0)
}

// Remainder of p divided by other over GF(256)
func (p polynomial) mod(other polynomial) (polynomial, error) {
	for {
		difference := len(p) - len(other)
		if difference < 0 {
			return p, nil
		}
		a, err := glog(p[0])
		if err != nil {
			return nil, err
		}
		b, err := glog(other[0])
		if err != nil {
			return nil, err
		}
		ratio := a - b
		num := make([]int, 0, len(p))
		for i := 0; i < len(other); i++ {
			c, err := glog(other[i])
			if err != nil {
				return nil, err
			}
			num = append(num, p[i]^gexp(c+ratio))
		}
		if difference > 0 {
			num = append(num, p[len(p)-difference:]...)
		}
		if p, err = newPolynomial(num, 0); err != nil {
			return nil, err
		}
	}
}

// Total and data codeword counts of one Reed Solomon block
type rsBlock struct{ totalCount, dataCount int }

// Block count, total and data codewords per version and level, the RS_BLOCK_TABLE of qrcode.base
var rsBlockTable = [][]int{
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

// Reed Solomon blocks of a version and error correction level
func rsBlocks(version, errorCorrection int) []rsBlock {
	offset := map[int]int{ErrorCorrectL: 0, ErrorCorrectM: 1, ErrorCorrectQ: 2, ErrorCorrectH: 3}[errorCorrection]
	row := rsBlockTable[(version-1)*4+offset]
	var blocks []rsBlock
	for i := 0; i < len(row); i += 3 {
		for n := 0; n < row[i]; n++ {
			blocks = append(blocks, rsBlock{row[i+1], row[i+2]})
		}
	}
	return blocks
}

// Data bit capacity per error correction level and version, index 0 holding 0
var bitLimitTable = func() [4][41]int {
	var table [4][41]int
	for ec := 0; ec < 4; ec++ {
		for version := 1; version <= 40; version++ {
			total := 0
			for _, b := range rsBlocks(version, ec) {
				total += b.dataCount
			}
			table[ec][version] = 8 * total
		}
	}
	return table
}()

// Number of bits of data, the position of its highest set bit
func bchDigit(data int) int {
	digit := 0
	for data != 0 {
		digit++
		data >>= 1
	}
	return digit
}

// Format information bits of the error correction level and mask
func bchTypeInfo(data int) int {
	d := data << 10
	for bchDigit(d)-bchDigit(g15) >= 0 {
		d ^= g15 << (bchDigit(d) - bchDigit(g15))
	}
	return ((data << 10) | d) ^ g15Mask
}

// Version information bits of versions 7 and up
func bchTypeNumber(data int) int {
	d := data << 12
	for bchDigit(d)-bchDigit(g18) >= 0 {
		d ^= g18 << (bchDigit(d) - bchDigit(g18))
	}
	return (data << 12) | d
}

// Mask predicate of a mask pattern
func maskFunc(pattern int) func(i, j int) bool {
	switch pattern {
	case 0:
		return func(i, j int) bool { return (i+j)%2 == 0 }
	case 1:
		return func(i, _ int) bool { return i%2 == 0 }
	case 2:
		return func(_, j int) bool { return j%3 == 0 }
	case 3:
		return func(i, j int) bool { return (i+j)%3 == 0 }
	case 4:
		return func(i, j int) bool { return (i/2+j/3)%2 == 0 }
	case 5:
		return func(i, j int) bool { return (i*j)%2+(i*j)%3 == 0 }
	case 6:
		return func(i, j int) bool { return ((i*j)%2+(i*j)%3)%2 == 0 }
	}
	return func(i, j int) bool { return ((i*j)%3+(i+j)%2)%2 == 0 }
}

// Character count indicator width class of a version, small, medium or large
func sizeClass(version int) int {
	switch {
	case version < 10:
		return 0
	case version < 27:
		return 1
	}
	return 2
}

// Character count indicator widths of a version
func modeSizesForVersion(version int) map[int]int {
	return []map[int]int{modeSizeSmall, modeSizeMedium, modeSizeLarge}[sizeClass(version)]
}

// Bit buffer the encoder writes data codewords into
type bitBuffer struct {
	buffer []int
	length int
}

func (b *bitBuffer) put(num, length int) {
	for i := 0; i < length; i++ {
		b.putBit((num>>(length-i-1))&1 == 1)
	}
}

func (b *bitBuffer) putBit(bit bool) {
	idx := b.length / 8
	if len(b.buffer) <= idx {
		b.buffer = append(b.buffer, 0)
	}
	if bit {
		b.buffer[idx] |= 0x80 >> (b.length % 8)
	}
	b.length++
}

// One data segment in a QR mode
type qrData struct {
	data []byte
	mode int
}

// Writes the segment bits of its mode
func (d qrData) write(buf *bitBuffer) {
	switch d.mode {
	case modeNumber:
		for i := 0; i < len(d.data); i += 3 {
			chars := d.data[i:min(i+3, len(d.data))]
			n := 0
			for _, c := range chars {
				n = n*10 + int(c-'0')
			}
			buf.put(n, numberLength[len(chars)])
		}
	case modeAlphaNum:
		for i := 0; i < len(d.data); i += 2 {
			chars := d.data[i:min(i+2, len(d.data))]
			if len(chars) > 1 {
				buf.put(strings.IndexByte(alphaNum, chars[0])*45+strings.IndexByte(alphaNum, chars[1]), 11)
			} else {
				buf.put(strings.Index(alphaNum, string(chars)), 6)
			}
		}
	default:
		for _, c := range d.data {
			buf.put(int(c), 8)
		}
	}
}

var (
	digitRun    = regexp.MustCompile(`[0-9]{20,}`)
	alphaNumRun = regexp.MustCompile(`[0-9A-Z $%*+\-./:]{20,}`)
	digitHead   = regexp.MustCompile(`^[0-9]+`)
	alphaHead   = regexp.MustCompile(`^[0-9A-Z $%*+\-./:]+`)
)

// Span of a whole string match like a pattern anchored by ^ and $, a final newline allowed
func anchoredMatch(head *regexp.Regexp) func(data []byte) []int {
	return func(data []byte) []int {
		loc := head.FindIndex(data)
		if loc == nil || !(loc[1] == len(data) || (loc[1] == len(data)-1 && data[loc[1]] == '\n')) {
			return nil
		}
		return loc
	}
}

// Splits data into runs the pattern matches and the text between them
func optimalSplit(data []byte, find func([]byte) []int) []struct {
	match bool
	chunk []byte
} {
	var out []struct {
		match bool
		chunk []byte
	}
	for len(data) > 0 {
		loc := find(data)
		if loc == nil {
			break
		}
		if loc[0] > 0 {
			out = append(out, struct {
				match bool
				chunk []byte
			}{false, data[:loc[0]]})
		}
		out = append(out, struct {
			match bool
			chunk []byte
		}{true, data[loc[0]:loc[1]]})
		data = data[loc[1]:]
	}
	if len(data) > 0 {
		out = append(out, struct {
			match bool
			chunk []byte
		}{false, data})
	}
	return out
}

// Data segments in the most compact modes, runs shorter than minimum staying in the surrounding mode
func optimalDataChunks(data []byte, minimum int) []qrData {
	numFind, alphaFind := digitRun.FindIndex, alphaNumRun.FindIndex
	if len(data) <= minimum {
		numFind, alphaFind = anchoredMatch(digitHead), anchoredMatch(alphaHead)
	}
	var out []qrData
	for _, part := range optimalSplit(data, numFind) {
		if part.match {
			out = append(out, qrData{part.chunk, modeNumber})
			continue
		}
		for _, sub := range optimalSplit(part.chunk, alphaFind) {
			mode := mode8BitByte
			if sub.match {
				mode = modeAlphaNum
			}
			out = append(out, qrData{sub.chunk, mode})
		}
	}
	return out
}

// Interleaved data and error correction codewords of the encoded segments
func createBytes(buf *bitBuffer, blocks []rsBlock) ([]int, error) {
	offset, maxDc, maxEc := 0, 0, 0
	var dcdata, ecdata [][]int
	for _, block := range blocks {
		dcCount, ecCount := block.dataCount, block.totalCount-block.dataCount
		maxDc, maxEc = max(maxDc, dcCount), max(maxEc, ecCount)
		current := make([]int, dcCount)
		for i := range current {
			current[i] = 0xFF & buf.buffer[i+offset]
		}
		offset += dcCount
		rsPoly := polynomial{1}
		for i := 0; i < ecCount; i++ {
			factor, err := newPolynomial([]int{1, gexp(i)}, 0)
			if err != nil {
				return nil, err
			}
			if rsPoly, err = rsPoly.mul(factor); err != nil {
				return nil, err
			}
		}
		raw, err := newPolynomial(current, len(rsPoly)-1)
		if err != nil {
			return nil, err
		}
		modPoly, err := raw.mod(rsPoly)
		if err != nil {
			return nil, err
		}
		ec := make([]int, ecCount)
		modOffset := len(modPoly) - ecCount
		for i := range ec {
			if idx := i + modOffset; idx >= 0 {
				ec[i] = modPoly[idx]
			}
		}
		dcdata, ecdata = append(dcdata, current), append(ecdata, ec)
	}
	var data []int
	for i := 0; i < maxDc; i++ {
		for _, dc := range dcdata {
			if i < len(dc) {
				data = append(data, dc[i])
			}
		}
	}
	for i := 0; i < maxEc; i++ {
		for _, ec := range ecdata {
			if i < len(ec) {
				data = append(data, ec[i])
			}
		}
	}
	return data, nil
}

// Codewords of the segments at a version, terminated and padded to the version capacity
func createData(version, errorCorrection int, segments []qrData) ([]int, error) {
	buf := &bitBuffer{}
	sizes := modeSizesForVersion(version)
	for _, d := range segments {
		buf.put(d.mode, 4)
		buf.put(len(d.data), sizes[d.mode])
		d.write(buf)
	}
	blocks := rsBlocks(version, errorCorrection)
	bitLimit := 0
	for _, b := range blocks {
		bitLimit += b.dataCount * 8
	}
	if buf.length > bitLimit {
		return nil, &DataOverflowError{fmt.Sprintf("Code length overflow. Data size (%d) > size available (%d)", buf.length, bitLimit)}
	}
	for i := 0; i < min(bitLimit-buf.length, 4); i++ {
		buf.putBit(false)
	}
	if delimit := buf.length % 8; delimit != 0 {
		for i := 0; i < 8-delimit; i++ {
			buf.putBit(false)
		}
	}
	fill := (bitLimit - buf.length) / 8
	for i := 0; i < fill; i++ {
		if i%2 == 0 {
			buf.put(pad0, 8)
		} else {
			buf.put(pad1, 8)
		}
	}
	return createBytes(buf, blocks)
}

// Penalty score of a module matrix, the sum of the four ISO 18004 rules as qrcode counts them
func lostPoint(modules [][]bool) int {
	n := len(modules)
	return lostPointLevel1(modules, n) + lostPointLevel2(modules, n) + lostPointLevel3(modules, n) + lostPointLevel4(modules, n)
}

func lostPointLevel1(modules [][]bool, n int) int {
	container := make([]int, n+1)
	count := func(at func(i, j int) bool) {
		for i := 0; i < n; i++ {
			previous, length := at(i, 0), 0
			for j := 0; j < n; j++ {
				if at(i, j) == previous {
					length++
				} else {
					if length >= 5 {
						container[length]++
					}
					length, previous = 1, at(i, j)
				}
			}
			if length >= 5 {
				container[length]++
			}
		}
	}
	count(func(i, j int) bool { return modules[i][j] })
	count(func(i, j int) bool { return modules[j][i] })
	lost := 0
	for length := 5; length <= n; length++ {
		lost += container[length] * (length - 2)
	}
	return lost
}

func lostPointLevel2(modules [][]bool, n int) int {
	lost := 0
	for row := 0; row < n-1; row++ {
		this, next := modules[row], modules[row+1]
		for col := 0; col < n-1; col++ {
			topRight := this[col+1]
			if topRight != next[col+1] {
				col++
				continue
			}
			if topRight == this[col] && topRight == next[col] {
				lost += 3
			}
		}
	}
	return lost
}

func lostPointLevel3(modules [][]bool, n int) int {
	lost := 0
	finder := func(at func(k int) bool) bool {
		return !at(1) && at(4) && !at(5) && at(6) && !at(9) &&
			(at(0) && at(2) && at(3) && !at(7) && !at(8) && !at(10) ||
				!at(0) && !at(2) && !at(3) && at(7) && at(8) && at(10))
	}
	for row := 0; row < n; row++ {
		for col := 0; col < n-10; col++ {
			if finder(func(k int) bool { return modules[row][col+k] }) {
				lost += 40
			}
			if modules[row][col+10] {
				col++
			}
		}
	}
	for col := 0; col < n; col++ {
		for row := 0; row < n-10; row++ {
			if finder(func(k int) bool { return modules[row+k][col] }) {
				lost += 40
			}
			if modules[row+10][col] {
				row++
			}
		}
	}
	return lost
}

func lostPointLevel4(modules [][]bool, n int) int {
	dark := 0
	for _, row := range modules {
		for _, m := range row {
			if m {
				dark++
			}
		}
	}
	percent := float64(dark) / float64(n*n)
	return int(math.Abs(percent*100-50)/5) * 10
}

// QR symbol built like qrcode.QRCode with box size 1, the fitted version and the best mask
type Code struct {
	version, errorCorrection, border int
	count                            int
	cells                            [][]int8
	segments                         []qrData
	dataCache                        []int
}

// Fits data into the smallest version of the level and applies the best mask like qrcode add_data and make
func Make(data string, errorCorrection, border int) (*Code, error) {
	if err := pystr.StrictUTF8EncodeError(data); err != nil {
		return nil, err
	}
	c := &Code{errorCorrection: errorCorrection, border: border, segments: optimalDataChunks([]byte(data), 20)}
	version, err := c.bestFit(1)
	if err != nil {
		return nil, err
	}
	c.version = version
	pattern, err := c.bestMaskPattern()
	if err != nil {
		return nil, err
	}
	if err := c.makeImpl(false, pattern); err != nil {
		return nil, err
	}
	return c, nil
}

// Smallest version from start whose capacity holds the segments, refitting when the count indicator widths change
func (c *Code) bestFit(start int) (int, error) {
	if start < 1 || start > 40 {
		return 0, &VersionError{start}
	}
	sizes := modeSizesForVersion(start)
	buf := &bitBuffer{}
	for _, d := range c.segments {
		buf.put(d.mode, 4)
		buf.put(len(d.data), sizes[d.mode])
		d.write(buf)
	}
	limits := bitLimitTable[c.errorCorrection]
	version := start
	for version < len(limits) && limits[version] < buf.length {
		version++
	}
	if version > 40 {
		return 0, &VersionError{version}
	}
	if sizeClass(version) != sizeClass(start) {
		return c.bestFit(version)
	}
	return version, nil
}

// Mask pattern with the lowest penalty, the first one winning ties
func (c *Code) bestMaskPattern() (int, error) {
	minLost, pattern := 0, 0
	for i := 0; i < 8; i++ {
		if err := c.makeImpl(true, i); err != nil {
			return 0, err
		}
		lost := lostPoint(c.matrix())
		if i == 0 || minLost > lost {
			minLost, pattern = lost, i
		}
	}
	return pattern, nil
}

// Module matrix with every cell decided, false for any left unset
func (c *Code) matrix() [][]bool {
	out := make([][]bool, c.count)
	for r, row := range c.cells {
		out[r] = make([]bool, c.count)
		for col, v := range row {
			out[r][col] = v == 1
		}
	}
	return out
}

func boolCell(b bool) int8 {
	if b {
		return 1
	}
	return 0
}

// Lays out the function patterns, format and version information and the masked data
func (c *Code) makeImpl(test bool, mask int) error {
	c.count = c.version*4 + 17
	c.cells = make([][]int8, c.count)
	for r := range c.cells {
		c.cells[r] = make([]int8, c.count)
		for col := range c.cells[r] {
			c.cells[r][col] = -1
		}
	}
	c.setupPositionProbePattern(0, 0)
	c.setupPositionProbePattern(c.count-7, 0)
	c.setupPositionProbePattern(0, c.count-7)
	c.setupPositionAdjustPattern()
	c.setupTimingPattern()
	c.setupTypeInfo(test, mask)
	if c.version >= 7 {
		c.setupTypeNumber(test)
	}
	if c.dataCache == nil {
		data, err := createData(c.version, c.errorCorrection, c.segments)
		if err != nil {
			return err
		}
		c.dataCache = data
	}
	c.mapData(c.dataCache, mask)
	return nil
}

func (c *Code) setupPositionProbePattern(row, col int) {
	for r := -1; r < 8; r++ {
		if row+r <= -1 || c.count <= row+r {
			continue
		}
		for cc := -1; cc < 8; cc++ {
			if col+cc <= -1 || c.count <= col+cc {
				continue
			}
			on := (0 <= r && r <= 6 && (cc == 0 || cc == 6)) || (0 <= cc && cc <= 6 && (r == 0 || r == 6)) ||
				(2 <= r && r <= 4 && 2 <= cc && cc <= 4)
			c.cells[row+r][col+cc] = boolCell(on)
		}
	}
}

func (c *Code) setupTimingPattern() {
	for r := 8; r < c.count-8; r++ {
		if c.cells[r][6] == -1 {
			c.cells[r][6] = boolCell(r%2 == 0)
		}
	}
	for col := 8; col < c.count-8; col++ {
		if c.cells[6][col] == -1 {
			c.cells[6][col] = boolCell(col%2 == 0)
		}
	}
}

func (c *Code) setupPositionAdjustPattern() {
	pos := patternPositionTable[c.version-1]
	for _, row := range pos {
		for _, col := range pos {
			if c.cells[row][col] != -1 {
				continue
			}
			for r := -2; r < 3; r++ {
				for cc := -2; cc < 3; cc++ {
					c.cells[row+r][col+cc] = boolCell(r == -2 || r == 2 || cc == -2 || cc == 2 || (r == 0 && cc == 0))
				}
			}
		}
	}
}

func (c *Code) setupTypeNumber(test bool) {
	bits := bchTypeNumber(c.version)
	for i := 0; i < 18; i++ {
		c.cells[i/3][i%3+c.count-8-3] = boolCell(!test && (bits>>i)&1 == 1)
	}
	for i := 0; i < 18; i++ {
		c.cells[i%3+c.count-8-3][i/3] = boolCell(!test && (bits>>i)&1 == 1)
	}
}

func (c *Code) setupTypeInfo(test bool, mask int) {
	bits := bchTypeInfo(c.errorCorrection<<3 | mask)
	for i := 0; i < 15; i++ {
		mod := boolCell(!test && (bits>>i)&1 == 1)
		switch {
		case i < 6:
			c.cells[i][8] = mod
		case i < 8:
			c.cells[i+1][8] = mod
		default:
			c.cells[c.count-15+i][8] = mod
		}
	}
	for i := 0; i < 15; i++ {
		mod := boolCell(!test && (bits>>i)&1 == 1)
		switch {
		case i < 8:
			c.cells[8][c.count-i-1] = mod
		case i < 9:
			c.cells[8][15-i-1+1] = mod
		default:
			c.cells[8][15-i-1] = mod
		}
	}
	c.cells[c.count-8][8] = boolCell(!test)
}

func (c *Code) mapData(data []int, mask int) {
	inc, row, bitIndex, byteIndex := -1, c.count-1, 7, 0
	maskAt := maskFunc(mask)
	for col := c.count - 1; col > 0; col -= 2 {
		left := col
		if left <= 6 {
			left--
		}
		for {
			for _, cc := range []int{left, left - 1} {
				if c.cells[row][cc] != -1 {
					continue
				}
				dark := false
				if byteIndex < len(data) {
					dark = (data[byteIndex]>>bitIndex)&1 == 1
				}
				if maskAt(row, cc) {
					dark = !dark
				}
				c.cells[row][cc] = boolCell(dark)
				bitIndex--
				if bitIndex == -1 {
					byteIndex++
					bitIndex = 7
				}
			}
			row += inc
			if row < 0 || c.count <= row {
				row -= inc
				inc = -inc
				break
			}
		}
	}
}

// Fitted symbol version
func (c *Code) Version() int { return c.version }

// Half block art of the symbol with its border, the output of print_ascii without a tty
func (c *Code) PrintASCII(invert bool) string {
	codes := []string{" ", "▀", "▄", "█"}
	if invert {
		codes = []string{"█", "▄", "▀", " "}
	}
	module := func(x, y int) int {
		if invert && c.border != 0 && max(x, y) >= c.count+c.border {
			return 1
		}
		if min(x, y) < 0 || max(x, y) >= c.count {
			return 0
		}
		return int(c.cells[x][y])
	}
	var b strings.Builder
	for r := -c.border; r < c.count+c.border; r += 2 {
		for col := -c.border; col < c.count+c.border; col++ {
			b.WriteString(codes[module(r, col)+module(r+1, col)<<1])
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Module matrix with the border modules around it, the get_matrix of qrcode
func (c *Code) Matrix() [][]bool {
	modules := c.matrix()
	if c.border == 0 {
		return modules
	}
	width := c.count + c.border*2
	var out [][]bool
	for i := 0; i < c.border; i++ {
		out = append(out, make([]bool, width))
	}
	for _, row := range modules {
		line := make([]bool, 0, width)
		line = append(line, make([]bool, c.border)...)
		line = append(line, row...)
		out = append(out, append(line, make([]bool, c.border)...))
	}
	for i := 0; i < c.border; i++ {
		out = append(out, make([]bool, width))
	}
	return out
}
