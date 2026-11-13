package irc

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Maximum IRC line length without the trailing CRLF
const maxLineBytes = 510

// One parsed protocol line, the prefix, the command and its parameters with the trailing one last
type message struct {
	prefix  string
	command string
	params  []string
}

// Protocol line split into prefix, command and parameters, message tags dropped
func parseMessage(raw string) message {
	var m message
	if strings.HasPrefix(raw, "@") {
		if i := strings.IndexByte(raw, ' '); i >= 0 {
			raw = strings.TrimLeft(raw[i+1:], " ")
		} else {
			return m
		}
	}
	if strings.HasPrefix(raw, ":") {
		if i := strings.IndexByte(raw, ' '); i >= 0 {
			m.prefix, raw = raw[1:i], raw[i+1:]
		} else {
			m.prefix, raw = raw[1:], ""
		}
	}
	trailing, hasTrailing := "", false
	if i := strings.Index(raw, " :"); i >= 0 {
		raw, trailing, hasTrailing = raw[:i], raw[i+2:], true
	} else if strings.HasPrefix(raw, ":") {
		raw, trailing, hasTrailing = "", raw[1:], true
	}
	fields := strings.Fields(raw)
	if len(fields) > 0 {
		m.command = strings.ToUpper(fields[0])
		m.params = fields[1:]
	}
	if hasTrailing {
		m.params = append(m.params, trailing)
	}
	return m
}

// Nickname of a nick, user and host prefix
func extractNick(prefix string) string {
	nick, _, _ := strings.Cut(prefix, "!")
	return nick
}

// Whether the target names a channel rather than a nick
func isChannel(target string) bool {
	return target != "" && strings.ContainsRune("#&+!", rune(target[0]))
}

// Whether the target is safe to place in a command, free of spaces and line breaks
func validTarget(target string) bool {
	return target != "" && !strings.ContainsAny(target, " \r\n\x00")
}

// Text with CR and LF turned into spaces and NUL removed so it cannot inject protocol lines
func stripControlChars(text string) string {
	return strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(text)
}

// Inbound text without mIRC bold, color, italic, underline, reverse, strike, monospace and reset codes
func stripFormatting(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case 0x02, 0x0f, 0x11, 0x16, 0x1d, 0x1e, 0x1f:
		case 0x03:
			i = skipColor(text, i+1, 2) - 1
		case 0x04:
			i = skipHexColor(text, i+1) - 1
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// Index after a color code of up to width digits with an optional comma and background
func skipColor(text string, i, width int) int {
	j := i
	for j < len(text) && j-i < width && text[j] >= '0' && text[j] <= '9' {
		j++
	}
	if j > i && j+1 < len(text) && text[j] == ',' && text[j+1] >= '0' && text[j+1] <= '9' {
		k := j + 1
		for k < len(text) && k-j-1 < width && text[k] >= '0' && text[k] <= '9' {
			k++
		}
		return k
	}
	return j
}

// Index after a hex color code of six hex digits with an optional comma and background
func skipHexColor(text string, i int) int {
	isHex := func(s string) bool {
		if len(s) != 6 {
			return false
		}
		_, err := strconv.ParseUint(s, 16, 32)
		return err == nil
	}
	if i+6 > len(text) || !isHex(text[i:i+6]) {
		return i
	}
	j := i + 6
	if j+7 <= len(text) && text[j] == ',' && isHex(text[j+1:j+7]) {
		return j + 7
	}
	return j
}

// Markdown rewrites applied in order, fences before inline code so fences vanish whole
var markdownRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\*\*(.+?)\*\*`), "${1}"},
	{regexp.MustCompile(`__(.+?)__`), "${1}"},
	{regexp.MustCompile(`\*(.+?)\*`), "${1}"},
}

var (
	codeFenceRE  = regexp.MustCompile("```\\w*\\n?")
	inlineCodeRE = regexp.MustCompile("`(.+?)`")
	mdImageRE    = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	mdLinkRE     = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
)

