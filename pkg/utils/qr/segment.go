package qrutil

import (
	"math"

	"rsc.io/qr/coding"
)

// Data modes a segment can use, from the most compact to the most general
const (
	modeNumeric = iota
	modeAlphanumeric
	modeByte
)

// Width of the character count field of each mode in each version class
var countBits = [3][3]int{{10, 12, 14}, {9, 11, 13}, {8, 16, 16}}

// Data bits of one character of each mode in sixths of a bit: 10/3, 11/2 and 8
var charCost = [3]int{20, 33, 48}

// Whether the byte fits the mode
func modeHolds(mode int, c byte) bool {
	switch mode {
	case modeNumeric:
		return '0' <= c && c <= '9'
	case modeAlphanumeric:
		switch {
		case '0' <= c && c <= '9', 'A' <= c && c <= 'Z':
			return true
		}
		switch c {
		case ' ', '$', '%', '*', '+', '-', '.', '/', ':':
			return true
		}
		return false
	}
	return true
}

// Whole bits a cost in sixths of a bit rounds up to, still in sixths
func roundUp6(cost int) int { return (cost + 5) / 6 * 6 }

// Segments of text in the modes that together take the fewest bits for a version class, found by dynamic programming over every byte
func segment(text string, class int) []coding.Encoding {
	n := len(text)
	if n == 0 {
		return nil
	}
	var head [3]int
	for m := range head {
		head[m] = (4 + countBits[m][class]) * 6
	}
	const unreachable = math.MaxInt / 4
	cost := [3]int{unreachable, unreachable, unreachable}
	from := make([][3]int8, n)
	for i := range n {
		c := text[i]
		var next [3]int
		for m := range next {
			next[m] = unreachable
			from[i][m] = -1
			if !modeHolds(m, c) {
				continue
			}
			if i == 0 {
				next[m] = head[m] + charCost[m]
				continue
			}
			for p, pc := range cost {
				if pc >= unreachable {
					continue
				}
				v := pc + charCost[m]
				if p != m {
					v = roundUp6(pc) + head[m] + charCost[m]
				}
				if v < next[m] {
					next[m], from[i][m] = v, int8(p)
				}
			}
		}
		cost = next
	}
	mode, best := modeByte, unreachable
	for m, c := range cost {
		if c < unreachable && roundUp6(c) < best {
			mode, best = m, roundUp6(c)
		}
	}
	modes := make([]int8, n)
	for i := n - 1; i >= 0; i-- {
		modes[i] = int8(mode)
		if i > 0 {
			mode = int(from[i][mode])
		}
	}
	var segs []coding.Encoding
	for start := 0; start < n; {
		end := start + 1
		for end < n && modes[end] == modes[start] {
			end++
		}
		switch chunk := text[start:end]; modes[start] {
		case modeNumeric:
			segs = append(segs, coding.Num(chunk))
		case modeAlphanumeric:
			segs = append(segs, coding.Alpha(chunk))
		default:
			segs = append(segs, coding.String(chunk))
		}
		start = end
	}
	return segs
}
