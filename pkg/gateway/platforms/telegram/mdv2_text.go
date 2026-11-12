package telegram

import (
	"github.com/nickheyer/icebath/pkg/internal/pyhtml"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyre"
)

var (
	mdv2EscapeRE            = mustTelegramRE(`([_*\[\]()~` + "`" + `>#\+\-=|{}.!\\])`)
	mdv2UnescapeRE          = mustTelegramRE(`\\([_*\[\]()~` + "`" + `>#\+\-=|{}.!\\])`)
	mdv2DoubleBoldRE        = mustTelegramRE(`\*\*([^*]+)\*\*`)
	mdv2BoldRE              = mustTelegramRE(`\*([^*]+)\*`)
	mdv2ItalicRE            = mustTelegramRE(`(?<!\w)_([^_]+)_(?!\w)`)
	mdv2StrikeRE            = mustTelegramRE(`~([^~]+)~`)
	mdv2SpoilerRE           = mustTelegramRE(`\|\|([^|]+)\|\|`)
	chunkIndicatorOnFenceRE = mustTelegramRE(`(?m)^` + "```" + ` (?P<indicator>(?:\\)?\(\d+/\d+(?:\\)?\))$`)
	richProtectedRegionRE   = mustTelegramRE(`(?m)(?:` + "```" + `[^\n]*\n[\s\S]*?` + "```" + `)|(?:^[^\n]*\|[^\n]*\n[ \t]*\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)+\|?[ \t]*(?:\n[^\n]*\|[^\n]*)*)`)
	loneNewlineRE           = mustTelegramRE(`(?<!\n)\n(?!\n)`)
)

func mustTelegramRE(pattern string) *regexp2.Regexp {
	re, err := pyre.Compile(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// Every match replaced by the template output, the re.sub of hermes over a compiled Python pattern
func telegramSub(re *regexp2.Regexp, text string, repl func(m *regexp2.Match) string) string {
	out, err := re.ReplaceFunc(text, func(m regexp2.Match) string { return repl(&m) }, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

func firstGroup(m *regexp2.Match) string { return m.GroupByNumber(1).String() }

// HTML bold of the label before the first colon, the whole line bold when it has none
func boldLabelHTML(line string) string {
	label, rest, ok := strings.Cut(line, ":")
	if !ok {
		return "<b>" + pyhtml.Escape(line, true) + "</b>"
	}
	return "<b>" + pyhtml.Escape(label, true) + ":</b>" + pyhtml.Escape(rest, true)
}

// Telegram MarkdownV2 special characters escaped with a preceding backslash
func escapeMdv2(text string) string {
	return telegramSub(mdv2EscapeRE, text, func(m *regexp2.Match) string { return `\` + firstGroup(m) })
}

// MarkdownV2 escapes and bold, italic, strike and spoiler markers removed for the plain text fallback
func stripMdv2(text string) string {
	cleaned := telegramSub(mdv2UnescapeRE, text, firstGroup)
	for _, re := range []*regexp2.Regexp{mdv2DoubleBoldRE, mdv2BoldRE, mdv2ItalicRE, mdv2StrikeRE, mdv2SpoilerRE} {
		cleaned = telegramSub(re, cleaned, firstGroup)
	}
	return cleaned
}

// MarkdownV2 escapes and markers removed for the plain text fallback
func StripMdv2(text string) string { return stripMdv2(text) }

// Chunk marker appended to a synthesized closing fence moved onto its own line
func separateChunkIndicatorFromFence(text string) string {
	return telegramSub(chunkIndicatorOnFenceRE, text, func(m *regexp2.Match) string {
		return "```\n" + firstGroup(m)
	})
}

// Lone newlines turned into hard breaks for sendRichMessage, fenced code and pipe tables left verbatim
func richNormalizeLinebreaks(text string) string {
	if text == "" || !strings.Contains(text, "\n") {
		return text
	}
	soft := func(s string) string {
		return telegramSub(loneNewlineRE, s, func(*regexp2.Match) string { return "  \n" })
	}
	runes := []rune(text)
	var out strings.Builder
	pos := 0
	m, err := richProtectedRegionRE.FindStringMatch(text)
	for ; err == nil && m != nil; m, err = richProtectedRegionRE.FindNextMatch(m) {
		out.WriteString(soft(string(runes[pos:m.RuneIndex])))
		out.WriteString(m.String())
		pos = m.RuneIndex + m.RuneLength
	}
	if err != nil {
		panic(err)
	}
	out.WriteString(soft(string(runes[pos:])))
	return out.String()
}
