package tools

import (
	"strings"

	"golang.org/x/net/html"

	"github.com/nickheyer/icebath/pkg/internal/pyhtml"
)

// libxml2 htmlBooleanAttrs, the attributes a value less occurrence spells out as their own name
var lxmlBooleanAttrs = setOf("checked", "compact", "declare", "defer", "disabled", "ismap", "multiple", "nohref", "noresize", "noshade", "nowrap", "readonly", "selected")

// libxml2 htmlC1Remap, the windows-1252 characters numeric references in the C1 range stand for
var lxmlC1Remap = [32]rune{
	0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
	0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
}

// libxml2 htmlEndPriority, an end tag only closes open elements of lower or equal priority
func lxmlEndPriority(name string) int {
	switch name {
	case "div":
		return 150
	case "td", "th":
		return 160
	case "tr":
		return 170
	case "thead", "tbody", "tfoot":
		return 180
	case "table":
		return 190
	case "head", "body":
		return 200
	case "html":
		return 220
	}
	return 100
}

// HTML whitespace, space tab line feed form feed and carriage return
func lxmlWS(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

// Characters of a numeric reference code point the way htmlCodePointToUtf8 maps it
func lxmlCodePoint(c int) string {
	switch {
	case c >= 0x80 && c < 0xA0:
		return string(lxmlC1Remap[c-0x80])
	case c <= 0, c >= 0xD800 && c < 0xE000, c > 0x10FFFF:
		return "�"
	}
	return string(rune(c))
}

// Decodes the character reference at the start of s, attribute values keeping unterminated names before alphanumerics or equals
func lxmlRef(s string, attr bool) (string, int, bool) {
	if len(s) < 2 {
		return "", 0, false
	}
	if s[1] == '#' {
		j, base := 2, 10
		digit := func(c byte) int {
			switch {
			case c >= '0' && c <= '9':
				return int(c - '0')
			case base == 16 && c|0x20 >= 'a' && c|0x20 <= 'f':
				return int(c|0x20-'a') + 10
			}
			return -1
		}
		if j < len(s) && s[j]|0x20 == 'x' {
			j, base = j+1, 16
		}
		if j >= len(s) || digit(s[j]) < 0 {
			return "", 0, false
		}
		cp := 0
		for ; j < len(s) && digit(s[j]) >= 0; j++ {
			if cp = cp*base + digit(s[j]); cp >= 0x110000 {
				cp = 0x110000
			}
		}
		if j < len(s) && s[j] == ';' {
			j++
		}
		return lxmlCodePoint(cp), j, true
	}
	v, n := pyhtml.LongestEntity(s[1:])
	if n == 0 {
		return "", 0, false
	}
	if attr && s[n] != ';' && 1+n < len(s) {
		if c := s[1+n]; c == '=' || c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z' {
			return "", 0, false
		}
	}
	return v, 1 + n, true
}

// Attribute value with references decoded, NUL replaced and carriage returns turned into line feeds
func lxmlAttrValue(raw string) string {
	var b strings.Builder
	for i := 0; i < len(raw); {
		switch c := raw[i]; c {
		case '&':
			if v, n, ok := lxmlRef(raw[i:], true); ok {
				b.WriteString(v)
				i += n
				continue
			}
		case 0:
			b.WriteString("�")
			i++
			continue
		case '\r':
			b.WriteByte('\n')
			if i++; i < len(raw) && raw[i] == '\n' {
				i++
			}
			continue
		}
		b.WriteByte(raw[i])
		i++
	}
	return b.String()
}

// Lower cased tag or attribute name with NUL replaced, the htmlParseHTMLName result
func lxmlName(raw string) string {
	return strings.ReplaceAll(strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, raw), "\x00", "�")
}

