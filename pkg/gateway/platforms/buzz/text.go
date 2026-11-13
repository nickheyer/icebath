package buzz

import (
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	presentationMentionSeparator = "​"
	maxAttachmentFilenameBytes   = 120
	eventMetaContentCap          = 500
)

// Python style whitespace class body for the media reference patterns
const wsClass = `\t\n\v\f\r \x{1c}-\x{1f}\x{85}\p{Z}`

var (
	unresolvedMentionRE = regexp.MustCompile(`mention '@([^']+)' does not match a current channel member`)
	mediaURLPattern     = `https?://[^` + wsClass + `<>\[\]()]+/media/[0-9a-f]{64}(?:\.[a-z0-9]{1,10})?(?:\?[^` + wsClass + `<>\[\]()]*)?`
	markdownMediaRE     = regexp.MustCompile(`(?i)!\[([^\]]*)\]\([` + wsClass + `]*(` + mediaURLPattern + `)(?:[` + wsClass + `]+["'][^"']*["'])?[` + wsClass + `]*\)`)
	bareMediaRE         = regexp.MustCompile(`(?i)` + mediaURLPattern)
	mediaPathRE         = regexp.MustCompile(`(?i)^/media/([0-9a-f]{64})(\.[a-z0-9]{1,10})?/?$`)
	trailingBlankRE     = regexp.MustCompile(`[ \t]+\n`)
	blankRunRE          = regexp.MustCompile(`\n{3,}`)
)

// Word character the way a Unicode regex word class sees it
func isWord(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) }

