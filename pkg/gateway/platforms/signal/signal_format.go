package signal

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Phone screen budget in monospace cells, wider tables render as vertical key value rows
var tableWidth = 40

var (
	codeBlockRE  = pyre.MustCompile("(?s)```[a-zA-Z0-9_+-]*\\n?(.*?)```")
	headingRE    = pyre.MustCompile(`(?m)^#{1,6}\s+`)
	fenceSplitRE = pyre.MustCompile("(?s)(```.*?```)")
	bulletRE     = pyre.MustCompile(`(?m)^([ \t]{0,3})[-*+]\s+`)
	blankRunRE   = pyre.MustCompile(`\n{3,}`)
)

// Inline marker patterns in claim order, the first to claim a span wins
var inlinePatterns = []struct {
	re    *regexp2.Regexp
	style string
}{
	{pyre.MustCompile(`(?s)\*\*(.+?)\*\*`), "BOLD"},
	{pyre.MustCompile(`(?s)__(.+?)__`), "BOLD"},
	{pyre.MustCompile(`(?s)~~(.+?)~~`), "STRIKETHROUGH"},
	{pyre.MustCompile("`(.+?)`"), "MONOSPACE"},
	{pyre.MustCompile(`(?<!\*)\*(?!\*| )(.+?)(?<!\*)\*(?!\*)`), "ITALIC"},
	{pyre.MustCompile(`(?<!\w)_(?!_)(.+?)(?<!_)_(?!\w)`), "ITALIC"},
}

// One styled range in code points
type styleRange struct {
	start, length int
	style         string
}

