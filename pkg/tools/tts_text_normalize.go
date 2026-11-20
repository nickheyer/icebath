package tools

import (
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyhtml"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Cap prepare_spoken_text applies when its caller passes no max_chars
const SpokenTextDefaultMaxChars = 4000

const spokenHeadingMark = "\x00"

var (
	ttsMDCodeBlockRe         = mustPyre("```[\\s\\S]*?```")
	ttsMDLinkRe              = mustPyre(`\[([^\]]+)\]\((?:[^()]|\([^)]*\))*\)`)
	ttsMDImageRe             = mustPyre(`!\[([^\]]*)\]\((?:[^()]|\([^)]*\))*\)`)
	ttsMDInlineCodeRe        = mustPyre("`([^`]+)`")
	ttsMDBoldRe              = mustPyre(`(?s)\*\*(.+?)\*\*`)
	ttsMDUnderscoreBoldRe    = mustPyre(`(?s)__(.+?)__`)
	ttsMDItalicRe            = mustPyre(`(?s)(?<!\*)\*(?!\*)(.+?)(?<!\*)\*(?!\*)`)
	ttsMDUnderscoreItalicRe  = mustPyre(`(?s)(?<!_)_(?!_)(.+?)(?<!_)_(?!_)`)
	ttsMDStrikeRe            = mustPyre(`(?s)~~(.+?)~~`)
	ttsMDHeadingLineRe       = mustPyre(`(?m)^[ \t]{0,3}#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)
	ttsMDBlockquoteRe        = mustPyre(`(?m)^\s*>\s?`)
	ttsMDListItemRe          = mustPyre(`(?m)^\s*(?:[-*+]|\d+[.)])\s+`)
	ttsMDHRRe                = mustPyre(`(?m)^\s*[-*_]{3,}\s*$`)
	ttsMDTablePipeRe         = mustPyre(`\s*\|\s*`)
	ttsURLRe                 = mustPyre(`https?://\S+`)
	ttsMediaPathRe           = mustPyre(`MEDIA:\S+?(?=[.,;:!?)\]]*(?:\s|$))`)
	ttsEmojiRe               = mustPyre("[\U0001F1E6-\U0001F1FF\U0001F300-\U0001F5FF\U0001F600-\U0001F64F\U0001F680-\U0001F6FF\U0001F700-\U0001F77F\U0001F780-\U0001F7FF\U0001F800-\U0001F8FF\U0001F900-\U0001F9FF\U0001FA00-\U0001FAFF\u2600-\u27bf]+")
	ttsSpaceVariants         = strings.NewReplacer("\u00a0", " ", "\u2007", " ", "\u202f", " ")
	ttsBulletGlyphs          = strings.NewReplacer("\u2022", " ", "\u25e6", " ", "\u25aa", " ", "\u25ab", " ")
	ttsVariationSelectors    = strings.NewReplacer("\ufe0e", "", "\ufe0f", "")
	ttsRateRe                = mustPyre(`(?<=\d)\s*/\s*(?=[A-Za-z])`)
	ttsPercentRe             = mustPyre(`(?<=\d)\s*%`)
	ttsBareDegreeNumberRe    = mustPyre(`(?<!\w)([-+]?\d+(?:\.\d+)?)\s*°`)
	ttsFilenameExtRe         = mustPyre(`(?i)[\w.-]{0,60}\.(?:wav|ogg|mp3|flac|m4a|aac|py|pyc|ts|tsx|js|jsx|mjs|cjs|json|yaml|yml|toml|md|mdx|txt|csv|xlsx|xls|pdf|png|jpg|jpeg|gif|webp|svg|log|sql|sh|bash|zsh|rs|go|java|rb|php|html|css|lock|tar|gz|zip|db|sqlite|sqlite3|onnx|pt|bin|env|ini|conf|cfg|xml)\b`)
	ttsUUIDRe                = mustPyre(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	ttsHashPrefixHexRe       = mustPyre(`(?i)\b(?:sha(?:-?256|-?512|-?1|3)?|blake2[ab]?|md5|crc32?)[:\s]+[0-9a-fA-F]{7,64}`)
	ttsHexRunFullRe          = mustPyreFullmatch(`(?<![0-9a-fA-F])[0-9a-fA-F]{7,64}(?![0-9a-fA-F])`)
	ttsIdentifierTokenRe     = mustPyre(`[A-Za-z0-9_./~@-]+`)
	ttsDateTokenFullRe       = mustPyreFullmatch(`\d{4}[-/]\d{1,2}(?:[-/]\d{1,2})?`)
	ttsDigitRe               = mustPyre(`\d`)
	ttsThinkBlockRe          = mustPyre(`(?si)<think[\s>].*?</think>`)
	ttsThinkBlockOpenRe      = mustPyre(`(?si)<think[\s>].*\Z`)
	ttsVerifierFooterRe      = mustPyre("(?m)^\\s*\u26a0\ufe0f?\\s*File-mutation verifier:.*(?:\\n[ \\t]+\u2022.*)*")
	ttsTrailingColonRe       = mustPyre(`([^0-9])(?:[:;]\s*)+$`)
	ttsSmoothBlankRunRe      = mustPyre(`\n{3,}`)
	ttsSmoothSpaceRunRe      = mustPyre(`[ \t]{2,}`)
	ttsSmoothSpaceBeforeRe   = mustPyre(`\s+([,.;:!?])`)
	ttsSmoothMarkGapRe       = mustPyre(`(?<![A-Za-z0-9])[,.;:!?](?=[A-Za-z])`)
	ttsSmoothEllipsisRunRe   = mustPyre(`\.{4,}`)
	ttsFlattenParagraphRe    = mustPyre(`\n{2,}`)
	ttsFlattenPunctNewlineRe = mustPyre(`(?<=[.!?;:,])\n`)
	ttsFlattenNewlineRe      = mustPyre(`\n`)
	ttsFlattenDoubleStopRe   = mustPyre(`\.\s*\.`)
	ttsDegreeUnits           = [][2]string{{"C", "Celsius"}, {"F", "Fahrenheit"}}
	ttsDegreeRangeRes        = ttsUnitPatterns(`(?i)(?<!\w)([-+−]?\d+(?:\.\d+)?)\s*[–—-]\s*([-+−]?\d+(?:\.\d+)?)\s*°\s*%s\b`)
	ttsDegreeNumberRes       = ttsUnitPatterns(`(?i)(?<!\w)([-+]?\d+(?:\.\d+)?)\s*°\s*%s\b`)
	ttsDegreeBareUnitRes     = ttsUnitPatterns(`(?i)°\s*%s\b`)
	ttsUnitWords             = []struct {
		re   *regexp2.Regexp
		word string
	}{
		{mustPyre(`(?i)(?<=\d)\s*km\s*/\s*h\b`), "kilometres per hour"}, {mustPyre(`(?i)(?<=\d)\s*km/h\b`), "kilometres per hour"},
		{mustPyre(`(?i)(?<=\d)\s*mm\b`), "millimetres"}, {mustPyre(`(?i)(?<=\d)\s*cm\b`), "centimetres"}, {mustPyre(`(?i)(?<=\d)\s*m\b`), "metres"},
	}
	ttsCurrencyWords = []struct {
		re   *regexp2.Regexp
		word string
	}{
		{mustPyre(`(?i)NZ\$\s*([\d,]*\d(?:\.\d+)?)`), "New Zealand dollars"}, {mustPyre(`(?i)A\$\s*([\d,]*\d(?:\.\d+)?)`), "Australian dollars"},
		{mustPyre(`(?i)US\$\s*([\d,]*\d(?:\.\d+)?)`), "US dollars"}, {mustPyre(`€\s*([\d,]*\d(?:\.\d+)?)`), "euros"},
		{mustPyre(`£\s*([\d,]*\d(?:\.\d+)?)`), "pounds"}, {mustPyre(`\$\s*([\d,]*\d(?:\.\d+)?)`), "dollars"},
	}
)

func mustPyreFullmatch(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

// One compiled pattern per degree unit, the unit letter substituted into the template
func ttsUnitPatterns(template string) []*regexp2.Regexp {
	out := make([]*regexp2.Regexp, len(ttsDegreeUnits))
	for i, u := range ttsDegreeUnits {
		out[i] = mustPyre(strings.Replace(template, "%s", u[0], 1))
	}
	return out
}

// Every match replaced by the callback result, as re.sub with a function
func ttsSub(re *regexp2.Regexp, text string, repl func(m regexp2.Match) string) string {
	out, err := re.ReplaceFunc(text, repl, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Every match replaced by a fixed string
func ttsSubLiteral(re *regexp2.Regexp, text, repl string) string {
	return ttsSub(re, text, func(regexp2.Match) string { return repl })
}

func ttsGroup(m regexp2.Match, n int) string { return m.GroupByNumber(n).String() }

// Markdown and Telegram formatting removed with the readable words kept and headings marked
func StripMarkdownForTTS(text string) string {
	if text == "" {
		return ""
	}
	text = pyhtml.Unescape(text)
	text = ttsSubLiteral(ttsMDCodeBlockRe, text, " ")
	text = ttsSub(ttsMDImageRe, text, func(m regexp2.Match) string {
		if alt := ttsGroup(m, 1); alt != "" {
			return " " + alt + " "
		}
		return " "
	})
	text = ttsSub(ttsMDLinkRe, text, func(m regexp2.Match) string { return ttsGroup(m, 1) })
	text = ttsSubLiteral(ttsURLRe, text, "")
	text = ttsSubLiteral(ttsMediaPathRe, text, " ")
	for _, re := range []*regexp2.Regexp{ttsMDInlineCodeRe, ttsMDBoldRe, ttsMDUnderscoreBoldRe, ttsMDItalicRe, ttsMDUnderscoreItalicRe, ttsMDStrikeRe} {
		text = ttsSub(re, text, func(m regexp2.Match) string { return ttsGroup(m, 1) })
	}
	text = ttsSub(ttsMDHeadingLineRe, text, func(m regexp2.Match) string { return pystr.Rstrip(ttsGroup(m, 1)) + spokenHeadingMark })
	text = ttsSubLiteral(ttsMDBlockquoteRe, text, "")
	text = ttsSubLiteral(ttsMDListItemRe, text, "")
	text = ttsSubLiteral(ttsMDHRRe, text, "")
	return ttsSubLiteral(ttsMDTablePipeRe, text, "; ")
}

// Temperature ranges such as 11-17 degrees C spoken as 11 to 17 degrees Celsius
func normalizeTemperatureRanges(text string) string {
	for i, u := range ttsDegreeUnits {
		word := u[1]
		text = ttsSub(ttsDegreeRangeRes[i], text, func(m regexp2.Match) string {
			return strings.ReplaceAll(ttsGroup(m, 1), "\u2212", "-") + " to " + strings.ReplaceAll(ttsGroup(m, 2), "\u2212", "-") + " degrees " + word
		})
	}
	return text
}

// Symbols, units, currencies and shorthand expanded into words a voice reads well
func NormalizeSymbolsForTTS(text string) string {
	if text == "" {
		return ""
	}
	text = ttsSpaceVariants.Replace(text)
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\u2212", "-"), "\u2026", "...")
	text = normalizeTemperatureRanges(text)
	for i, u := range ttsDegreeUnits {
		word := u[1]
		text = ttsSub(ttsDegreeNumberRes[i], text, func(m regexp2.Match) string { return ttsGroup(m, 1) + " degrees " + word })
	}
	for i, u := range ttsDegreeUnits {
		text = ttsSubLiteral(ttsDegreeBareUnitRes[i], text, "degrees "+u[1])
	}
	text = ttsSub(ttsBareDegreeNumberRe, text, func(m regexp2.Match) string { return ttsGroup(m, 1) + " degrees" })
	text = strings.ReplaceAll(text, "°", " degrees")
	for _, u := range ttsUnitWords {
		text = ttsSubLiteral(u.re, text, " "+u.word)
	}
	text = ttsSubLiteral(ttsRateRe, text, " per ")
	for _, c := range ttsCurrencyWords {
		word := c.word
		text = ttsSub(c.re, text, func(m regexp2.Match) string { return ttsGroup(m, 1) + " " + word })
	}
	text = ttsSubLiteral(ttsPercentRe, text, " percent")
	text = ttsBulletGlyphs.Replace(strings.ReplaceAll(text, "&", " and "))
	for _, pair := range [][2]string{{"\u2192", " to "}, {"\u21d2", " to "}, {"\u2248", " about "}, {"~", " about "}} {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return ttsSubLiteral(ttsEmojiRe, ttsVariationSelectors.Replace(text), "")
}

func ttsMatches(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// Machine tokens a voice would spell out character by character, emails and dates excluded
func isDenseIdentifier(token string) bool {
	if strings.Contains(token, "@") || ttsMatches(ttsDateTokenFullRe, token) {
		return false
	}
	for _, p := range []string{"~/", "./", "../", "/"} {
		if strings.HasPrefix(token, p) {
			return true
		}
	}
	digits := strings.IndexFunc(token, pystr.IsDigit) >= 0
	if strings.Contains(token, "/") && (ttsMatches(ttsFilenameExtRe, token) || digits) {
		return true
	}
	if ttsMatches(ttsFilenameExtRe, token) || ttsMatches(ttsUUIDRe, token) {
		return true
	}
	if ttsMatches(ttsHexRunFullRe, token) && ttsMatches(ttsDigitRe, token) {
		return true
	}
	if !digits {
		return false
	}
	seps := 0
	for _, c := range []string{"_", ".", "/"} {
		if strings.Contains(token, c) {
			seps++
		}
	}
	hyphens := strings.Count(token, "-")
	if seps >= 2 || hyphens >= 2 {
		return true
	}
	return strings.Contains(token, "/") || (hyphens >= 1 && seps >= 1)
}

// Identifier dense tokens silenced while ordinary prose stays verbatim
func PruneIdentifierTokensForTTS(text string) string {
	if text == "" {
		return ""
	}
	text = ttsSubLiteral(ttsHashPrefixHexRe, text, " ")
	return ttsSub(ttsIdentifierTokenRe, text, func(m regexp2.Match) string {
		if tok := m.String(); !isDenseIdentifier(tok) {
			return tok
		}
		return " "
	})
}

func lastRuneIn(s, set string) bool {
	r, _ := pystr.DecodeLastRune(s)
	return strings.ContainsRune(set, r)
}

// Visual formatting collapsed into spoken paragraphs, marked headings folded into the next line as a lead in
func SmoothWhitespaceForTTS(text string) string {
	if text == "" {
		return ""
	}
	rawLines := pystr.Splitlines(text, false)
	contentLines := 0
	for _, raw := range rawLines {
		if pystr.Strip(strings.ReplaceAll(raw, spokenHeadingMark, "")) != "" {
			contentLines++
		}
	}
	addSentencePauses := contentLines > 1
	var lines []string
	var pending *string
	flushPending := func() {
		if pending != nil {
			lines = append(lines, pystr.RstripChars(*pending, ".:;,")+".")
			pending = nil
		}
	}
	for _, raw := range rawLines {
		isHeading := strings.HasSuffix(pystr.Rstrip(raw), spokenHeadingMark)
		line := pystr.Strip(strings.ReplaceAll(raw, spokenHeadingMark, ""))
		if line == "" {
			if pending == nil && len(lines) > 0 && lines[len(lines)-1] != "" {
				lines = append(lines, "")
			}
			continue
		}
		if isHeading {
			flushPending()
			heading := pystr.RstripChars(line, ".:;,")
			pending = &heading
			continue
		}
		if pending != nil {
			line = pystr.RstripChars(*pending, ".:;,") + ", " + line
			pending = nil
		}
		if addSentencePauses {
			if lastRuneIn(line, ":;") {
				line = pystr.Rstrip(pystr.RstripChars(line, ":;")) + "."
			} else if !lastRuneIn(line, ".!?") {
				line += "."
			}
		}
		lines = append(lines, line)
	}
	flushPending()
	text = strings.Join(lines, "\n")
	text = ttsSubLiteral(ttsSmoothBlankRunRe, text, "\n\n")
	text = ttsSubLiteral(ttsSmoothSpaceRunRe, text, " ")
	text = ttsSub(ttsSmoothSpaceBeforeRe, text, func(m regexp2.Match) string { return ttsGroup(m, 1) })
	text = ttsSub(ttsSmoothMarkGapRe, text, func(m regexp2.Match) string { return m.String() + " " })
	text = ttsSubLiteral(ttsSmoothEllipsisRunRe, text, "...")
	return pystr.Strip(closeTrailingColon(text))
}

// Final colon or semicolon run closed with a period unless a digit precedes it
func closeTrailingColon(text string) string {
	return ttsSub(ttsTrailingColonRe, text, func(m regexp2.Match) string { return ttsGroup(m, 1) + "." })
}

// Think reasoning blocks, unterminated ones included, and the file mutation verifier footer removed
func StripNonspokenBlocks(text string) string {
	if text == "" {
		return ""
	}
	for _, re := range []*regexp2.Regexp{ttsThinkBlockRe, ttsThinkBlockOpenRe, ttsVerifierFooterRe} {
		text = ttsSubLiteral(re, text, " ")
	}
	return text
}

// Newlines collapsed into sentence breaks for providers that truncate at the first newline
func FlattenNewlinesForPayload(text string) string {
	if text == "" {
		return ""
	}
	text = ttsSubLiteral(ttsFlattenParagraphRe, text, ". ")
	text = ttsSubLiteral(ttsFlattenPunctNewlineRe, text, " ")
	text = ttsSubLiteral(ttsFlattenNewlineRe, text, ". ")
	text = ttsSubLiteral(ttsFlattenDoubleStopRe, text, ".")
	text = ttsSubLiteral(ttsSmoothSpaceRunRe, text, " ")
	return pystr.Strip(closeTrailingColon(text))
}

// Spoken script of assistant text cut to maxChars code points, nil or non positive leaving it uncut
func PrepareSpokenText(text string, maxChars *int) string {
	spoken := text
	for _, step := range []func(string) string{StripNonspokenBlocks, StripMarkdownForTTS, NormalizeSymbolsForTTS, PruneIdentifierTokensForTTS, SmoothWhitespaceForTTS, FlattenNewlinesForPayload} {
		spoken = step(spoken)
	}
	if maxChars != nil && *maxChars > 0 {
		if rs := pystr.Runes(spoken); len(rs) > *maxChars {
			spoken = pystr.Rstrip(pystr.FromRunes(rs[:*maxChars]))
		}
	}
	return spoken
}

// Spoken script with no length cap, the name tts_tool and the voice paths call
func PrepareSpokenTextUncapped(text string) string { return PrepareSpokenText(text, nil) }
