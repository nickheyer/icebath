// Text effects used to animate labels
package fx

import "math"

// Reveals text left to right by progress while the rest churns through the glyphs, changing with tick
func Decode(text string, progress float64, tick int, glyphs []rune) string {
	runes := []rune(text)
	reveal := Revealed(text, progress)
	if reveal >= len(runes) {
		return text
	}
	return string(runes[:reveal]) + Churn(string(runes[reveal:]), tick+reveal, glyphs)
}

// Replaces every rune but blanks with a glyph picked by position and tick, so text boils until it is revealed
func Churn(text string, tick int, glyphs []rune) string {
	if len(glyphs) == 0 {
		return text
	}
	runes := []rune(text)
	out := make([]rune, len(runes))
	for i, r := range runes {
		if r == ' ' || r == '\n' || r == '\t' {
			out[i] = r
			continue
		}
		out[i] = glyphs[scramble(i, tick)%uint32(len(glyphs))]
	}
	return string(out)
}

// Number of leading runes that progress has resolved
func Revealed(text string, progress float64) int {
	n := len([]rune(text))
	p := min(max(progress, 0), 1)
	return min(n, int(math.Floor(p*float64(n)+1e-9)))
}

func scramble(i, tick int) uint32 {
	h := uint32(i)*0x9e3779b1 ^ uint32(tick)*0x85ebca77
	h ^= h >> 15
	h *= 0x2c1b3c6d
	h ^= h >> 12
	return h
}