// Length of the runes in UTF-16 code units
func utf16LenRunes(rs []rune) int {
	n := 0
	for _, r := range rs {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// Python slice of a rune sequence, empty when the bounds cross
func runeSlice(rs []rune, start, end int) []rune {
	start, end = max(0, min(start, len(rs))), max(0, min(end, len(rs)))
	if start >= end {
		return nil
	}
	return rs[start:end]
}

// Alternating outside and fenced parts of the source the way re.split keeps its capture group
func splitFences(source string) []string {
	runes := pystr.Runes(source)
	var parts []string
	last := 0
	m, _ := fenceSplitRE.FindRunesMatch(runes)
	for m != nil {
		parts = append(parts, pystr.FromRunes(runes[last:m.RuneIndex]), pystr.FromRunes(runes[m.RuneIndex:m.RuneIndex+m.RuneLength]))
		last = m.RuneIndex + m.RuneLength
		m, _ = fenceSplitRE.FindNextMatch(m)
	}
	return append(parts, pystr.FromRunes(runes[last:]))
}

// Markdown bullet markers outside code fences replaced with plain Unicode bullets
func normalizeBulletMarkers(source string) string {
	parts := splitFences(source)
	for idx, part := range parts {
		if idx%2 == 0 {
			out, err := bulletRE.Replace(part, "$1• ", -1, -1)
			if err != nil {
				panic(err)
			}
			parts[idx] = out
		}
	}
	return strings.Join(parts, "")
}

// Every pipe table outside a code fence realigned and wrapped in a fence
func fenceTables(source string) string {
	parts := splitFences(source)
	for idx, part := range parts {
		if idx%2 == 0 {
			parts[idx] = fenceUnfencedTables(part)
		}
	}
	return strings.Join(parts, "")
}

// Header row, divider and contiguous pipe rows of each table fenced as one aligned block
func fenceUnfencedTables(text string) string {
	if !strings.Contains(text, "|") {
		return text
	}
	lines := strings.Split(text, "\n")
	var out []string
	n := len(lines)
	for i := 0; i < n; {
		if strings.Contains(lines[i], "|") && i+1 < n && tools.IsTableDivider(lines[i+1]) {
			j := i + 2
			for j < n && strings.Contains(lines[j], "|") && pystr.Strip(lines[j]) != "" {
				j++
			}
			out = append(out, "```", tools.RealignMarkdownTables(strings.Join(lines[i:j], "\n"), &tableWidth), "```")
			i = j
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

// Plain text and start length style ranges in UTF-16 code units for the signal-cli textStyle params
func markdownToSignal(text string) (string, []string) {
	collapsed, err := blankRunRE.Replace(text, "\n\n", -1, -1)
	if err != nil {
		panic(err)
	}
	runes := pystr.Runes(fenceTables(normalizeBulletMarkers(pystr.Strip(collapsed))))
	var styles []styleRange
	for {
		m, _ := codeBlockRE.FindRunesMatch(runes)
		if m == nil {
			break
		}
		g := m.GroupByNumber(1)
		inner := runes[g.RuneIndex : g.RuneIndex+g.RuneLength]
		for len(inner) > 0 && inner[len(inner)-1] == '\n' {
			inner = inner[:len(inner)-1]
		}
		styles = append(styles, styleRange{m.RuneIndex, len(inner), "MONOSPACE"})
		next := append([]rune{}, runes[:m.RuneIndex]...)
		next = append(next, inner...)
		runes = append(next, runes[m.RuneIndex+m.RuneLength:]...)
	}
	var newText []rune
	lastEnd := 0
	for m, _ := headingRE.FindRunesMatch(runes); m != nil; m, _ = headingRE.FindNextMatch(m) {
		matchEnd := m.RuneIndex + m.RuneLength
		newText = append(newText, runeSlice(runes, lastEnd, m.RuneIndex)...)
		eol := len(runes)
		for k := matchEnd; k < len(runes); k++ {
			if runes[k] == '\n' {
				eol = k
				break
			}
		}
		heading := runeSlice(runes, matchEnd, eol)
		styles = append(styles, styleRange{len(newText), len(heading), "BOLD"})
		newText = append(newText, heading...)
		lastEnd = eol
	}
	runes = append(newText, runeSlice(runes, lastEnd, len(runes))...)
	type inlineMatch struct {
		ms, me, g1s, g1e int
		style            string
	}
	var allMatches []inlineMatch
	var occupied [][2]int
	for _, p := range inlinePatterns {
		for m, _ := p.re.FindRunesMatch(runes); m != nil; m, _ = p.re.FindNextMatch(m) {
			ms, me := m.RuneIndex, m.RuneIndex+m.RuneLength
			overlaps := false
			for _, o := range occupied {
				if ms < o[1] && me > o[0] {
					overlaps = true
					break
				}
			}
			if !overlaps {
				g := m.GroupByNumber(1)
				allMatches = append(allMatches, inlineMatch{ms, me, g.RuneIndex, g.RuneIndex + g.RuneLength, p.style})
				occupied = append(occupied, [2]int{ms, me})
			}
		}
	}
	sort.SliceStable(allMatches, func(i, j int) bool {
		a, b := allMatches[i], allMatches[j]
		switch {
		case a.ms != b.ms:
			return a.ms < b.ms
		case a.me != b.me:
			return a.me < b.me
		case a.g1s != b.g1s:
			return a.g1s < b.g1s
		case a.g1e != b.g1e:
			return a.g1e < b.g1e
		}
		return a.style < b.style
	})
	var result []rune
	lastEnd = 0
	var removals [][2]int
	var inlineStyles []styleRange
	for _, im := range allMatches {
		if im.g1s > im.ms {
			removals = append(removals, [2]int{im.ms, im.g1s - im.ms})
		}
		if im.me > im.g1e {
			removals = append(removals, [2]int{im.g1e, im.me - im.g1e})
		}
		result = append(result, runeSlice(runes, lastEnd, im.ms)...)
		inner := runeSlice(runes, im.g1s, im.g1e)
		inlineStyles = append(inlineStyles, styleRange{len(result), len(inner), im.style})
		result = append(result, inner...)
		lastEnd = im.me
	}
	sort.SliceStable(removals, func(i, j int) bool {
		if removals[i][0] != removals[j][0] {
			return removals[i][0] < removals[j][0]
		}
		return removals[i][1] < removals[j][1]
	})
	adjust := func(pos int) int {
		shift := 0
		for _, r := range removals {
			if r[0] >= pos {
				break
			}
			shift += min(r[1], pos-r[0])
		}
		return pos - shift
	}
	var combined []styleRange
	for _, s := range styles {
		if start, end := adjust(s.start), adjust(s.start+s.length); end > start {
			combined = append(combined, styleRange{start, end - start, s.style})
		}
	}
	combined = append(combined, inlineStyles...)
	runes = append(result, runeSlice(runes, lastEnd, len(runes))...)
	sort.SliceStable(combined, func(i, j int) bool {
		a, b := combined[i], combined[j]
		switch {
		case a.start != b.start:
			return a.start < b.start
		case a.length != b.length:
			return a.length < b.length
		}
		return a.style < b.style
	})
	styleStrings := []string{}
	for _, s := range combined {
		if 0 <= s.start && s.start+s.length <= len(runes) {
			styleStrings = append(styleStrings, fmt.Sprintf("%d:%d:%s", utf16LenRunes(runes[:s.start]), utf16LenRunes(runes[s.start:s.start+s.length]), s.style))
		}
	}
	return pystr.FromRunes(runes), styleStrings
}