// Parses a raw start tag into its name, first occurrence attributes and whether it ends with a slash before the bracket
func lxmlParseTag(raw string) (string, []html.Attribute, bool) {
	i := 1
	j := i
	for j < len(raw) && !lxmlWS(raw[j]) && raw[j] != '/' && raw[j] != '>' {
		j++
	}
	name := lxmlName(raw[i:j])
	var attrs []html.Attribute
	seen := map[string]bool{}
	selfClosing := false
	for i = j; i < len(raw); {
		if lxmlWS(raw[i]) {
			i++
			continue
		}
		if raw[i] == '>' {
			break
		}
		if raw[i] == '/' {
			if i+1 < len(raw) && raw[i+1] == '>' {
				selfClosing = true
				break
			}
			i++
			continue
		}
		j = i + 1
		for j < len(raw) && !lxmlWS(raw[j]) && raw[j] != '/' && raw[j] != '>' && raw[j] != '=' {
			j++
		}
		key := lxmlName(raw[i:j])
		for i = j; i < len(raw) && lxmlWS(raw[i]); i++ {
		}
		val, hasVal := "", false
		if i < len(raw) && raw[i] == '=' {
			hasVal = true
			for i++; i < len(raw) && lxmlWS(raw[i]); i++ {
			}
			if i < len(raw) && (raw[i] == '"' || raw[i] == '\'') {
				if k := strings.IndexByte(raw[i+1:], raw[i]); k >= 0 {
					val, i = raw[i+1:i+1+k], i+2+k
				} else {
					val, i = raw[i+1:], len(raw)
				}
			} else {
				for j = i; j < len(raw) && !lxmlWS(raw[j]) && raw[j] != '>'; j++ {
				}
				val, i = raw[i:j], j
			}
			val = lxmlAttrValue(val)
		}
		if !hasVal && lxmlBooleanAttrs[key] {
			val = key
		}
		if !seen[key] {
			seen[key] = true
			attrs = append(attrs, html.Attribute{Key: key, Val: val})
		}
	}
	return name, attrs, selfClosing
}

// Public identifier of a raw document type declaration, empty when it names none
func lxmlDoctypePublic(raw string) string {
	i := min(len(raw), len("<!DOCTYPE"))
	for i < len(raw) && lxmlWS(raw[i]) {
		i++
	}
	for i < len(raw) && !lxmlWS(raw[i]) && raw[i] != '>' {
		i++
	}
	for i < len(raw) && lxmlWS(raw[i]) {
		i++
	}
	if len(raw)-i < 6 || !strings.EqualFold(raw[i:i+6], "PUBLIC") {
		return ""
	}
	for i += 6; i < len(raw) && lxmlWS(raw[i]); i++ {
	}
	if i >= len(raw) || raw[i] != '"' && raw[i] != '\'' {
		return ""
	}
	q := raw[i]
	end := strings.IndexAny(raw[i+1:], string(q)+">")
	if end < 0 {
		return raw[i+1:]
	}
	return raw[i+1 : i+1+end]
}

// lxmlBuilder replays the libxml2 legacy HTML tree construction with remove_blank_text over an HTML5 token stream
type lxmlBuilder struct {
	doc     *html.Node
	stack   []*html.Node
	insert  int
	depth   int
	public  string
	started bool
	mode    string
	cdata   map[*html.Node]bool
}

func (b *lxmlBuilder) name() string {
	if len(b.stack) == 0 {
		return ""
	}
	return b.stack[len(b.stack)-1].Data
}

// Opens an element under the current node or the document, the htmlnamePush and SAX2 startElement pair
func (b *lxmlBuilder) push(name string, attrs []html.Attribute) {
	if b.insert < 1 && name == "head" {
		b.insert = 1
	}
	if b.insert < 2 && name == "body" {
		b.insert = 2
	}
	n := &html.Node{Type: html.ElementNode, Data: name, Attr: attrs}
	b.parent().AppendChild(n)
	b.stack = append(b.stack, n)
}

func (b *lxmlBuilder) pop() {
	b.stack = b.stack[:len(b.stack)-1]
}

func (b *lxmlBuilder) parent() *html.Node {
	if len(b.stack) == 0 {
		return b.doc
	}
	return b.stack[len(b.stack)-1]
}

