package slack

import (
	"maps"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	MaxBlocks      = 50
	MaxSectionText = 3000
	MaxHeaderText  = 150
	maxTableRows   = 100
	maxTableCols   = 20
	maxTableChars  = 10000
)

// Compiled Python pattern, panicking at init on a malformed constant
func mustPyRe(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

var (
	hrRe           = mustPyRe(`^\s{0,3}([-*_])(?:\s*\1){2,}\s*$`)
	headerRe       = mustPyRe(`^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$`)
	fenceRe        = mustPyRe("^\\s*(`{3,}|~{3,})(.*)$")
	orderedRe      = mustPyRe(`^(\s*)(\d+)[.)]\s+(.*)$`)
	bulletRe       = mustPyRe(`^(\s*)[-*+]\s+(.*)$`)
	quoteRe        = mustPyRe(`^\s{0,3}>\s?(.*)$`)
	tableSepRe     = mustPyRe(`^\s*\|?\s*:?-{1,}:?\s*(\|\s*:?-{1,}:?\s*)+\|?\s*$`)
	inlineCodeRe   = mustPyRe("`([^`]+)`")
	linkRe         = mustPyRe(`(?<!!)\[([^\]]+)\]\(([^()\s]+(?:\([^()]*\)[^()\s]*)*)\)`)
	slackLinkRe    = mustPyRe(`<([a-zA-Z][a-zA-Z0-9+.\-]*:[^>|]+)(?:\|([^>]+))?>`)
	slackMentionRe = mustPyRe(`<(@[A-Z0-9]+|#[A-Z0-9]+|!subteam\^[A-Z0-9]+|!(?:here|channel|everyone))(?:\|[^>]*)?>`)
	boldRe         = mustPyRe(`(?:\*\*|__)(.+?)(?:\*\*|__)`)
	italicRe       = mustPyRe(`(?<![\*_])(?:\*|_)(?![\*_\s])(.+?)(?<![\*_\s])(?:\*|_)(?![\*_])`)
	strikeRe       = mustPyRe(`~~(.+?)~~`)
)

// One regex match with rune offsets and its group texts, empty for a group that did not take part
type reMatch struct {
	start, end int
	groups     []string
}

// Every non overlapping match of the pattern in the text, as re finditer yields them
func findAll(re *regexp2.Regexp, s string) []reMatch {
	var out []reMatch
	m, _ := re.FindStringMatch(s)
	for m != nil {
		out = append(out, toMatch(m))
		m, _ = re.FindNextMatch(m)
	}
	return out
}

// Rune offsets and group texts of a regexp2 match
func toMatch(m *regexp2.Match) reMatch {
	gs := m.Groups()
	rm := reMatch{start: m.RuneIndex, end: m.RuneIndex + m.RuneLength, groups: make([]string, len(gs))}
	for i, g := range gs {
		rm.groups[i] = g.String()
	}
	return rm
}

// First match of the pattern, ok false when there is none
func search(re *regexp2.Regexp, s string) (reMatch, bool) {
	m, _ := re.FindStringMatch(s)
	if m == nil {
		return reMatch{}, false
	}
	return toMatch(m), true
}

// Whether a markdown list item starts the line
func isListLine(line string) bool {
	_, b := search(bulletRe, line)
	_, o := search(orderedRe, line)
	return b || o
}

// Nesting level of leading whitespace, two spaces or one tab per level, capped at five
func indentLevel(spaces string) int {
	width := 0
	for _, ch := range spaces {
		if ch == '\t' {
			width += 4
		} else {
			width++
		}
	}
	return min(width/2, 5)
}

// Rich text element of a Slack mention token body, nil for anything else
func mentionElement(body string) map[string]any {
	switch {
	case strings.HasPrefix(body, "@"):
		return map[string]any{"type": "user", "user_id": body[1:]}
	case strings.HasPrefix(body, "#"):
		return map[string]any{"type": "channel", "channel_id": body[1:]}
	case strings.HasPrefix(body, "!subteam^"):
		return map[string]any{"type": "usergroup", "usergroup_id": body[len("!subteam^"):]}
	case strings.HasPrefix(body, "!"):
		return map[string]any{"type": "broadcast", "range": body[1:]}
	}
	return nil
}

// Rune slice of a string between rune offsets
func runeSlice(r []rune, a, b int) string { return string(r[a:b]) }

// Style copy with one more flag set
func withStyle(style map[string]any, key string) map[string]any {
	out := maps.Clone(style)
	out[key] = true
	return out
}

// Rich text section children of inline markdown, unmatched markup kept verbatim as text
func inlineElements(text string) []any {
	var elements []any
	emitText := func(s string, style map[string]any) {
		if s == "" {
			return
		}
		el := map[string]any{"type": "text", "text": s}
		if len(style) > 0 {
			el["style"] = style
		}
		elements = append(elements, el)
	}
	emitLink := func(url, text string, style map[string]any) {
		el := map[string]any{"type": "link", "url": url, "text": text}
		if len(style) > 0 {
			el["style"] = maps.Clone(style)
		}
		elements = append(elements, el)
	}
	styleOrNil := func(style map[string]any) map[string]any {
		if len(style) == 0 {
			return nil
		}
		return maps.Clone(style)
	}
	var walkEmphasis func(s string, style map[string]any)
	walkEmphasis = func(s string, style map[string]any) {
		if s == "" {
			return
		}
		r := []rune(s)
		for _, rx := range []struct {
			re  *regexp2.Regexp
			key string
		}{{boldRe, "bold"}, {strikeRe, "strike"}, {italicRe, "italic"}} {
			if m, ok := search(rx.re, s); ok {
				walkEmphasis(runeSlice(r, 0, m.start), style)
				walkEmphasis(m.groups[1], withStyle(style, rx.key))
				walkEmphasis(runeSlice(r, m.end, len(r)), style)
				return
			}
		}
		pos := 0
		for _, m := range findAll(slackMentionRe, s) {
			emitText(runeSlice(r, pos, m.start), styleOrNil(style))
			if el := mentionElement(m.groups[1]); el != nil {
				elements = append(elements, el)
			} else {
				emitText(m.groups[0], styleOrNil(style))
			}
			pos = m.end
		}
		emitText(runeSlice(r, pos, len(r)), styleOrNil(style))
	}
	walkSlackLinks := func(s string, style map[string]any) {
		s = replaceAll(slackMentionRe, s, func(m reMatch) string { return "<" + m.groups[1] + ">" })
		r := []rune(s)
		pos := 0
		for _, m := range findAll(slackLinkRe, s) {
			walkEmphasis(runeSlice(r, pos, m.start), style)
			url := m.groups[1]
			label := m.groups[2]
			if label == "" {
				label = url
			}
			emitLink(url, label, style)
			pos = m.end
		}
		walkEmphasis(runeSlice(r, pos, len(r)), style)
	}
	walkLinks := func(s string, style map[string]any) {
		r := []rune(s)
		pos := 0
		for _, m := range findAll(linkRe, s) {
			walkSlackLinks(runeSlice(r, pos, m.start), style)
			emitLink(m.groups[2], m.groups[1], style)
			pos = m.end
		}
		walkSlackLinks(runeSlice(r, pos, len(r)), style)
	}
	walk := func(s string, style map[string]any) {
		r := []rune(s)
		pos := 0
		for _, m := range findAll(inlineCodeRe, s) {
			walkLinks(runeSlice(r, pos, m.start), style)
			emitText(m.groups[1], withStyle(style, "code"))
			pos = m.end
		}
		walkLinks(runeSlice(r, pos, len(r)), style)
	}
	walk(text, map[string]any{})
	if len(elements) == 0 {
		return []any{map[string]any{"type": "text", "text": text}}
	}
	return elements
}

// Text with every match replaced by the function result, as re sub with a callable
func replaceAll(re *regexp2.Regexp, s string, fn func(reMatch) string) string {
	r := []rune(s)
	var b strings.Builder
	pos := 0
	for _, m := range findAll(re, s) {
		b.WriteString(runeSlice(r, pos, m.start))
		b.WriteString(fn(m))
		pos = m.end
	}
	b.WriteString(runeSlice(r, pos, len(r)))
	return b.String()
}

// Child list without zero length text elements, a single space when nothing remains
func nonemptyElements(elements []any) []any {
	var els []any
	for _, e := range elements {
		if m, ok := e.(map[string]any); ok && m["type"] == "text" && !pyrepr.Truthy(m["text"]) {
			continue
		}
		els = append(els, e)
	}
	if len(els) == 0 {
		return []any{map[string]any{"type": "text", "text": " "}}
	}
	return els
}

// Plain text header capped at 150 characters, nil when emphasis removal leaves nothing
func headerBlock(text string) map[string]any {
	clean := pystr.Strip(strings.Map(func(r rune) rune {
		if strings.ContainsRune("*_~`", r) {
			return -1
		}
		return r
	}, text))
	if clean == "" {
		return nil
	}
	if r := []rune(clean); len(r) > MaxHeaderText {
		clean = string(r[:MaxHeaderText-1]) + "…"
	}
	return map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": clean, "emoji": true}}
}

// One rich text block wrapping a single element of the given kind
func richTextBlock(kind string, children []any) map[string]any {
	return map[string]any{"type": "rich_text", "elements": []any{map[string]any{"type": kind, "elements": nonemptyElements(children)}}}
}

// Monospace block of code fence or table text
func preformattedBlock(text string) map[string]any {
	return richTextBlock("rich_text_preformatted", []any{map[string]any{"type": "text", "text": strings.TrimRight(text, "\n")}})
}

// Quote block of the quoted lines joined by newline text elements
func quoteBlock(lines []string) map[string]any {
	var children []any
	for i, ln := range lines {
		if i > 0 {
			children = append(children, map[string]any{"type": "text", "text": "\n"})
		}
		children = append(children, inlineElements(ln)...)
	}
	return richTextBlock("rich_text_quote", children)
}

type listItem struct {
	indent  int
	ordered bool
	text    string
}

// One rich text block whose runs of equal indent and kind become separate list elements
func listBlock(items []listItem) map[string]any {
	var elements []any
	var cur map[string]any
	curIndent, curOrdered, started := 0, false, false
	for _, it := range items {
		if !started || it.indent != curIndent || it.ordered != curOrdered {
			style := "bullet"
			if it.ordered {
				style = "ordered"
			}
			cur = map[string]any{"type": "rich_text_list", "style": style, "indent": it.indent, "elements": []any{}}
			elements = append(elements, cur)
			curIndent, curOrdered, started = it.indent, it.ordered, true
		}
		cur["elements"] = append(cur["elements"].([]any), map[string]any{"type": "rich_text_section", "elements": nonemptyElements(inlineElements(it.text))})
	}
	return map[string]any{"type": "rich_text", "elements": elements}
}

// Mrkdwn section block
func sectionBlock(text string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}
}

