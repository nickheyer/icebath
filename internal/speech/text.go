package speech

import (
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
)

// Mark a heading carries until whitespace smoothing folds it into the next line
const headingMark = "\x00"

func pattern(expr string) *regexp2.Regexp { return regexp2.MustCompile(expr, regexp2.None) }

func whole(expr string) *regexp2.Regexp { return regexp2.MustCompile(`^(?:`+expr+`)\z`, regexp2.None) }

var (
	codeBlock        = pattern("```[\\s\\S]*?```")
	mdLink           = pattern(`\[([^\]]+)\]\((?:[^()]|\([^)]*\))*\)`)
	mdImage          = pattern(`!\[([^\]]*)\]\((?:[^()]|\([^)]*\))*\)`)
	inlineCode       = pattern("`([^`]+)`")
	bold             = pattern(`(?s)\*\*(.+?)\*\*`)
	underscoreBold   = pattern(`(?s)__(.+?)__`)
	italic           = pattern(`(?s)(?<!\*)\*(?!\*)(.+?)(?<!\*)\*(?!\*)`)
	underscoreItalic = pattern(`(?s)(?<!_)_(?!_)(.+?)(?<!_)_(?!_)`)
	strike           = pattern(`(?s)~~(.+?)~~`)
	headingLine      = pattern(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	blockquote       = pattern(`(?m)^\s*>\s?`)
	listItem         = pattern(`(?m)^\s*(?:[-*+]|\d+[.)])\s+`)
	rule             = pattern(`(?m)^\s*[-*_]{3,}\s*$`)
	tablePipe        = pattern(`\s*\|\s*`)
	urlText          = pattern(`https?://\S+`)
	mediaPath        = pattern(`MEDIA:\S+?(?=[.,;:!?)\]]*(?:\s|$))`)
	emoji            = pattern("[\U0001F1E6-\U0001F1FF\U0001F300-\U0001F5FF\U0001F600-\U0001F64F\U0001F680-\U0001F6FF\U0001F700-\U0001F77F\U0001F780-\U0001F7FF" +
		"\U0001F800-\U0001F8FF\U0001F900-\U0001F9FF\U0001FA00-\U0001FAFF\u2600-\u27bf]+")
	lineBreaks         = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\u2028", "\n", "\u2029", "\n")
	spaceVariants      = strings.NewReplacer("\u00a0", " ", "\u2007", " ", "\u202f", " ")
	bulletGlyphs       = strings.NewReplacer("\u2022", " ", "\u25e6", " ", "\u25aa", " ", "\u25ab", " ")
	variationSelectors = strings.NewReplacer("\ufe0e", "", "\ufe0f", "")
	rate               = pattern(`(?<=\d)\s*/\s*(?=[A-Za-z])`)
	percent            = pattern(`(?<=\d)\s*%`)
	bareDegreeNumber   = pattern(`(?<!\w)([-+]?\d+(?:\.\d+)?)\s*°`)
	fileExtension      = pattern(`(?i)[\w.-]{0,60}\.(?:wav|ogg|mp3|flac|m4a|aac|py|pyc|ts|tsx|js|jsx|mjs|cjs|json|yaml|yml|toml|md|mdx|txt|csv|xlsx|xls|pdf|png|` +
		`jpg|jpeg|gif|webp|svg|log|sql|sh|bash|zsh|rs|go|java|rb|php|html|css|lock|tar|gz|zip|db|sqlite|sqlite3|onnx|pt|bin|env|ini|conf|cfg|xml)\b`)
	uuidText        = pattern(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hashPrefixHex   = pattern(`(?i)\b(?:sha(?:-?256|-?512|-?1|3)?|blake2[ab]?|md5|crc32?)[:\s]+[0-9a-fA-F]{7,64}`)
	hexRun          = whole(`(?<![0-9a-fA-F])[0-9a-fA-F]{7,64}(?![0-9a-fA-F])`)
	identifierToken = pattern(`[A-Za-z0-9_./~@-]+`)
	dateToken       = whole(`\d{4}[-/]\d{1,2}(?:[-/]\d{1,2})?`)
	digit           = pattern(`\d`)
	thinkBlock      = pattern(`(?si)<think[\s>].*?</think>`)
	thinkOpen       = pattern(`(?si)<think[\s>].*\Z`)
	verifierFooter  = pattern("(?m)^\\s*\u26a0\ufe0f?\\s*File-mutation verifier:.*(?:\\n[ \\t]+\u2022.*)*")
	trailingColon   = pattern(`([^0-9])(?:[:;]\s*)+$`)
	blankRun        = pattern(`\n{3,}`)
	spaceRun        = pattern(`[ \t]{2,}`)
	spaceBeforeMark = pattern(`\s+([,.;:!?])`)
	markGap         = pattern(`(?<![A-Za-z0-9])[,.;:!?](?=[A-Za-z])`)
	ellipsisRun     = pattern(`\.{4,}`)
	paragraphBreak  = pattern(`\n{2,}`)
	punctNewline    = pattern(`(?<=[.!?;:,])\n`)
	newline         = pattern(`\n`)
	doubleStop      = pattern(`\.\s*\.`)
	degreeUnits     = [][2]string{{"C", "Celsius"}, {"F", "Fahrenheit"}}
	degreeRanges    = unitPatterns(`(?i)(?<!\w)([-+−]?\d+(?:\.\d+)?)\s*[–—-]\s*([-+−]?\d+(?:\.\d+)?)\s*°\s*%s\b`)
	degreeNumbers   = unitPatterns(`(?i)(?<!\w)([-+]?\d+(?:\.\d+)?)\s*°\s*%s\b`)
	degreeBareUnits = unitPatterns(`(?i)°\s*%s\b`)
	unitWords       = []spoken{{pattern(`(?i)(?<=\d)\s*km\s*/\s*h\b`), "kilometres per hour"}, {pattern(`(?i)(?<=\d)\s*km/h\b`), "kilometres per hour"},
		{pattern(`(?i)(?<=\d)\s*mm\b`), "millimetres"}, {pattern(`(?i)(?<=\d)\s*cm\b`), "centimetres"}, {pattern(`(?i)(?<=\d)\s*m\b`), "metres"}}
	currencyWords = []spoken{{pattern(`(?i)NZ\$\s*([\d,]*\d(?:\.\d+)?)`), "New Zealand dollars"}, {pattern(`(?i)A\$\s*([\d,]*\d(?:\.\d+)?)`), "Australian dollars"},
		{pattern(`(?i)US\$\s*([\d,]*\d(?:\.\d+)?)`), "US dollars"}, {pattern(`€\s*([\d,]*\d(?:\.\d+)?)`), "euros"},
		{pattern(`£\s*([\d,]*\d(?:\.\d+)?)`), "pounds"}, {pattern(`\$\s*([\d,]*\d(?:\.\d+)?)`), "dollars"}}
)

// A pattern and the words it reads as
type spoken struct {
	re   *regexp2.Regexp
	word string
}

// One pattern per degree unit, the unit letter put into the template
func unitPatterns(template string) []*regexp2.Regexp {
	out := make([]*regexp2.Regexp, len(degreeUnits))
	for i, unit := range degreeUnits {
		out[i] = pattern(strings.Replace(template, "%s", unit[0], 1))
	}
	return out
}

func sub(re *regexp2.Regexp, text string, replace func(m regexp2.Match) string) string {
	out, err := re.ReplaceFunc(text, replace, -1, -1)
	if err != nil {
		return text
	}
	return out
}

func subText(re *regexp2.Regexp, text, replacement string) string {
	return sub(re, text, func(regexp2.Match) string { return replacement })
}

func group(m regexp2.Match, n int) string { return m.GroupByNumber(n).String() }

func matches(re *regexp2.Regexp, text string) bool {
	ok, err := re.MatchString(text)
	return err == nil && ok
}

// Spoken script of text an assistant wrote: reasoning blocks, markdown, symbols, identifiers and layout turned into what a voice reads
// well, cut to limit characters when the limit is positive
func Spoken(text string, limit int) string {
	for _, step := range []func(string) string{nonspoken, unmarked, symbols, identifiers, smoothed, flattened} {
		text = step(text)
	}
	if runes := []rune(text); limit > 0 && len(runes) > limit {
		text = strings.TrimRightFunc(string(runes[:limit]), unicode.IsSpace)
	}
	return text
}

// Reasoning blocks, unterminated ones too, and the file mutation verifier's footer taken out
func nonspoken(text string) string {
	for _, re := range []*regexp2.Regexp{thinkBlock, thinkOpen, verifierFooter} {
		text = subText(re, text, " ")
	}
	return text
}

// Markdown and chat formatting taken out with the readable words kept and headings marked
func unmarked(text string) string {
	if text == "" {
		return ""
	}
	text = html.UnescapeString(text)
	text = subText(codeBlock, text, " ")
	text = sub(mdImage, text, func(m regexp2.Match) string {
		if alt := group(m, 1); alt != "" {
			return " " + alt + " "
		}
		return " "
	})
	text = sub(mdLink, text, func(m regexp2.Match) string { return group(m, 1) })
	text = subText(urlText, text, "")
	text = subText(mediaPath, text, " ")
	for _, re := range []*regexp2.Regexp{inlineCode, bold, underscoreBold, italic, underscoreItalic, strike} {
		text = sub(re, text, func(m regexp2.Match) string { return group(m, 1) })
	}
	text = sub(headingLine, text, func(m regexp2.Match) string {
		return strings.TrimRightFunc(group(m, 1), unicode.IsSpace) + headingMark
	})
	text = subText(blockquote, text, "")
	text = subText(listItem, text, "")
	text = subText(rule, text, "")
	return subText(tablePipe, text, "; ")
}

// Symbols, units, currencies and shorthand spelled out as words
func symbols(text string) string {
	if text == "" {
		return ""
	}
	text = spaceVariants.Replace(text)
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\u2212", "-"), "\u2026", "...")
	for i, unit := range degreeUnits {
		word := unit[1]
		text = sub(degreeRanges[i], text, func(m regexp2.Match) string {
			return strings.ReplaceAll(group(m, 1), "\u2212", "-") + " to " + strings.ReplaceAll(group(m, 2), "\u2212", "-") + " degrees " + word
		})
	}
	for i, unit := range degreeUnits {
		word := unit[1]
		text = sub(degreeNumbers[i], text, func(m regexp2.Match) string { return group(m, 1) + " degrees " + word })
	}
	for i, unit := range degreeUnits {
		text = subText(degreeBareUnits[i], text, "degrees "+unit[1])
	}
	text = sub(bareDegreeNumber, text, func(m regexp2.Match) string { return group(m, 1) + " degrees" })
	text = strings.ReplaceAll(text, "°", " degrees")
	for _, unit := range unitWords {
		text = subText(unit.re, text, " "+unit.word)
	}
	text = subText(rate, text, " per ")
	for _, currency := range currencyWords {
		word := currency.word
		text = sub(currency.re, text, func(m regexp2.Match) string { return group(m, 1) + " " + word })
	}
	text = subText(percent, text, " percent")
	text = bulletGlyphs.Replace(strings.ReplaceAll(text, "&", " and "))
	for _, pair := range [][2]string{{"\u2192", " to "}, {"\u21d2", " to "}, {"\u2248", " about "}, {"~", " about "}} {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return subText(emoji, variationSelectors.Replace(text), "")
}

// Machine tokens a voice would spell out character by character, emails and dates left alone
func dense(token string) bool {
	if strings.Contains(token, "@") || matches(dateToken, token) {
		return false
	}
	for _, prefix := range []string{"~/", "./", "../", "/"} {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	digits := strings.IndexFunc(token, unicode.IsDigit) >= 0
	if strings.Contains(token, "/") && (matches(fileExtension, token) || digits) {
		return true
	}
	if matches(fileExtension, token) || matches(uuidText, token) || matches(hexRun, token) && matches(digit, token) {
		return true
	}
	if !digits {
		return false
	}
	separators := 0
	for _, c := range []string{"_", ".", "/"} {
		if strings.Contains(token, c) {
			separators++
		}
	}
	hyphens := strings.Count(token, "-")
	if separators >= 2 || hyphens >= 2 {
		return true
	}
	return strings.Contains(token, "/") || hyphens >= 1 && separators >= 1
}

// Identifier dense tokens silenced, ordinary prose kept as written
func identifiers(text string) string {
	if text == "" {
		return ""
	}
	text = subText(hashPrefixHex, text, " ")
	return sub(identifierToken, text, func(m regexp2.Match) string {
		if token := m.String(); !dense(token) {
			return token
		}
		return " "
	})
}

func endsIn(s, set string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return strings.ContainsRune(set, r)
}

// Visual layout collapsed into spoken paragraphs, a marked heading folded into the line after it as a lead in
func smoothed(text string) string {
	if text == "" {
		return ""
	}
	raw := strings.Split(lineBreaks.Replace(text), "\n")
	content := 0
	for _, line := range raw {
		if strings.TrimSpace(strings.ReplaceAll(line, headingMark, "")) != "" {
			content++
		}
	}
	pauses := content > 1
	var lines []string
	var pending *string
	flush := func() {
		if pending != nil {
			lines = append(lines, strings.TrimRight(*pending, ".:;,")+".")
			pending = nil
		}
	}
	for _, line := range raw {
		heading := strings.HasSuffix(strings.TrimRightFunc(line, unicode.IsSpace), headingMark)
		line = strings.TrimSpace(strings.ReplaceAll(line, headingMark, ""))
		if line == "" {
			if pending == nil && len(lines) > 0 && lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			continue
		}
		if heading {
			flush()
			lead := strings.TrimRight(line, ".:;,")
			pending = &lead
			continue
		}
		if pending != nil {
			line = strings.TrimRight(*pending, ".:;,") + ", " + line
			pending = nil
		}
		if pauses {
			if endsIn(line, ":;") {
				line = strings.TrimRightFunc(strings.TrimRight(line, ":;"), unicode.IsSpace) + "."
			} else if !endsIn(line, ".!?") {
				line += "."
			}
		}
		lines = append(lines, line)
	}
	flush()
	text = strings.Join(lines, "\n")
	text = subText(blankRun, text, "\n\n")
	text = subText(spaceRun, text, " ")
	text = sub(spaceBeforeMark, text, func(m regexp2.Match) string { return group(m, 1) })
	text = sub(markGap, text, func(m regexp2.Match) string { return m.String() + " " })
	text = subText(ellipsisRun, text, "...")
	return strings.TrimSpace(closeColon(text))
}

// A final colon or semicolon run closed with a period unless a digit comes before it
func closeColon(text string) string {
	return sub(trailingColon, text, func(m regexp2.Match) string { return group(m, 1) + "." })
}

// Newlines turned into sentence breaks, for services that stop reading at the first newline
func flattened(text string) string {
	if text == "" {
		return ""
	}
	text = subText(paragraphBreak, text, ". ")
	text = subText(punctNewline, text, " ")
	text = subText(newline, text, ". ")
	text = subText(doubleStop, text, ".")
	text = subText(spaceRun, text, " ")
	return strings.TrimSpace(closeColon(text))
}