// Closes every open element the new start tag implies the end of, htmlAutoClose
func (b *lxmlBuilder) autoClose(newtag string) {
	for len(b.stack) > 0 && lxmlStartClose[[2]string{b.name(), newtag}] {
		b.pop()
	}
}

// Closes the elements above a matching open element unless a higher priority one sits between, htmlAutoCloseOnClose
func (b *lxmlBuilder) autoCloseOnClose(newtag string) {
	priority := lxmlEndPriority(newtag)
	i := len(b.stack) - 1
	for ; i >= 0; i-- {
		if b.stack[i].Data == newtag {
			break
		}
		if lxmlEndPriority(b.stack[i].Data) > priority {
			return
		}
	}
	if i < 0 {
		return
	}
	for b.name() != newtag {
		b.pop()
	}
}

// Opens the html, head or body elements a start tag needs, htmlCheckImplied
func (b *lxmlBuilder) checkImplied(newtag string) {
	if newtag == "html" {
		return
	}
	if len(b.stack) == 0 {
		b.push("html", nil)
	}
	if newtag == "body" || newtag == "head" {
		return
	}
	switch {
	case len(b.stack) <= 1 && (newtag == "script" || newtag == "style" || newtag == "meta" || newtag == "link" || newtag == "title" || newtag == "base"):
		if b.insert >= 1 {
			return
		}
		b.push("head", nil)
	case newtag != "noframes" && newtag != "frame" && newtag != "frameset":
		if b.insert >= 2 {
			return
		}
		for _, n := range b.stack {
			if n.Data == "body" || n.Data == "head" {
				return
			}
		}
		b.push("body", nil)
	}
}

// Prepares the tree for character data outside body, htmlStartCharData
func (b *lxmlBuilder) startCharData() {
	if b.name() == "head" {
		b.autoClose("p")
	}
	b.checkImplied("p")
}

// Appends text to the current node, merged into a preceding text node of the same kind like xmlSAX2Text
func (b *lxmlBuilder) text(s string, cdata bool) {
	p := b.parent()
	if last := p.LastChild; last != nil && last.Type == html.TextNode && b.cdata[last] == cdata {
		last.Data += s
		return
	}
	n := &html.Node{Type: html.TextNode, Data: s}
	b.cdata[n] = cdata
	p.AppendChild(n)
}

// Reports blank character data the parser drops, libxml2 areBlanks with cur the character after the chunk
func (b *lxmlBuilder) areBlanks(s string, cur byte) bool {
	for i := 0; i < len(s); i++ {
		if !lxmlWS(s[i]) {
			return false
		}
	}
	if cur == 0 {
		return true
	}
	if cur != '<' {
		return false
	}
	name := b.name()
	if name == "" || name == "html" || name == "head" {
		return true
	}
	if name == "body" {
		if id := strings.ToLower(b.public); id == "-//w3c//dtd html 4.01//en" || id == "-//w3c//dtd html 4//en" {
			return true
		}
	}
	last := b.parent().LastChild
	switch {
	case last == nil:
		return !lxmlAllowPCData[name]
	case last.Type == html.TextNode:
		return false
	}
	return !lxmlAllowPCData[last.Data]
}

// Delivers one character data chunk, htmlCharDataSAXCallback with remove_blank_text
func (b *lxmlBuilder) charData(s string, cur byte, mode string) {
	if mode != "" && mode != "DATA_RCDATA" {
		b.text(s, true)
		return
	}
	if name := b.name(); name == "" || name == "html" || name == "head" {
		i := 0
		for i < len(s) && lxmlWS(s[i]) {
			i++
		}
		if s = s[i:]; s == "" {
			return
		}
		b.startCharData()
	}
	if mode == "" && b.areBlanks(s, cur) {
		return
	}
	b.text(s, false)
}

