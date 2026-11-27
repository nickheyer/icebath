package transport

import (
	"math"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	// Shortest text judged for repetition
	minRepeatText = 400
	// Window of runes whose repeats are counted, the repeats that make a run, and the share of the text it must cover
	repeatWindow = 60
	minRepeats   = 5
	dominance    = 0.5
	// Anchors sampled for a periodic run and matches tried for each
	anchorSamples = 32
	anchorMatches = 8
	// Share of distinct lines at most a runaway text has
	distinctLines = 0.5
	// Characters a channel streams before it is first judged, and the tail it is judged over
	runawayAt   = 16000
	runawayTail = 4 * runawayAt
)

// Line breaks text is split on
var lineBreaks = []rune{'\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029}

func splitLines(r []rune) [][]rune {
	var lines [][]rune
	start := 0
	for i := 0; i < len(r); i++ {
		if !slices.Contains(lineBreaks, r[i]) {
			continue
		}
		lines = append(lines, r[start:i])
		if r[i] == '\r' && i+1 < len(r) && r[i+1] == '\n' {
			i++
		}
		start = i + 1
	}
	if start < len(r) {
		lines = append(lines, r[start:])
	}
	return lines
}

// Whether exact repeats cover at least half the text: one line repeated, one window recurring, or one periodic run
func Repetitive(text string) bool {
	r := []rune(text)
	n := len(r)
	if n < minRepeatText {
		return false
	}
	return linesDominate(r, n) || windowsDominate(r, n) || periodDominates(r, n)
}

func windowsDominate(r []rune, n int) bool {
	needed := max(int(math.Ceil(float64(n)*dominance/repeatWindow)), minRepeats)
	counts := map[string]int{}
	for i := 0; i+repeatWindow <= n; i++ {
		key := string(r[i : i+repeatWindow])
		counts[key]++
		if counts[key] >= needed {
			return true
		}
	}
	return false
}

func periodDominates(r []rune, n int) bool {
	last := n - repeatWindow
	if last < 1 {
		return false
	}
	step := max((last+anchorSamples-2)/(anchorSamples-1), 1)
	var starts []int
	for s := 0; s <= last; s += step {
		starts = append(starts, s)
	}
	if starts[len(starts)-1] != last {
		starts = append(starts, last)
	}
	type run struct{ left, right, period int }
	var rejected []run
	for _, start := range starts {
		anchor := r[start : start+repeatWindow]
		from := start + 1
		for range anchorMatches {
			match := index(r, anchor, from)
			if match < 0 {
				break
			}
			period := match - start
			from = match + 1
			if slices.ContainsFunc(rejected, func(rj run) bool { return rj.left <= start && start < rj.right && period%rj.period == 0 }) {
				continue
			}
			left, right := expand(r, n, start, period)
			if right-left >= minRepeats*period && float64(right-left) >= float64(n)*dominance {
				return true
			}
			rejected = append(rejected, run{left, right, period})
		}
	}
	return false
}

// Bounds of the exact periodic run one matched window lies in
func expand(r []rune, n, start, period int) (int, int) {
	left := start
	for left > 0 && r[left-1] == r[left-1+period] {
		left--
	}
	right := start + repeatWindow
	for right+period < n && r[right] == r[right+period] {
		right++
	}
	return left, right + period
}

func linesDominate(r []rune, n int) bool {
	counts := map[string]int{}
	for _, line := range splitLines(r) {
		if norm := strings.TrimSpace(string(line)); norm != "" {
			counts[norm]++
		}
	}
	for line, c := range counts {
		if c >= minRepeats && float64(c*utf8.RuneCountInString(line)) >= float64(n)*dominance {
			return true
		}
	}
	return false
}

// Whether the text is a runaway loop: repetitive, and mostly the same lines when it has lines
func Runaway(text string) bool {
	if !Repetitive(text) {
		return false
	}
	var lines []string
	for _, line := range splitLines([]rune(text)) {
		if norm := strings.TrimSpace(string(line)); norm != "" {
			lines = append(lines, norm)
		}
	}
	if len(lines) < minRepeats {
		return true
	}
	distinct := map[string]bool{}
	for _, line := range lines {
		distinct[line] = true
	}
	return float64(len(distinct)) <= float64(len(lines))*distinctLines
}

// Watches one channel of a stream, such as its text or its reasoning, for a runaway loop
type RunawayWatch struct {
	parts []string
	chars int
	next  int
}

// Adds one delta, true once the channel has turned into a runaway loop
func (w *RunawayWatch) Feed(text string) bool {
	if text == "" {
		return false
	}
	if w.next == 0 {
		w.next = runawayAt
	}
	w.parts = append(w.parts, text)
	w.chars += utf8.RuneCountInString(text)
	if w.chars < w.next {
		return false
	}
	w.next = w.chars + min(w.chars, runawayTail)
	joined := []rune(strings.Join(w.parts, ""))
	if len(joined) > runawayTail {
		joined = joined[len(joined)-runawayTail:]
	}
	tail := string(joined)
	w.parts = []string{tail}
	return Runaway(tail)
}
