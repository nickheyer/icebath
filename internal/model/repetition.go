package model

import (
	"math"
	"strings"
)

const (
	minFragmentLength        = 400
	repeatWindow             = 60
	minRepeatCount           = 5
	dominanceRatio           = 0.5
	maxAnchorSamples         = 32
	maxAnchorMatches         = 8
	runawayDistinctLineRatio = 0.5
	StopPathMinChars         = 16000
	streamTailChars          = 4 * StopPathMinChars
)

const RepetitionLoopInterrupted = "[the reply degenerated into a repetition loop and was interrupted]"

func IsRepetitionDominated(text string) bool {
	runes := []rune(text)
	n := len(runes)
	if n < minFragmentLength {
		return false
	}
	if lineRepetitionDominated(text, n) {
		return true
	}
	return windowCountDominated(runes) || periodicRunDominated(runes)
}

func windowCountDominated(text []rune) bool {
	n := len(text)
	needed := max(minRepeatCount, int(math.Ceil(float64(n)*dominanceRatio/repeatWindow)))
	counts := map[string]int{}
	for i := 0; i+repeatWindow <= n; i++ {
		key := string(text[i : i+repeatWindow])
		c := counts[key] + 1
		if c >= needed {
			return true
		}
		counts[key] = c
	}
	return false
}

type rejectedRun struct{ lo, hi, period int }

func periodicRunDominated(text []rune) bool {
	n := len(text)
	maxStart := n - repeatWindow
	if maxStart < 1 {
		return false
	}
	step := max(1, (maxStart+maxAnchorSamples-2)/(maxAnchorSamples-1))
	var starts []int
	for s := 0; s <= maxStart; s += step {
		starts = append(starts, s)
	}
	if starts[len(starts)-1] != maxStart {
		starts = append(starts, maxStart)
	}
	var rejected []rejectedRun
	for _, start := range starts {
		anchor := text[start : start+repeatWindow]
		from := start + 1
		for range maxAnchorMatches {
			match := indexRunes(text, anchor, from)
			if match < 0 {
				break
			}
			period := match - start
			from = match + 1
			skip := false
			for _, r := range rejected {
				if r.lo <= start && start < r.hi && period%r.period == 0 {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			left, right := expandRun(text, start, period, repeatWindow)
			if right-left >= minRepeatCount*period && float64(right-left) >= float64(n)*dominanceRatio {
				return true
			}
			rejected = append(rejected, rejectedRun{left, right, period})
		}
	}
	return false
}

func indexRunes(hay, needle []rune, from int) int {
	if len(needle) == 0 {
		return from
	}
	for i := from; i+len(needle) <= len(hay); i++ {
		if hay[i] != needle[0] {
			continue
		}
		equal := true
		for j := 1; j < len(needle); j++ {
			if hay[i+j] != needle[j] {
				equal = false
				break
			}
		}
		if equal {
			return i
		}
	}
	return -1
}

func expandRun(text []rune, start, period, matched int) (int, int) {
	n := len(text)
	left := start
	for left > 0 && text[left-1] == text[left-1+period] {
		left--
	}
	right := start + matched
	for right+period < n && text[right] == text[right+period] {
		right++
	}
	return left, right + period
}

func IsRunawayRepetition(text string) bool {
	if !IsRepetitionDominated(text) {
		return false
	}
	var lines []string
	for _, line := range splitLines(text) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < minRepeatCount {
		return true
	}
	distinct := map[string]bool{}
	for _, line := range lines {
		distinct[line] = true
	}
	return float64(len(distinct)) <= float64(len(lines))*runawayDistinctLineRatio
}

func lineRepetitionDominated(text string, n int) bool {
	counts := map[string]int{}
	var order []string
	for _, line := range splitLines(text) {
		if norm := strings.TrimSpace(line); norm != "" {
			if counts[norm] == 0 {
				order = append(order, norm)
			}
			counts[norm]++
		}
	}
	for _, line := range order {
		c := counts[line]
		if c >= minRepeatCount && float64(c*len([]rune(line))) >= float64(n)*dominanceRatio {
			return true
		}
	}
	return false
}

func splitLines(text string) []string {
	var lines []string
	runes := []rune(text)
	start := 0
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '\n', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, string(runes[start:i]))
			start = i + 1
		case '\r':
			lines = append(lines, string(runes[start:i]))
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
			start = i + 1
		}
	}
	if start < len(runes) {
		lines = append(lines, string(runes[start:]))
	}
	return lines
}

type RunawayWatch struct {
	parts []string
	chars int
	next  int
}

func NewRunawayWatch() *RunawayWatch { return &RunawayWatch{next: StopPathMinChars} }

func (w *RunawayWatch) Feed(text string) bool {
	if text == "" {
		return false
	}
	w.parts = append(w.parts, text)
	w.chars += len([]rune(text))
	if w.chars < w.next {
		return false
	}
	w.next = w.chars + min(w.chars, streamTailChars)
	joined := []rune(strings.Join(w.parts, ""))
	if len(joined) > streamTailChars {
		joined = joined[len(joined)-streamTailChars:]
	}
	tail := string(joined)
	w.parts = []string{tail}
	return IsRunawayRepetition(tail)
}