// Whether the rune counts as a word character for underscore emphasis
func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r)
}

// Underscore emphasis unwrapped where both underscores sit outside a word, within one line
func stripUnderscoreEmphasis(text string) string {
	runes := []rune(text)
	var b strings.Builder
	for i := 0; i < len(runes); i++ {
		if runes[i] != '_' || (i > 0 && isWordRune(runes[i-1])) {
			b.WriteRune(runes[i])
			continue
		}
		end := -1
		for j := i + 1; j < len(runes) && runes[j] != '\n'; j++ {
			if j > i+1 && runes[j] == '_' && (j+1 == len(runes) || !isWordRune(runes[j+1])) {
				end = j
				break
			}
		}
		if end < 0 {
			b.WriteRune(runes[i])
			continue
		}
		b.WriteString(string(runes[i+1 : end]))
		i = end
	}
	return b.String()
}

// Plain text of basic markdown, images reduced to their URL and links to text and URL
func stripMarkdown(text string) string {
	for _, rule := range markdownRules {
		text = rule.re.ReplaceAllString(text, rule.repl)
	}
	text = stripUnderscoreEmphasis(text)
	text = codeFenceRE.ReplaceAllString(text, "")
	text = inlineCodeRE.ReplaceAllString(text, "${1}")
	text = mdImageRE.ReplaceAllString(text, "${2}")
	return mdLinkRE.ReplaceAllString(text, "${1} (${2})")
}

// Payload bytes left in one line after the PRIVMSG command, the target and CRLF
func privmsgBudget(target string) int {
	return maxLineBytes - (len("PRIVMSG "+target+" :") + 2)
}

// Line split into chunks of at most limit bytes, breaking at a space past the first third
func chunkParagraph(paragraph string, limit int) []string {
	var chunks []string
	for paragraph != "" {
		if len(paragraph) <= limit {
			return append(chunks, paragraph)
		}
		splitAt := 0
		for i, r := range paragraph {
			if i+utf8.RuneLen(r) > limit {
				break
			}
			splitAt = i + utf8.RuneLen(r)
		}
		if splitAt == 0 {
			_, size := utf8.DecodeRuneInString(paragraph)
			splitAt = size
		}
		if space := strings.LastIndexByte(paragraph[:splitAt], ' '); space > splitAt/3 {
			splitAt = space
		}
		if chunk := strings.TrimRightFunc(paragraph[:splitAt], unicode.IsSpace); chunk != "" {
			chunks = append(chunks, chunk)
		}
		paragraph = strings.TrimLeftFunc(paragraph[splitAt:], unicode.IsSpace)
	}
	return chunks
}

// Protocol safe PRIVMSG payload lines of a message, markdown stripped, blank lines dropped and long lines split
func splitLines(content, target string, limit int) []string {
	limit = min(limit, privmsgBudget(target))
	var lines []string
	for _, paragraph := range strings.Split(stripMarkdown(content), "\n") {
		paragraph = strings.TrimRightFunc(stripControlChars(paragraph), unicode.IsSpace)
		if strings.TrimSpace(paragraph) == "" {
			continue
		}
		lines = append(lines, chunkParagraph(paragraph, limit)...)
	}
	return lines
}

// Millisecond timestamp used as the id of a sent message
func msID() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// Random 32 hex digit id of an inbound line
func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Nick tried after a collision, base then base_ then base_1, base_2 and onward
func nextNick(base, current string) string {
	if i := strings.LastIndexByte(current, '_'); i >= 0 && isDigits(current[i+1:]) {
		n, _ := strconv.Atoi(current[i+1:])
		return strings.TrimRight(base, "_0123456789") + "_" + strconv.Itoa(n+1)
	}
	if current == base {
		return base + "_"
	}
	return base + "_1"
}

func isDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}