// Column aligns of a markdown separator row
func parseAlignment(sepLine string) []string {
	var aligns []string
	for _, cell := range strings.Split(strings.Trim(pystr.Strip(sepLine), "|"), "|") {
		c := pystr.Strip(cell)
		left, right := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case left && right:
			aligns = append(aligns, "center")
		case right:
			aligns = append(aligns, "right")
		default:
			aligns = append(aligns, "left")
		}
	}
	return aligns
}

// Trimmed cells of a table row, escaped pipes kept inside their cell
func splitRow(row string) []string {
	protected := strings.ReplaceAll(strings.Trim(pystr.Strip(row), "|"), `\|`, "\x00PIPE\x00")
	var out []string
	for _, c := range strings.Split(protected, "|") {
		out = append(out, strings.ReplaceAll(pystr.Strip(c), "\x00PIPE\x00", "|"))
	}
	return out
}

// Padded parsed rows and their column count
func padRows(parsed [][]string) int {
	ncols := 0
	for _, r := range parsed {
		ncols = max(ncols, len(r))
	}
	for i, r := range parsed {
		for len(r) < ncols {
			r = append(r, "")
		}
		parsed[i] = r
	}
	return ncols
}

// Native table block of pipe table rows, nil past the row, column or character limits
func tableBlock(rows []string, sepLine string) map[string]any {
	var parsed [][]string
	for _, r := range rows {
		if pystr.Strip(r) != "" {
			parsed = append(parsed, splitRow(r))
		}
	}
	if len(parsed) == 0 {
		return nil
	}
	ncols := 0
	for _, r := range parsed {
		ncols = max(ncols, len(r))
	}
	if len(parsed) > maxTableRows || ncols > maxTableCols {
		return nil
	}
	padRows(parsed)
	total := 0
	for _, r := range parsed {
		for _, c := range r {
			total += utf8.RuneCountInString(c)
		}
	}
	if total > maxTableChars {
		return nil
	}
	aligns := parseAlignment(sepLine)
	lastNonDefault := -1
	padded := make([]string, ncols)
	for c := range padded {
		padded[c] = "left"
		if c < len(aligns) {
			padded[c] = aligns[c]
		}
		if padded[c] != "left" {
			lastNonDefault = c
		}
	}
	var tableRows []any
	for _, row := range parsed {
		cells := make([]any, len(row))
		for i, cell := range row {
			cells[i] = richTextBlock("rich_text_section", inlineElements(cell))
		}
		tableRows = append(tableRows, cells)
	}
	block := map[string]any{"type": "table", "rows": tableRows}
	if lastNonDefault >= 0 {
		settings := make([]any, lastNonDefault+1)
		for i := range settings {
			settings[i] = map[string]any{"align": padded[i]}
		}
		block["column_settings"] = settings
	}
	return block
}

