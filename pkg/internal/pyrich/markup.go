package pyrich

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyast"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var tagRE = regexp.MustCompile(`(\\*)\[([a-z#/@][^\[]*?)]`)

var escapeRE = regexp.MustCompile(`(\\*)(\[[a-z#/@][^\[]*?])`)

var emojiRE = mustPyre(`(:(\S*?)(?:(?:\-)(emoji|text))?:)`)

var handlerRE = mustPyre(`^([\w.]*?)(\(.*?\))?$`)

var controlStripper = strings.NewReplacer("\a", "", "\b", "", "\v", "", "\f", "", "\r", "")

func mustPyre(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// MarkupError is the rich.errors.MarkupError console markup raises for a closing tag or meta tag it cannot apply
type MarkupError struct{ Msg string }

func (e *MarkupError) Error() string { return e.Msg }

// Emoji codes replaced by their characters, unknown codes kept, as rich _emoji_replace without a default variant
func emojiReplace(text string) string {
	if !strings.Contains(text, ":") {
		return text
	}
	out, err := emojiRE.ReplaceFunc(text, func(m regexp2.Match) string {
		emoji, ok := emojiCodes[strings.ToLower(m.GroupByNumber(2).String())]
		if !ok {
			return m.String()
		}
		switch m.GroupByNumber(3).String() {
		case "text":
			return emoji + "\ufe0e"
		case "emoji":
			return emoji + "\ufe0f"
		}
		return emoji
	}, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Bell, backspace, vertical tab, form feed and carriage return removed, as rich.control.strip_control_codes
func stripControlCodes(text string) string { return controlStripper.Replace(text) }

// Meta tag parameters evaluated as rich does on close, failing with the MarkupError rich raises
func checkMetaParams(params string) error {
	parameters := strings.TrimSpace(params)
	if m, _ := handlerRE.FindStringMatch(parameters); m != nil {
		if g := m.GroupByNumber(2); len(g.Captures) == 0 {
			parameters = "()"
		} else {
			parameters = g.String()
		}
	}
	if _, err := pyast.LiteralEval(parameters); err != nil {
		var syn *pyast.SyntaxError
		if errors.As(err, &syn) {
			return &MarkupError{fmt.Sprintf("error parsing %s in %s; %s", pyrepr.StrRepr(parameters), pyrepr.StrRepr(params), syn.Msg)}
		}
		return &MarkupError{fmt.Sprintf("error parsing %s; %s", pyrepr.StrRepr(params), err.Error())}
	}
	return nil
}

// Escapes square bracket tags and a trailing backslash so text prints literally, as rich.markup.escape does
func Escape(markup string) string {
	markup = escapeRE.ReplaceAllStringFunc(markup, func(m string) string {
		sub := escapeRE.FindStringSubmatch(m)
		return sub[1] + sub[1] + `\` + sub[2]
	})
	if strings.HasSuffix(markup, `\`) && !strings.HasSuffix(markup, `\\`) {
		return markup + `\`
	}
	return markup
}

// Plain text of console markup with every style tag removed, panicking with a MarkupError where rich raises one
func Plain(markup string) string { return Render(markup, false) }

// Text of console markup, ANSI styled for the terminal color system when color is true, panicking with a MarkupError as rich raises
func Render(markup string, color bool) string {
	if !color {
		return plainMarkup(markup)
	}
	plain, spans := mustMarkupText(markup)
	return renderSpans(plain, spans, TermColorSystem())
}

// Markup rendered as Console.render_str does, the repr highlighter spans laid under the markup spans when highlight is true
func renderHighlighted(markup string, color, highlight bool) string {
	if !color {
		return plainMarkup(markup)
	}
	plain, spans := mustMarkupText(markup)
	if highlight {
		spans = append(reprSpans(plain), spans...)
	}
	return renderSpans(plain, spans, TermColorSystem())
}

func plainMarkup(markup string) string {
	plain, _ := mustMarkupText(markup)
	return plain
}

// Markup parsed as a rich console print parses it, its MarkupError raised as a panic carrying the error
func mustMarkupText(markup string) (string, []textSpan) {
	plain, spans, err := markupText(markup)
	if err != nil {
		panic(err)
	}
	return plain, spans
}

type markupTag struct {
	start      int
	name, text string
	params     string
	hasParams  bool
}

// Plain text of console markup with emoji codes replaced and control codes stripped, and its spans, as rich.markup.render
func markupText(markup string) (string, []textSpan, error) {
	if !strings.Contains(markup, "[") {
		return stripControlCodes(emojiReplace(markup)), nil, nil
	}
	var b strings.Builder
	n := 0
	appendText := func(s string) {
		s = stripControlCodes(emojiReplace(s))
		b.WriteString(s)
		n += utf8.RuneCountInString(s)
	}
	var stack []markupTag
	var spans []textSpan
	pos := 0
	for _, m := range tagRE.FindAllStringSubmatchIndex(markup, -1) {
		start, end := m[0], m[1]
		if start > pos {
			appendText(strings.ReplaceAll(markup[pos:start], `\[`, "["))
		}
		pos = end
		backslashes, escaped := (m[3]-m[2])/2, (m[3]-m[2])%2
		appendText(strings.Repeat(`\`, backslashes))
		if escaped == 1 {
			appendText(strings.ReplaceAll(markup[m[3]:end], `\[`, "["))
			continue
		}
		tag := markup[m[4]:m[5]]
		name, params, hasParams := strings.Cut(tag, "=")
		if strings.HasPrefix(name, "/") {
			position := utf8.RuneCountInString(markup[:start]) + 2*backslashes
			closing := strings.TrimSpace(name[1:])
			idx := len(stack) - 1
			if closing != "" {
				closing = normalizeStyle(closing)
				for idx >= 0 && stack[idx].name != closing {
					idx--
				}
				if idx < 0 {
					return "", nil, &MarkupError{fmt.Sprintf("closing tag '[%s]' at position %d doesn't match any open tag", tag, position)}
				}
			} else if idx < 0 {
				return "", nil, &MarkupError{fmt.Sprintf("closing tag '[/]' at position %d has nothing to close", position)}
			}
			open := stack[idx]
			stack = append(stack[:idx], stack[idx+1:]...)
			if !strings.HasPrefix(open.name, "@") {
				spans = append(spans, textSpan{open.start, n, open.text})
				continue
			}
			if open.hasParams && open.params != "" {
				if err := checkMetaParams(open.params); err != nil {
					return "", nil, err
				}
			}
			spans = append(spans, textSpan{open.start, n, ""})
			continue
		}
		t := markupTag{start: n, name: normalizeStyle(name), params: params, hasParams: hasParams}
		t.text = t.name
		if hasParams {
			t.text = t.name + " " + params
		}
		stack = append(stack, t)
	}
	if pos < len(markup) {
		appendText(strings.ReplaceAll(markup[pos:], `\[`, "["))
	}
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].text != "" {
			spans = append(spans, textSpan{stack[i].start, n, stack[i].text})
		}
	}
	for i, j := 0, len(spans)-1; i < j; i, j = i+1, j-1 {
		spans[i], spans[j] = spans[j], spans[i]
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return b.String(), spans, nil
}
