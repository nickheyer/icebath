package telegram

import (
	"fmt"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	fencedCodeRE    = mustTelegramRE(`(?m)^((?:(?!` + "```" + `)[^\n])*)(` + "```" + `[^` + "`" + `\n]*\n)([\s\S]*?)(^[ \t]*` + "```" + `)[ \t]*\r?$`)
	inlineCodeRE    = mustTelegramRE("(`[^`]+`)")
	mdLinkRE        = mustTelegramRE(`\[([^\]]+)\]\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)
	mdHeaderRE      = mustTelegramRE(`(?m)^#{1,6}\s+(.+)$`)
	mdBoldInnerRE   = mustTelegramRE(`\*\*(.+?)\*\*`)
	mdItalicRE      = mustTelegramRE(`\*([^*\n]+)\*`)
	mdStrikeRE      = mustTelegramRE(`~~(.+?)~~`)
	mdSpoilerRE     = mustTelegramRE(`\|\|(.+?)\|\|`)
	mdBlockquoteRE  = mustTelegramRE(`(?m)^((?:\*\*)?>{1,3}) (.+)$`)
	codeSpanSplitRE = mustTelegramRE("(```[\\s\\S]*?```|`[^`]+`)")
	bareBracketRE   = mustTelegramRE(`[(){}]`)
)

// Standard markdown as Telegram MarkdownV2 with code kept verbatim and everything else escaped
func FormatMessage(content string) string {
	if content == "" {
		return content
	}
	var keys []string
	protectedSpans := map[string]string{}
	ph := func(value string) string {
		key := fmt.Sprintf("\x00PH%d\x00", len(keys))
		keys = append(keys, key)
		protectedSpans[key] = value
		return key
	}
	phWrap := func(open, close string) func(m *regexp2.Match) string {
		return func(m *regexp2.Match) string { return ph(open + escapeMdv2(firstGroup(m)) + close) }
	}
	text := gateway.ConvertTableToBullets(content)
	text = telegramSub(fencedCodeRE, text, func(m *regexp2.Match) string {
		body := strings.ReplaceAll(strings.ReplaceAll(m.GroupByNumber(3).String(), `\`, `\\`), "`", "\\`")
		return m.GroupByNumber(1).String() + ph(m.GroupByNumber(2).String()+body+m.GroupByNumber(4).String())
	})
	text = telegramSub(inlineCodeRE, text, func(m *regexp2.Match) string {
		return ph(strings.ReplaceAll(m.String(), `\`, `\\`))
	})
	text = telegramSub(mdLinkRE, text, func(m *regexp2.Match) string {
		url := strings.ReplaceAll(strings.ReplaceAll(m.GroupByNumber(2).String(), `\`, `\\`), ")", `\)`)
		return ph("[" + escapeMdv2(firstGroup(m)) + "](" + url + ")")
	})
	text = telegramSub(mdHeaderRE, text, func(m *regexp2.Match) string {
		inner := telegramSub(mdBoldInnerRE, pystr.Strip(firstGroup(m)), firstGroup)
		return ph("*" + escapeMdv2(inner) + "*")
	})
	text = telegramSub(mdBoldInnerRE, text, phWrap("*", "*"))
	text = telegramSub(mdItalicRE, text, phWrap("_", "_"))
	text = telegramSub(mdStrikeRE, text, phWrap("~", "~"))
	text = telegramSub(mdSpoilerRE, text, phWrap("||", "||"))
	text = telegramSub(mdBlockquoteRE, text, func(m *regexp2.Match) string {
		prefix, body := firstGroup(m), m.GroupByNumber(2).String()
		if strings.HasPrefix(prefix, "**") && strings.HasSuffix(body, "||") {
			return ph(prefix + " " + escapeMdv2(body[:len(body)-2]) + "||")
		}
		return ph(prefix + " " + escapeMdv2(body))
	})
	text = escapeMdv2(text)
	for i := len(keys) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, keys[i], protectedSpans[keys[i]])
	}
	var out strings.Builder
	for idx, seg := range pySplitKeep(codeSpanSplitRE, text) {
		if idx%2 == 1 {
			out.WriteString(seg)
			continue
		}
		runes := []rune(seg)
		out.WriteString(telegramSub(bareBracketRE, seg, func(m *regexp2.Match) string { return escapeBareBracket(m.RuneIndex, runes) }))
	}
	return out.String()
}

// Segments between matches with each first group kept in between, the re.split of a capturing pattern
func pySplitKeep(re *regexp2.Regexp, text string) []string {
	runes := []rune(text)
	var parts []string
	last := 0
	m, err := re.FindStringMatch(text)
	for ; m != nil && err == nil; m, err = re.FindNextMatch(m) {
		parts = append(parts, string(runes[last:m.RuneIndex]), m.GroupByNumber(1).String())
		last = m.RuneIndex + m.RuneLength
	}
	if err != nil {
		panic(err)
	}
	return append(parts, string(runes[last:]))
}

// Bracket at index escaped unless already escaped or delimiting a markdown link
func escapeBareBracket(s int, seg []rune) string {
	ch := string(seg[s])
	if s > 0 && seg[s-1] == '\\' {
		return ch
	}
	if ch == "(" && s > 0 && seg[s-1] == ']' {
		return ch
	}
	if ch == ")" {
		before := string(seg[:s])
		if strings.Contains(before, "](http") || strings.Contains(before, "](") {
			depth := 0
			for j := s - 1; j > max(s-2000, -1); j-- {
				if seg[j] == '(' {
					depth--
					if depth < 0 {
						if j > 0 && seg[j-1] == ']' {
							return ch
						}
						break
					}
				} else if seg[j] == ')' {
					depth++
				}
			}
		}
	}
	return `\` + ch
}