// Aligned monospace rendering of pipe table rows with a header underline
func renderTable(rows []string) string {
	parsed := make([][]string, len(rows))
	for i, r := range rows {
		parsed[i] = splitRow(r)
	}
	if len(parsed) == 0 {
		return strings.Join(rows, "\n")
	}
	ncols := padRows(parsed)
	widths := make([]int, ncols)
	for c := range widths {
		for _, r := range parsed {
			widths[c] = max(widths[c], utf8.RuneCountInString(r[c]))
		}
	}
	var out []string
	for ri, r := range parsed {
		cells := make([]string, ncols)
		for c := range cells {
			cells[c] = pystr.Ljust(r[c], widths[c])
		}
		out = append(out, pystr.Rstrip(strings.Join(cells, " | ")))
		if ri == 0 {
			dashes := make([]string, ncols)
			for c := range dashes {
				dashes[c] = strings.Repeat("-", widths[c])
			}
			out = append(out, strings.Join(dashes, "-+-"))
		}
	}
	return strings.Join(out, "\n")
}

// Block Kit blocks of agent markdown, nil when empty, over the block limit or when rendering fails
func RenderBlocks(markdown string, mrkdwnFn func(string) string) (blocks []any) {
	if pystr.Strip(markdown) == "" {
		return nil
	}
	if mrkdwnFn == nil {
		mrkdwnFn = func(s string) string { return s }
	}
	defer func() {
		if recover() != nil {
			blocks = nil
		}
	}()
	lines := strings.Split(strings.ReplaceAll(markdown, "\r\n", "\n"), "\n")
	n := len(lines)
	var para []string
	flushPara := func() {
		if len(para) == 0 {
			return
		}
		text := pystr.Strip(strings.Join(para, "\n"))
		para = nil
		if text == "" {
			return
		}
		for _, chunk := range splitText(mrkdwnFn(text), MaxSectionText) {
			blocks = append(blocks, sectionBlock(chunk))
		}
	}
	for i := 0; i < n; {
		line := lines[i]
		if pystr.Strip(line) == "" {
			flushPara()
			i++
			continue
		}
		if fence, ok := search(fenceRe, line); ok {
			flushPara()
			marker := fence.groups[1]
			var body []string
			i++
			for i < n && !strings.HasPrefix(pystr.Lstrip(lines[i]), marker) {
				body = append(body, lines[i])
				i++
			}
			i++
			blocks = append(blocks, preformattedBlock(strings.Join(body, "\n")))
			continue
		}
		if _, ok := search(hrRe, line); ok {
			flushPara()
			blocks = append(blocks, map[string]any{"type": "divider"})
			i++
			continue
		}
		if hm, ok := search(headerRe, line); ok {
			flushPara()
			if header := headerBlock(hm.groups[2]); header != nil {
				blocks = append(blocks, header)
			}
			i++
			continue
		}
		if strings.Contains(line, "|") && i+1 < n {
			if _, ok := search(tableSepRe, lines[i+1]); ok {
				flushPara()
				sepLine := lines[i+1]
				trows := []string{line}
				i += 2
				for i < n && strings.Contains(lines[i], "|") && pystr.Strip(lines[i]) != "" {
					trows = append(trows, lines[i])
					i++
				}
				if table := tableBlock(trows, sepLine); table != nil {
					blocks = append(blocks, table)
				} else {
					blocks = append(blocks, preformattedBlock(renderTable(trows)))
				}
				continue
			}
		}
		if _, ok := search(quoteRe, line); ok {
			flushPara()
			var qlines []string
			for i < n {
				qm, ok := search(quoteRe, lines[i])
				if !ok {
					break
				}
				qlines = append(qlines, qm.groups[1])
				i++
			}
			blocks = append(blocks, quoteBlock(qlines))
			continue
		}
		if isListLine(line) {
			flushPara()
			var items []listItem
		collect:
			for i < n {
				bm, bok := search(bulletRe, lines[i])
				om, ook := search(orderedRe, lines[i])
				switch {
				case bok:
					items = append(items, listItem{indentLevel(bm.groups[1]), false, bm.groups[2]})
					i++
				case ook:
					items = append(items, listItem{indentLevel(om.groups[1]), true, om.groups[3]})
					i++
				case pystr.Strip(lines[i]) != "" && (strings.HasPrefix(lines[i], " ") || strings.HasPrefix(lines[i], "\t")) && len(items) > 0:
					items[len(items)-1].text += " " + pystr.Strip(lines[i])
					i++
				case pystr.Strip(lines[i]) == "" && len(items) > 0:
					j := i + 1
					for j < n && pystr.Strip(lines[j]) == "" {
						j++
					}
					if j < n && isListLine(lines[j]) {
						i = j
					} else {
						break collect
					}
				default:
					break collect
				}
			}
			blocks = append(blocks, listBlock(items))
			continue
		}
		para = append(para, line)
		i++
	}
	flushPara()
	if len(blocks) == 0 || len(blocks) > MaxBlocks {
		return nil
	}
	return blocks
}

