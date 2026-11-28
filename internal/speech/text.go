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

// Text inside one span of emphasis, which never runs past the end of a paragraph
const span = `((?:(?!\n[ \t]*\n).)+?)`

// Names of the HTML tags a reply may hold, which are markup and never read
const htmlNames = `a|abbr|b|big|blockquote|br|center|cite|code|dd|del|details|div|dl|dt|em|font|h[1-6]|hr|i|img|ins|kbd|li|mark|ol|p|pre|q|s|` +
	`samp|small|span|strike|strong|sub|summary|sup|table|tbody|td|tfoot|th|thead|tr|tt|u|ul|var`

var (
	codeBlock = pattern("```[\\s\\S]*?```")
	// A fence nothing closes: what follows it is code, not read, as a closed block is not
	openFence   = pattern("```[\\s\\S]*\\z")
	mdLink      = pattern(`\[([^\]]+)\]\((?:[^()]|\([^)]*\))*\)`)
	mdImage     = pattern(`!\[([^\]]*)\]\((?:[^()]|\([^)]*\))*\)`)
	linkAddress = pattern(`\]\([^\s)]*\)?`)
	inlineCode  = pattern("`((?:(?!\\n[ \\t]*\\n)[^`])+)`")
	bold        = pattern(`(?s)\*\*` + span + `\*\*`)
	// Emphasis of single markers opens before a character that is no space and closes after one; an underscore inside a word, as in
	// snake_case, never does
	underscoreBold   = pattern(`(?s)__` + span + `__`)
	italic           = pattern(`(?s)(?<!\*)\*(?![\s*])` + span + `(?<![\s*])\*(?!\*)`)
	underscoreItalic = pattern(`(?s)(?<![\w_])_(?![\s_])` + span + `(?<![\s_])_(?![\w_])`)
	strike           = pattern(`(?s)~~` + span + `~~`)
	// The one rule of a star that is no markup: a star or a times sign between two numbers, or standing apart with a word or number on
	// each side, is multiplication and said as times; every other star is markup or a footnote mark and is dropped
	arithmetic = pattern(`(?<=\d)[ \t]*[*\u00d7][ \t]*(?=\d)|(?<=[\w)])[ \t]+[*\u00d7][ \t]+(?=[\w(])`)
	// A footnote's mark, where it is cited and where it is given
	footnote    = pattern(`\[\^[^\]\s]+\]:?`)
	headingLine = pattern(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	blockquote  = pattern(`(?m)^[ \t]*(?:>[ \t]?)+`)
	listItem    = pattern(`(?m)^[ \t]*(?:[-*+]|\d+[.)])[ \t]+`)
	rule        = pattern(`(?m)^[ \t]*(?:[-*_][ \t]*){3,}$`)
	tableRule   = pattern(`(?m)^[ \t]*\|?[ \t]*:?-{3,}:?[ \t]*(?:\|[ \t]*:?-{3,}:?[ \t]*)*\|?[ \t]*$`)
	// The pipes at the ends of a row of a table, then those between its cells, each cell read in turn
	tableEdge = pattern(`(?m)^[ \t]*\|[ \t]*|[ \t]*\|[ \t]*$`)
	tablePipe = pattern(`[ \t]*\|[ \t]*`)
	// Tags of HTML a reply may hold, and one whose attributes a cut falls inside
	htmlTag = pattern(`(?i)</?(?:` + htmlNames + `)(?:\s[^<>]*)?/?>`)
	openTag = pattern(`(?i)<(?:` + htmlNames + `)\s[^<>]*\z`)
	// Markers of markup left once every span that closes is taken out: a marker that never closes is dropped, never read
	strayMark = pattern("\\*+|~~|`+|(?<![A-Za-z0-9])_+|_+(?![A-Za-z0-9])")
	// Brackets of a link or an image nothing closed, and any other
	brackets = pattern(`!?\[|\]`)
	// Markup an entity of HTML stands for, dropped once the entities are read as their characters
	entityMarks = strings.NewReplacer("*", " ", "`", " ")
	urlText     = pattern(`https?://\S+`)
	mediaPath   = pattern(`MEDIA:\S+?(?=[.,;:!?)\]]*(?:\s|$))`)
	emoji       = pattern("[\U0001F1E6-\U0001F1FF\U0001F300-\U0001F5FF\U0001F600-\U0001F64F\U0001F680-\U0001F6FF\U0001F700-\U0001F77F\U0001F780-\U0001F7FF" +
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
	thinkBlock      = regexp2.MustCompile(`<(`+thinkNames()+`)[\s>].*?</\1>`, regexp2.Singleline|regexp2.IgnoreCase)
	thinkOpen       = regexp2.MustCompile(`<(?:`+thinkNames()+`)(?=[\s>]|$).*\z`, regexp2.Singleline|regexp2.IgnoreCase)
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

// Markdown and chat formatting taken out with the readable words kept and headings marked: code blocks, a fence that never closes with all
// after it, tags, footnote marks, the addresses of links and images, addresses, the marks of lines (headings, rules, quotes, list items, the
// rows of tables), multiplication said, the spans of code and emphasis, then every marker that never closed, then the entities read as
// their characters. No markup character is left, whatever part of a reply the text is, and a part cut at a space reads as the start of
// the whole
func unmarked(text string) string {
	if text == "" {
		return ""
	}
	text = subText(codeBlock, text, " ")
	text = subText(openFence, text, " ")
	text = subText(htmlTag, text, " ")
	text = subText(openTag, text, " ")
	text = subText(footnote, text, "")
	text = sub(mdImage, text, func(m regexp2.Match) string {
		if alt := group(m, 1); alt != "" {
			return " " + alt + " "
		}
		return " "
	})
	text = sub(mdLink, text, func(m regexp2.Match) string { return group(m, 1) })
	text = subText(linkAddress, text, "")
	text = subText(urlText, text, "")
	text = subText(mediaPath, text, " ")
	text = sub(inlineCode, text, func(m regexp2.Match) string { return group(m, 1) })
	text = sub(headingLine, text, func(m regexp2.Match) string {
		return strings.TrimRightFunc(group(m, 1), unicode.IsSpace) + headingMark
	})
	for _, re := range []*regexp2.Regexp{rule, tableRule, blockquote, listItem, tableEdge} {
		text = subText(re, text, "")
	}
	text = subText(arithmetic, text, " times ")
	for _, re := range []*regexp2.Regexp{bold, underscoreBold, italic, underscoreItalic, strike} {
		text = sub(re, text, func(m regexp2.Match) string { return group(m, 1) })
	}
	text = subText(strayMark, text, " ")
	text = subText(brackets, text, " ")
	text = entityMarks.Replace(html.UnescapeString(text))
	return subText(tablePipe, text, ", ")
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
