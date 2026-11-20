package pycodecs

import (
	"sort"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Inclusive code point ranges of one stringprep table
type cpRanges [][2]rune

func (rs cpRanges) has(r rune) bool {
	i := sort.Search(len(rs), func(i int) bool { return rs[i][1] >= r })
	return i < len(rs) && rs[i][0] <= r
}

// Stringprep and Unicode 3.2 normalization data of CPython's ucd_3_2_0
type nameprepData struct {
	b1, prohibited, randAL, lCat cpRanges
	b2, nfkd                     map[rune][]rune
	ccc                          map[rune]int
	compose                      map[[2]rune]rune
}

func readRanges(name string) cpRanges {
	var out cpRanges
	r := &blobReader{b: blobBytes(nameprepBlobs, "nameprep", name)}
	for r.more() {
		lo, hi := r.u24(), r.u24()
		out = append(out, [2]rune{lo, hi})
	}
	return out
}

func readRuneMap(name string) map[rune][]rune {
	out := map[rune][]rune{}
	r := &blobReader{b: blobBytes(nameprepBlobs, "nameprep", name)}
	for r.more() {
		cp := r.u24()
		n := r.u8()
		seq := make([]rune, n)
		for i := range seq {
			seq[i] = r.u24()
		}
		out[cp] = seq
	}
	return out
}

var nameprepTables = sync.OnceValue(func() *nameprepData {
	d := &nameprepData{b1: readRanges("b1"), prohibited: readRanges("prohibited"), randAL: readRanges("randal"),
		lCat: readRanges("lcat"), b2: readRuneMap("b2"), nfkd: readRuneMap("nfkd"), ccc: map[rune]int{}, compose: map[[2]rune]rune{}}
	r := &blobReader{b: blobBytes(nameprepBlobs, "nameprep", "ccc")}
	for r.more() {
		cp := r.u24()
		d.ccc[cp] = r.u8()
	}
	r = &blobReader{b: blobBytes(nameprepBlobs, "nameprep", "compose")}
	for r.more() {
		a, b, c := r.u24(), r.u24(), r.u24()
		d.compose[[2]rune{a, b}] = c
	}
	return d
})

const (
	hangulSBase, hangulLBase, hangulVBase, hangulTBase = 0xAC00, 0x1100, 0x1161, 0x11A7
	hangulLCount, hangulVCount, hangulTCount           = 19, 21, 28
	hangulNCount, hangulSCount                         = hangulVCount * hangulTCount, hangulLCount * hangulNCount
)

// NFKC of the Unicode 3.2 database, decomposition, canonical ordering and canonical composition
func (d *nameprepData) nfkc(s []rune) []rune {
	var dec []rune
	for _, cp := range s {
		switch {
		case cp >= hangulSBase && cp < hangulSBase+hangulSCount:
			i := cp - hangulSBase
			dec = append(dec, hangulLBase+i/hangulNCount, hangulVBase+(i%hangulNCount)/hangulTCount)
			if i%hangulTCount != 0 {
				dec = append(dec, hangulTBase+i%hangulTCount)
			}
		case d.nfkd[cp] != nil:
			dec = append(dec, d.nfkd[cp]...)
		default:
			dec = append(dec, cp)
		}
	}
	for i := 0; i < len(dec); {
		if d.ccc[dec[i]] == 0 {
			i++
			continue
		}
		j := i
		for j < len(dec) && d.ccc[dec[j]] != 0 {
			j++
		}
		run := dec[i:j]
		sort.SliceStable(run, func(a, b int) bool { return d.ccc[run[a]] < d.ccc[run[b]] })
		i = j
	}
	var out []rune
	starter := -1
	for _, cp := range dec {
		cc := d.ccc[cp]
		if starter >= 0 {
			prev := d.ccc[out[len(out)-1]]
			if len(out)-1 == starter || (prev != 0 && prev < cc) {
				s0 := out[starter]
				composed, ok := rune(0), false
				switch {
				case s0 >= hangulLBase && s0 < hangulLBase+hangulLCount && cp >= hangulVBase && cp < hangulVBase+hangulVCount:
					composed, ok = hangulSBase+((s0-hangulLBase)*hangulVCount+(cp-hangulVBase))*hangulTCount, true
				case s0 >= hangulSBase && s0 < hangulSBase+hangulSCount && (s0-hangulSBase)%hangulTCount == 0 &&
					cp > hangulTBase && cp < hangulTBase+hangulTCount:
					composed, ok = s0+(cp-hangulTBase), true
				default:
					composed, ok = d.compose[[2]rune{s0, cp}]
				}
				if ok {
					out[starter] = composed
					continue
				}
			}
		}
		if cc == 0 {
			starter = len(out)
		}
		out = append(out, cp)
	}
	return out
}

// Failure of an idna step positioned in the string that step worked on
type idnaFail struct {
	start, end int
	reason     string
}

// Repr of one code point as Python prints it, a lone surrogate escaped
func charRepr(c rune) string {
	var b strings.Builder
	writeCodePoint(&b, c)
	return pyrepr.StrRepr(b.String())
}

// Nameprep of RFC 3491 as encodings.idna applies it with unassigned code points allowed
func nameprep(label []rune) ([]rune, *idnaFail) {
	d := nameprepTables()
	var mapped []rune
	for _, c := range label {
		if d.b1.has(c) {
			continue
		}
		if m, ok := d.b2[c]; ok {
			mapped = append(mapped, m...)
			continue
		}
		mapped = append(mapped, c)
	}
	out := d.nfkc(mapped)
	for i, c := range out {
		if d.prohibited.has(c) {
			return out, &idnaFail{i, i + 1, "Invalid character " + charRepr(c)}
		}
	}
	randAL := make([]bool, len(out))
	any := false
	for i, c := range out {
		randAL[i] = d.randAL.has(c)
		any = any || randAL[i]
	}
	if any {
		for i, c := range out {
			if d.lCat.has(c) {
				return out, &idnaFail{i, i + 1, "Violation of BIDI requirement 2"}
			}
		}
		if !randAL[0] {
			return out, &idnaFail{0, 1, "Violation of BIDI requirement 3"}
		}
		if !randAL[len(out)-1] {
			return out, &idnaFail{len(out) - 1, len(out), "Violation of BIDI requirement 3"}
		}
	}
	return out, nil
}