// Splits a raw text token into the chunks htmlParseCharData delivers, after the byte following the token
func (b *lxmlBuilder) chars(raw string, after byte) {
	mode := b.mode
	at := func(i int) byte {
		if i < len(raw) {
			return raw[i]
		}
		return after
	}
	start := 0
	flush := func(end int) {
		if end > start {
			b.charData(raw[start:end], at(end), mode)
		}
	}
	if mode != "" && mode != "DATA_RCDATA" {
		b.started = true
	}
	for i := 0; i < len(raw); {
		c := raw[i]
		switch {
		case c == '<' && mode == "":
			flush(i)
			b.started = true
			b.startCharData()
			b.text("<", false)
			i++
			start = i
			continue
		case c == '&' && (mode == "" || mode == "DATA_RCDATA"):
			if v, n, ok := lxmlRef(raw[i:], false); ok {
				flush(i)
				i += n
				start = i
				b.charData(v, at(i), mode)
				b.started = true
				continue
			}
		case c == 0:
			flush(i)
			i++
			start = i
			if mode != "" {
				b.charData("�", at(i), mode)
			}
			continue
		case c == '\r':
			flush(i)
			i++
			start = i
			if at(i) != '\n' {
				b.charData("\n", at(i), mode)
			}
			continue
		}
		if !lxmlWS(c) {
			b.started = true
		}
		i++
	}
	flush(len(raw))
}

// Handles a start tag, htmlParseStartTag followed by the empty and self closing checks of htmlParseElementInternal
func (b *lxmlBuilder) startTag(name string, attrs []html.Attribute, selfClosing bool) {
	b.autoClose(name)
	b.checkImplied(name)
	discard := false
	if len(b.stack) > 0 && name == "html" {
		discard = true
		b.depth++
	}
	if len(b.stack) != 1 && name == "head" {
		discard = true
		b.depth++
	}
	if name == "body" {
		for _, n := range b.stack {
			if n.Data == "body" {
				discard = true
				b.depth++
			}
		}
	}
	if !discard {
		b.push(name, attrs)
	}
	cur := b.name()
	if cur == "" {
		return
	}
	if selfClosing || lxmlEmptyElements[cur] {
		b.pop()
		return
	}
	b.mode = lxmlDataModes[cur]
}

// Handles an end tag, htmlParseEndTag
func (b *lxmlBuilder) endTag(name string) {
	if b.depth > 0 && (name == "html" || name == "body" || name == "head") {
		b.depth--
		return
	}
	found := false
	for _, n := range b.stack {
		found = found || n.Data == name
	}
	if !found {
		return
	}
	b.autoCloseOnClose(name)
	if b.name() == name {
		b.pop()
	}
}

// Builds the document libxml2 2.15 produces for page text with the ddgs HTMLParser options
func lxmlParseHTML(src string) *html.Node {
	b := &lxmlBuilder{doc: &html.Node{Type: html.DocumentNode}, cdata: map[*html.Node]bool{}}
	z := html.NewTokenizer(strings.NewReader(src))
	off := 0
	for {
		tt := z.Next()
		start := off
		if tt == html.ErrorToken {
			rest := src[start:]
			if len(rest) > 1 && rest[0] == '<' && (rest[1]|0x20 >= 'a' && rest[1]|0x20 <= 'z') {
				j := 1
				for j < len(rest) && !lxmlWS(rest[j]) && rest[j] != '/' && rest[j] != '>' {
					j++
				}
				name := lxmlName(rest[1:j])
				b.autoClose(name)
				b.checkImplied(name)
			}
			break
		}
		raw := string(z.Raw())
		off += len(raw)
		after := byte(0)
		if off < len(src) {
			after = src[off]
		}
		mode := b.mode
		b.mode = ""
		switch tt {
		case html.TextToken:
			b.mode = mode
			b.chars(raw, after)
			b.mode = ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, attrs, selfClosing := lxmlParseTag(raw)
			b.started = true
			b.startTag(name, attrs, selfClosing)
			if b.mode == "" {
				z.NextIsNotRawText()
			}
		case html.EndTagToken:
			b.started = true
			name, _, _ := lxmlParseTag("<" + raw[2:])
			b.endTag(name)
		case html.DoctypeToken:
			if !b.started && b.doc.FirstChild == nil {
				b.public = lxmlDoctypePublic(raw)
				b.started = true
			}
		}
	}
	for c := b.doc.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			return b.doc
		}
	}
	return nil
}
