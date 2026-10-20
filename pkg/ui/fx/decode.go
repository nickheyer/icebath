// Text effects used to animate labels
package fx

import "math"

// Characters that churn in the unresolved part of a decoding label
var Glyphs = []rune("░▒▓█▚▞▙▟╳01")

// Reveals text left to right by progress while the tail churns through glyphs that change with tick
func Decode(text string, progress float64, tick int) string {
	runes := []rune(text)
	reveal := Revealed(text, progress)
	if reveal >= len(runes) {
		return text
	}
	out := make([]rune, len(runes))
	copy(out, runes[:reveal])
	for i := reveal; i < len(runes); i++ {
		if runes[i] == ' ' {
			out[i] = ' '
			continue
		}
		out[i] = Glyphs[scramble(i, tick)%uint32(len(Glyphs))]
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