func isHexDigit(r rune) bool { return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') }

func isASCIIAlnum(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// Rune ending just before byte offset i, false at the start of the text
func runeBefore(s string, i int) (rune, bool) {
	if i <= 0 {
		return 0, false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return r, true
}

// Rune starting at byte offset i, false at the end of the text
func runeAt(s string, i int) (rune, bool) {
	if i >= len(s) {
		return 0, false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return r, true
}

// Byte length of a case insensitive match of prefix at the start of s, false when it does not match
func foldPrefix(s, prefix string) (int, bool) {
	n := utf8.RuneCountInString(prefix)
	end := 0
	for i := 0; i < n; i++ {
		if end >= len(s) {
			return 0, false
		}
		_, size := utf8.DecodeRuneInString(s[end:])
		end += size
	}
	if !strings.EqualFold(s[:end], prefix) {
		return 0, false
	}
	return end, true
}

// Whether needle occurs in text with neither neighbour rejected by the boundary checks
func boundedContains(text, needle string, badBefore func(rune) bool, okAfter func(r rune, end bool) bool) bool {
	if needle == "" {
		return false
	}
	for from := 0; from <= len(text)-len(needle); {
		idx := strings.Index(text[from:], needle)
		if idx < 0 {
			return false
		}
		start := from + idx
		end := start + len(needle)
		before, hasBefore := runeBefore(text, start)
		after, hasAfter := runeAt(text, end)
		if (!hasBefore || !badBefore(before)) && okAfter(after, !hasAfter) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		from = start + size
	}
	return false
}

// Non overlapping spans of an at sign name token whose neighbours pass the boundary checks, matched case insensitively
func foldTokenSpans(text, name string, okBefore func(r rune, start bool) bool, okAfter func(r rune, end bool) bool) [][2]int {
	var spans [][2]int
	token := "@" + name
	for i := 0; i < len(text); {
		if text[i] == '@' {
			before, hasBefore := runeBefore(text, i)
			if okBefore(before, !hasBefore) {
				if n, ok := foldPrefix(text[i:], token); ok {
					after, hasAfter := runeAt(text, i+n)
					if okAfter(after, !hasAfter) {
						spans = append(spans, [2]int{i, i + n})
						i += n
						continue
					}
				}
			}
		}
		_, size := utf8.DecodeRuneInString(text[i:])
		i += size
	}
	return spans
}

// Closing punctuation and whitespace that may follow an addressed display name
func mentionTerminator(r rune, end bool) bool {
	return end || isSpace(r) || strings.ContainsRune(",;.!?:)]}", r)
}

// Text with a CLI rejected at sign token made presentation only through an invisible separator after the at sign
func escapeUnresolvedPresentationMention(content, cliError string) (string, bool) {
	m := unresolvedMentionRE.FindStringSubmatch(cliError)
	if m == nil || m[1] == "" {
		return "", false
	}
	spans := foldTokenSpans(content, m[1],
		func(r rune, start bool) bool { return start || isSpace(r) },
		func(r rune, end bool) bool { return end || !(isASCIIAlnum(r) || r == '.' || r == '_' || r == '-') })
	if len(spans) == 0 {
		return "", false
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		b.WriteString(content[prev:sp[0]])
		b.WriteString("@" + presentationMentionSeparator + content[sp[0]+1:sp[1]])
		prev = sp[1]
	}
	b.WriteString(content[prev:])
	return b.String(), true
}

// Spans of an at sign display name that is neither glued to a word or at sign before nor followed by a word character
func displayMentionSpans(text, name string) [][2]int {
	return foldTokenSpans(text, name,
		func(r rune, start bool) bool { return start || !(isWord(r) || r == '@') },
		func(r rune, end bool) bool { return end || !isWord(r) })
}

// Keys ordered longest first, ties kept in their original order
func longestFirst(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool { return utf8.RuneCountInString(out[i]) > utf8.RuneCountInString(out[j]) })
	return out
}

// Text with the spans replaced by a NUL so shorter names cannot match inside them
func consumeSpans(text string, spans [][2]int) string {
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		b.WriteString(text[prev:sp[0]])
		b.WriteByte(0)
		prev = sp[1]
	}
	b.WriteString(text[prev:])
	return b.String()
}

// Tag rows of an event, each row with at least a name
func eventTags(event map[string]any) [][]any {
	list, _ := event["tags"].([]any)
	out := make([][]any, 0, len(list))
	for _, t := range list {
		if row, ok := t.([]any); ok && len(row) > 0 {
			out = append(out, row)
		}
	}
	return out
}

// Target and marker of each well formed NIP-10 e tag
func eTags(event map[string]any) [][2]string {
	var out [][2]string
	for _, tag := range eventTags(event) {
		if len(tag) >= 2 && textOf(tag[0]) == "e" {
			marker := ""
			if len(tag) > 3 {
				marker = textOf(tag[3])
			}
			out = append(out, [2]string{textOf(tag[1]), marker})
		}
	}
	return out
}

// Direct parent id from NIP-10 e tags, the reply marker then the root then the last positional tag
func eventReplyParentID(event map[string]any) string {
	var reply, root, last string
	for _, tag := range eTags(event) {
		target := strip(tag[0])
		if target == "" {
			continue
		}
		last = target
		switch tag[1] {
		case "reply":
			reply = target
		case "root":
			root = target
		}
	}
	for _, id := range []string{reply, root, last} {
		if id != "" {
			return id
		}
	}
	return ""
}

// NIP-10 thread root of an event, a lone reply marker naming the thread starter, empty when top level
func extractThreadRoot(event map[string]any) string {
	var root, reply string
	hasReply := false
	for _, tag := range eTags(event) {
		switch strings.ToLower(tag[1]) {
		case "root":
			root = tag[0]
		case "reply":
			reply, hasReply = tag[0], true
		case "":
			if !hasReply {
				reply, hasReply = tag[0], true
			}
		}
	}
	if root != "" {
		return root
	}
	return reply
}

// Whether the event carries a p tag naming the pubkey
func pTagged(event map[string]any, pubkey string) bool {
	for _, tag := range eventTags(event) {
		if len(tag) > 1 && tag[0] == "p" && strings.ToLower(textOf(tag[1])) == pubkey {
			return true
		}
	}
	return false
}

// First runes of a string
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// Last dot suffix of a file name, empty for dot files and names ending in a dot
func pathSuffix(name string) string {
	i := strings.LastIndex(name, ".")
	if i > 0 && i < len(name)-1 {
		return name[i:]
	}
	return ""
}

// Base name safe for cache files and agent context, bounded in bytes with its extension kept
func safeAttachmentFilename(value string) string {
	name := strings.ReplaceAll(value, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strip(strings.Map(func(r rune) rune {
		if r < 32 || r == 0x7f {
			return -1
		}
		return r
	}, name))
	if name == "" || name == "." || name == ".." {
		return "attachment.bin"
	}
	suffix := pathSuffix(name)
	if len(suffix) > 20 {
		suffix = ""
	}
	stem := strings.TrimSuffix(name, suffix)
	budget := maxAttachmentFilenameBytes - len(suffix)
	if len(stem) > budget {
		stem = strings.ToValidUTF8(stem[:budget], "")
	}
	stem = strings.TrimRight(stem, " .")
	if stem == "" {
		stem = "attachment"
	}
	return stem + suffix
}

// Lowercase host and port of an HTTPS equivalent origin
type origin struct {
	host string
	port int
}

// Port number of a parsed URL, false for a malformed or out of range port
func urlPort(u *url.URL) (int, bool, bool) {
	p := u.Port()
	if p == "" {
		return 0, false, true
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 0 || n > 65535 {
		return 0, false, false
	}
	return n, true, true
}

// Exact HTTPS equivalent origin of a configured host or URL, false when it is not one
func attachmentOrigin(value string) (origin, bool) {
	raw := strip(value)
	if raw == "" {
		return origin{}, false
	}
	if !strings.Contains(raw, "://") {
		raw = "//" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return origin{}, false
	}
	port, has, valid := urlPort(u)
	if !valid {
		return origin{}, false
	}
	if !has || port == 0 {
		port = 443
	}
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if (u.Scheme != "" && u.Scheme != "https" && u.Scheme != "wss") || host == "" {
		return origin{}, false
	}
	return origin{host, port}, true
}

// Port of a URL with the scheme default filled in, false when unknown
func effectivePort(u *url.URL) (int, bool) {
	port, has, valid := urlPort(u)
	if !valid {
		return 0, false
	}
	if has {
		return port, true
	}
	p, ok := map[string]int{"https": 443, "wss": 443, "http": 80, "ws": 80}[u.Scheme]
	return p, ok
}

// Whether the URL is a Buzz media object on the configured relay
func isRelayMediaURL(raw, relayURL string) bool {
	candidate, err := url.Parse(raw)
	if err != nil {
		return false
	}
	relay, err := url.Parse(relayURL)
	if err != nil {
		return false
	}
	if candidate.Scheme != "http" && candidate.Scheme != "https" {
		return false
	}
	if candidate.Hostname() == "" || relay.Hostname() == "" || !strings.EqualFold(candidate.Hostname(), relay.Hostname()) {
		return false
	}
	cp, cok := effectivePort(candidate)
	rp, rok := effectivePort(relay)
	if cok != rok || cp != rp {
		return false
	}
	return mediaPathRE.MatchString(candidate.EscapedPath())
}

// One text span to replace with its replacement
type textReplacement struct {
	start, end int
	with       string
}

// Same relay media URLs of a text in first seen order and the text replacements that remove them
func findRelayMediaRefs(text, relayURL string) ([]string, []textReplacement) {
	var urls []string
	var replacements []textReplacement
	var markdownSpans [][2]int
	addURL := func(u string) {
		for _, existing := range urls {
			if existing == u {
				return
			}
		}
		urls = append(urls, u)
	}
	for _, m := range markdownMediaRE.FindAllStringSubmatchIndex(text, -1) {
		u := text[m[4]:m[5]]
		if !isRelayMediaURL(u, relayURL) {
			continue
		}
		markdownSpans = append(markdownSpans, [2]int{m[0], m[1]})
		replacements = append(replacements, textReplacement{m[0], m[1], strip(text[m[2]:m[3]])})
		addURL(u)
	}
	for _, m := range bareMediaRE.FindAllStringIndex(text, -1) {
		inside := false
		for _, sp := range markdownSpans {
			if sp[0] <= m[0] && m[1] <= sp[1] {
				inside = true
				break
			}
		}
		u := text[m[0]:m[1]]
		if inside || !isRelayMediaURL(u, relayURL) {
			continue
		}
		replacements = append(replacements, textReplacement{m[0], m[1], ""})
		addURL(u)
	}
	return urls, replacements
}

// Text with the replacements applied back to front and blank runs collapsed
func replaceMediaRefs(text string, replacements []textReplacement) string {
	sorted := append([]textReplacement(nil), replacements...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].start != sorted[j].start {
			return sorted[i].start > sorted[j].start
		}
		if sorted[i].end != sorted[j].end {
			return sorted[i].end > sorted[j].end
		}
		return sorted[i].with > sorted[j].with
	})
	for _, r := range sorted {
		text = text[:r.start] + r.with + text[r.end:]
	}
	return strip(blankRunRE.ReplaceAllString(trailingBlankRE.ReplaceAllString(text, "\n"), "\n\n"))
}
