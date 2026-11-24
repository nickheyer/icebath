package consoleutil

import (
	"iter"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/charmbracelet/colorprofile"
	"github.com/rivo/uniseg"
)

// How wrap lays out text: the side lines align to, what a too wide word does, whether lines wrap at all
type wrapOptions struct {
	// left pads each line to the width, center and right pad before the text, empty leaves lines as they are
	justify string
	// fold (the default) breaks words wider than the width across lines, ellipsis and crop cut them
	overflow string
	// lines kept whole and uncut, for a terminal that wraps them itself
	noWrap bool
	// style of the padding justification adds
	pad Style
}

// Start and end byte offsets of each word of text with the white space around it, leading space belonging to the first word
func words(text string) iter.Seq2[int, int] {
	return func(yield func(int, int) bool) {
		skip := func(i int, space bool) int {
			for i < len(text) {
				r, size := utf8.DecodeRuneInString(text[i:])
				if unicode.IsSpace(r) != space {
					break
				}
				i += size
			}
			return i
		}
		for i := 0; i < len(text); {
			start := i
			i = skip(i, true)
			if i == len(text) {
				return
			}
			i = skip(skip(i, false), true)
			if !yield(start, i) {
				return
			}
		}
	}
}

// Byte offsets where text breaks into lines of at most width cells, words wider than a line folded across lines when fold is set
func divideLine(text string, width int, fold bool) []int {
	var breaks []int
	offset := 0
	for start, end := range words(text) {
		word := text[start:end]
		wordLen := cells(strings.TrimRightFunc(word, unicode.IsSpace))
		switch {
		case width-offset >= wordLen:
			offset += cells(word)
		case wordLen > width && fold:
			chunks := chopCells(word, width)
			for i, chunk := range chunks {
				if start > 0 {
					breaks = append(breaks, start)
				}
				if i == len(chunks)-1 {
					offset = cells(chunk)
				} else {
					start += len(chunk)
				}
			}
		case wordLen > width:
			if start > 0 {
				breaks = append(breaks, start)
			}
			offset = cells(word)
		case offset > 0 && start > 0:
			breaks = append(breaks, start)
			offset = cells(word)
		}
	}
	return breaks
}

// Pieces of s of at most width cells each, whole grapheme clusters only
func chopCells(s string, width int) []string {
	var out []string
	start, total, state := 0, 0, -1
	for i := 0; i < len(s); {
		cluster, _, w, next := uniseg.FirstGraphemeClusterInString(s[i:], state)
		if total+w > width && i > start {
			out = append(out, s[start:i])
			start, total = i, 0
		}
		total, i, state = total+w, i+len(cluster), next
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// Lines of styled text wrapped at width cells: split at newlines, tabs expanded, broken at word boundaries, trailing space trimmed past the width, then justified and fitted to the width
func wrap(t segments, width int, o wrapOptions) []segments {
	overflow := o.overflow
	if overflow == "" {
		overflow = "fold"
	}
	var out []segments
	for _, line := range t.lines() {
		line = line.expandTabs(8)
		if o.noWrap || width <= 0 {
			if width > 0 {
				line = line.rstripEnd(width)
			}
			out = append(out, justifyLine(line, width, overflow, o))
			continue
		}
		plain := line.plain()
		prev := 0
		for _, b := range divideLine(plain, width, overflow == "fold") {
			out = append(out, justifyLine(line.slice(prev, b).rstripEnd(width), width, overflow, o))
			prev = b
		}
		out = append(out, justifyLine(line.slice(prev, len(plain)).rstripEnd(width), width, overflow, o))
	}
	return out
}

// One wrapped line aligned and fitted to width cells, left alone past the width when lines do not wrap
func justifyLine(l segments, width int, overflow string, o wrapOptions) segments {
	if width <= 0 {
		return l
	}
	fit := func(l segments, pad bool) segments {
		if o.noWrap {
			return l
		}
		return l.truncate(width, overflow, pad, o.pad)
	}
	switch o.justify {
	case "left":
		return fit(l, true)
	case "center":
		l = fit(l.rstrip(), false)
		return segments(nil).with(spaces(max(0, width-l.width())/2), o.pad).join(l).padTo(width, o.pad)
	case "right":
		l = fit(l.rstrip(), false)
		return segments(nil).with(spaces(width-l.width()), o.pad).join(l)
	}
	return fit(l, false)
}

// Narrowest and widest a text lays out in cells: its longest word and its longest line
type measurement struct{ min, max int }

func measureText(t segments) measurement {
	plain := t.expandTabs(8).plain()
	var m measurement
	for line := range strings.SplitSeq(plain, "\n") {
		m.max = max(m.max, cells(line))
	}
	found := false
	for _, w := range strings.FieldsFunc(plain, unicode.IsSpace) {
		m.min = max(m.min, cells(w))
		found = true
	}
	if !found {
		m.min = m.max
	}
	return m
}

// Values with total cells taken from them in proportion to ratios, no value losing more than its maximum
func ratioReduce(total int, ratios, maximums, values []int) []int {
	ratios = append([]int(nil), ratios...)
	totalRatio := 0
	for i := range ratios {
		if maximums[i] == 0 {
			ratios[i] = 0
		}
		totalRatio += ratios[i]
	}
	out := append([]int(nil), values...)
	if totalRatio == 0 {
		return out
	}
	remaining := total
	for i, v := range values {
		if ratios[i] == 0 || totalRatio <= 0 {
			continue
		}
		taken := min(maximums[i], int(math.RoundToEven(float64(ratios[i]*remaining)/float64(totalRatio))))
		out[i] = v - taken
		remaining -= taken
		totalRatio -= ratios[i]
	}
	return out
}

// Column widths narrowed to fit maxWidth, the widest wrappable columns giving up cells first
func collapseWidths(widths []int, wrapable []bool, maxWidth int) []int {
	if !slices.Contains(wrapable, true) {
		return widths
	}
	total := sum(widths)
	excess := total - maxWidth
	for total > 0 && excess > 0 {
		widest := 0
		for i, w := range widths {
			if wrapable[i] {
				widest = max(widest, w)
			}
		}
		second := 0
		for i, w := range widths {
			if wrapable[i] && w != widest {
				second = max(second, w)
			}
		}
		ratios := make([]int, len(widths))
		found := false
		for i, w := range widths {
			if wrapable[i] && w == widest {
				ratios[i] = 1
				found = true
			}
		}
		diff := widest - second
		if !found || diff == 0 {
			break
		}
		limits := make([]int, len(widths))
		for i := range limits {
			limits[i] = min(excess, diff)
		}
		widths = ratioReduce(excess, ratios, limits, widths)
		total = sum(widths)
		excess = total - maxWidth
	}
	return widths
}

// Column widths fitted within maxWidth: wrappable columns narrowed first, then every column evenly
func fitWidths(widths []int, wrapable []bool, maxWidth int) []int {
	if sum(widths) <= maxWidth {
		return widths
	}
	widths = collapseWidths(widths, wrapable, maxWidth)
	if total := sum(widths); total > maxWidth {
		ones := make([]int, len(widths))
		for i := range ones {
			ones[i] = 1
		}
		widths = ratioReduce(total-maxWidth, ones, widths, widths)
	}
	return widths
}

func sum(values []int) int {
	n := 0
	for _, v := range values {
		n += v
	}
	return n
}

// Lines of ANSI styled text wrapped at width cells, long words folded, each line closing and reopening the styles it draws in
func WrapStyled(text string, width int) []string {
	lines := wrap(decodeANSI(text), width, wrapOptions{})
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = renderLine(l, colorprofile.TrueColor, true)
	}
	return out
}