// Chunks of at most limit characters split on lines then hard, each chunk fence balanced
func splitText(text string, limit int) []string {
	r := []rune(text)
	if len(r) <= limit {
		return []string{text}
	}
	hasFence := strings.Contains(text, "```")
	splitLimit := limit
	if hasFence {
		splitLimit = max(limit-8, limit/2, 1)
	}
	var out []string
	remaining := r
	for len(remaining) > splitLimit {
		cut := -1
		for k := min(splitLimit, len(remaining)) - 1; k >= 0; k-- {
			if remaining[k] == '\n' {
				cut = k
				break
			}
		}
		if cut <= 0 {
			cut = splitLimit
		}
		out = append(out, string(remaining[:cut]))
		remaining = []rune(strings.TrimLeft(string(remaining[cut:]), "\n"))
	}
	if len(remaining) > 0 {
		out = append(out, string(remaining))
	}
	if len(out) > 1 && hasFence {
		reopen := false
		for i, chunk := range out {
			if reopen {
				chunk = "```\n" + chunk
			}
			odd := strings.Count(chunk, "```")%2 == 1
			if odd {
				chunk += "\n```"
			}
			reopen = odd
			out[i] = chunk
		}
	}
	return out
}

// Text object with its text clamped to limit characters, changed false when it already fits, ok false where Python raises
func clampTextObj(obj map[string]any, limit int) (out map[string]any, changed, ok bool) {
	v := obj["text"]
	if !pyrepr.Truthy(v) {
		return obj, false, true
	}
	switch t := v.(type) {
	case string:
		r := []rune(t)
		if len(r) <= limit {
			return obj, false, true
		}
		clamped := maps.Clone(obj)
		clamped["text"] = pystr.Rstrip(string(r[:limit-1])) + "…"
		return clamped, true, true
	case []any:
		return obj, false, len(t) <= limit
	case map[string]any:
		return obj, false, len(t) <= limit
	}
	return obj, false, false
}

// Stripped text of a text object, ok false when the text is a non string truthy value that str strip rejects
func strippedText(obj map[string]any) (string, bool) {
	v := obj["text"]
	if !pyrepr.Truthy(v) {
		return "", true
	}
	s, ok := v.(string)
	return pystr.Strip(s), ok
}

// Blocks clamped to Slack limits with empty blocks dropped and null column settings repaired, nil when nothing valid remains
func SanitizeBlocks(blocks []any) []any {
	if len(blocks) == 0 {
		return nil
	}
	var out []any
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || !pyrepr.Truthy(block["type"]) {
			continue
		}
		switch block["type"] {
		case "section":
			hasBody := pyrepr.Truthy(block["fields"]) || pyrepr.Truthy(block["accessory"])
			if textObj, isDict := block["text"].(map[string]any); isDict {
				text, ok := strippedText(textObj)
				if !ok {
					return nil
				}
				if text == "" && !hasBody {
					continue
				}
				clamped, changed, ok := clampTextObj(textObj, MaxSectionText)
				if !ok {
					return nil
				}
				if changed {
					block = maps.Clone(block)
					block["text"] = clamped
				}
			} else if !hasBody {
				continue
			}
		case "header":
			textObj, isDict := block["text"].(map[string]any)
			if !isDict {
				continue
			}
			text, ok := strippedText(textObj)
			if !ok {
				return nil
			}
			if text == "" {
				continue
			}
			clamped, changed, ok := clampTextObj(textObj, MaxHeaderText)
			if !ok {
				return nil
			}
			if changed {
				block = maps.Clone(block)
				block["text"] = clamped
			}
		case "context":
			if !pyrepr.Truthy(block["elements"]) {
				continue
			}
			if elements, isList := block["elements"].([]any); isList {
				clampedEls := make([]any, len(elements))
				anyChanged := false
				for i, el := range elements {
					clampedEls[i] = el
					if m, isDict := el.(map[string]any); isDict && (m["type"] == "mrkdwn" || m["type"] == "plain_text") {
						c, changed, ok := clampTextObj(m, MaxSectionText)
						if !ok {
							return nil
						}
						clampedEls[i], anyChanged = c, anyChanged || changed
					}
				}
				if anyChanged {
					block = maps.Clone(block)
					block["elements"] = clampedEls
				}
			}
		case "rich_text", "actions", "context_actions":
			if !pyrepr.Truthy(block["elements"]) {
				continue
			}
		case "table":
			if !pyrepr.Truthy(block["rows"]) {
				continue
			}
			if settings, isList := block["column_settings"].([]any); isList {
				bad := false
				for _, cs := range settings {
					if _, isDict := cs.(map[string]any); !isDict {
						bad = true
					}
				}
				if bad {
					var fixed []any
					for _, cs := range settings {
						if m, isDict := cs.(map[string]any); isDict {
							fixed = append(fixed, m)
						} else {
							fixed = append(fixed, map[string]any{})
						}
					}
					for len(fixed) > 0 && len(fixed[len(fixed)-1].(map[string]any)) == 0 {
						fixed = fixed[:len(fixed)-1]
					}
					block = maps.Clone(block)
					if len(fixed) > 0 {
						block["column_settings"] = fixed
					} else {
						delete(block, "column_settings")
					}
				}
			}
		}
		out = append(out, block)
	}
	if len(out) == 0 {
		return nil
	}
	if len(out) > MaxBlocks {
		out = out[:MaxBlocks]
	}
	return out
}
