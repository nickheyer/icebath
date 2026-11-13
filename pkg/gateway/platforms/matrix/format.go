package matrix

import (
	"bytes"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	ghtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
	"golang.org/x/net/html"

	"github.com/nickheyer/icebath/pkg/internal/pyhtml"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// HTML escape of the text with quotes escaped when asked, like html.escape
func htmlEscape(s string, quote bool) string { return pyhtml.Escape(s, quote) }

var sanitizerAllowedTags = map[string]bool{
	"a": true, "b": true, "blockquote": true, "br": true, "code": true, "del": true, "em": true, "h1": true, "h2": true,
	"h3": true, "h4": true, "h5": true, "h6": true, "hr": true, "i": true, "li": true, "ol": true, "p": true, "pre": true,
	"s": true, "strike": true, "strong": true, "table": true, "tbody": true, "td": true, "th": true, "thead": true,
	"tr": true, "ul": true}

var sanitizerVoidTags = map[string]bool{"br": true, "hr": true}

var (
	safeURLControlRE  = pyre.MustCompile(`[\x00-\x1f\x7f]+`)
	safeURLSchemeRE   = pyre.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):`)
	codeLanguageRE    = pyre.MustCompile(`\A(?:language-[A-Za-z0-9_+.-]{1,64})\Z`)
	sanitizerEntityRE = pyre.MustCompile(`&(?:#(?:[0-9]+|[xX][0-9a-fA-F]+)|[a-zA-Z][-.a-zA-Z0-9]*)`)
)

// Allowlist sanitizer state for Matrix compatible formatted HTML
type matrixHTMLSanitizer struct {
	parts     []string
	skipDepth int
}

// Link target with control characters removed, empty for a scheme outside http, https, matrix and mailto
func sanitizerSafeURL(value string) string {
	stripped := pystr.Strip(reSub(safeURLControlRE, value, func(regexp2.Match) string { return "" }))
	scheme := ""
	if m, ok := reMatch(safeURLSchemeRE, stripped); ok {
		scheme = pystr.Lower(group(*m, 1))
	}
	if scheme != "" && scheme != "http" && scheme != "https" && scheme != "matrix" && scheme != "mailto" {
		return ""
	}
	return stripped
}

// Attributes kept for a tag, a safe href on links and a language class on code
func (s *matrixHTMLSanitizer) safeAttrs(tag string, attrs []html.Attribute) string {
	var safe strings.Builder
	for _, a := range attrs {
		attr := pystr.Lower(a.Key)
		switch {
		case tag == "a" && attr == "href":
			if href := sanitizerSafeURL(a.Val); href != "" {
				safe.WriteString(` href="` + htmlEscape(href, true) + `"`)
			}
		case tag == "code" && attr == "class":
			if _, ok := reMatch(codeLanguageRE, a.Val); ok {
				safe.WriteString(` class="` + htmlEscape(a.Val, true) + `"`)
			}
		}
	}
	return safe.String()
}

// Opening tag kept when allowed, script and style bodies skipped
func (s *matrixHTMLSanitizer) handleStarttag(tag string, attrs []html.Attribute) {
	tag = pystr.Lower(tag)
	switch {
	case tag == "script" || tag == "style":
		s.skipDepth++
	case s.skipDepth == 0 && sanitizerAllowedTags[tag]:
		if sanitizerVoidTags[tag] {
			s.parts = append(s.parts, "<"+tag+">")
		} else {
			s.parts = append(s.parts, "<"+tag+s.safeAttrs(tag, attrs)+">")
		}
	}
}

// Closing tag kept when allowed and not void, leaving a script or style body
func (s *matrixHTMLSanitizer) handleEndtag(tag string) {
	tag = pystr.Lower(tag)
	if (tag == "script" || tag == "style") && s.skipDepth > 0 {
		s.skipDepth--
		return
	}
	if s.skipDepth > 0 || !sanitizerAllowedTags[tag] || sanitizerVoidTags[tag] {
		return
	}
	s.parts = append(s.parts, "</"+tag+">")
}

// Text appended outside skipped bodies
func (s *matrixHTMLSanitizer) emit(text string) {
	if s.skipDepth == 0 {
		s.parts = append(s.parts, text)
	}
}

// Raw text escaped with entity and character references passed through like HTMLParser without charref conversion
func (s *matrixHTMLSanitizer) handleRawText(raw string, atEnd bool) {
	for raw != "" {
		m, err := sanitizerEntityRE.FindStringMatch(raw)
		if err != nil || m == nil {
			s.emit(htmlEscape(raw, true))
			return
		}
		start, _ := m.ByteRange()
		ref := m.String()
		rest := raw[start+len(ref):]
		if rest == "" && atEnd {
			s.emit(htmlEscape(raw, true))
			return
		}
		if start > 0 {
			s.emit(htmlEscape(raw[:start], true))
		}
		if strings.HasPrefix(ref, "&#") {
			s.emit("&#" + ref[2:] + ";")
		} else {
			s.emit("&" + ref[1:] + ";")
		}
		raw = strings.TrimPrefix(rest, ";")
	}
}

// Sanitized Matrix HTML, the escaped input when tokenizing fails
func sanitizeMatrixHTML(input string) string {
	s := &matrixHTMLSanitizer{}
	z := html.NewTokenizer(strings.NewReader(input))
	type tok struct {
		kind  html.TokenType
		name  string
		attrs []html.Attribute
		raw   string
	}
	var toks []tok
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() != io.EOF {
				return htmlEscape(input, true)
			}
			break
		}
		t := tok{kind: tt, raw: string(z.Raw())}
		if tt == html.StartTagToken || tt == html.EndTagToken || tt == html.SelfClosingTagToken {
			name, hasAttr := z.TagName()
			t.name = string(name)
			for hasAttr {
				var key, val []byte
				key, val, hasAttr = z.TagAttr()
				t.attrs = append(t.attrs, html.Attribute{Key: string(key), Val: string(val)})
			}
			if tt == html.StartTagToken && t.name != "script" && t.name != "style" {
				z.NextIsNotRawText()
			}
		}
		toks = append(toks, t)
	}
	for i, t := range toks {
		switch t.kind {
		case html.StartTagToken:
			s.handleStarttag(t.name, t.attrs)
		case html.SelfClosingTagToken:
			s.handleStarttag(t.name, t.attrs)
			s.handleEndtag(t.name)
		case html.EndTagToken:
			s.handleEndtag(t.name)
		case html.TextToken:
			if s.skipDepth > 0 {
				continue
			}
			s.handleRawText(t.raw, i == len(toks)-1)
		}
	}
	return strings.Join(s.parts, "")
}

var htmlBlockParserType = reflect.TypeOf(parser.NewHTMLBlockParser())

// Block parsers of CommonMark without the raw HTML block parser
func blockParsersWithoutHTML() []util.PrioritizedValue {
	return slices.DeleteFunc(parser.DefaultBlockParsers(), func(v util.PrioritizedValue) bool {
		return reflect.TypeOf(v.Value) == htmlBlockParserType
	})
}

// Converter for outbound room messages with tables, hard line breaks and no raw HTML blocks
var matrixMarkdown = goldmark.New(
	goldmark.WithParser(parser.NewParser(
		parser.WithBlockParsers(blockParsersWithoutHTML()...),
		parser.WithInlineParsers(parser.DefaultInlineParsers()...),
		parser.WithParagraphTransformers(parser.DefaultParagraphTransformers()...))),
	goldmark.WithExtensions(extension.Table),
	goldmark.WithRendererOptions(ghtml.WithHardWraps(), ghtml.WithUnsafe()))

// Converter for standalone deliveries with tables and raw HTML passed through
var standaloneMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.Table),
	goldmark.WithRendererOptions(ghtml.WithUnsafe()))

// Markdown rendered by the converter with surrounding whitespace stripped
func renderMarkdown(md goldmark.Markdown, text string) string {
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return htmlEscape(text, true)
	}
	return pystr.Strip(buf.String())
}

// Markdown converted to sanitized org.matrix.custom.html with LaTeX math as data-mx-maths markup
func markdownToHTML(text string) string {
	text = preSanitizeMatrixMarkdown(text)
	text, texStore := latexToTokens(text)
	rendered := renderMarkdown(matrixMarkdown, text)
	if strings.Count(rendered, "<p>") == 1 {
		rendered = strings.ReplaceAll(strings.ReplaceAll(rendered, "<p>", ""), "</p>", "")
	}
	return tokensToMxMaths(sanitizeMatrixHTML(rendered), texStore)
}

var (
	outboundMentionRE    = pyre.MustCompile(`(?<![\w/])(@[0-9A-Za-z._=/-]+:[0-9A-Za-z.-]+(?::\d+)?)`)
	outboundRoomRE       = pyre.MustCompile(`(?<![\w/])@room(?![\w:.-])`)
	protectedRegionREs   = []*regexp2.Regexp{pyre.MustCompile("```[\\s\\S]*?```"), pyre.MustCompile("`[^`\\n]+`"), pyre.MustCompile(`\[[^\]]+\]\([^)]+\)`)}
	markdownImageRE      = pyre.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	mentionSpacingRE     = pyre.MustCompile(`[ \t]{2,}`)
	mentionPunctuationRE = pyre.MustCompile(`\s+([,.;:!?])`)
)

// Text content with HTML and outbound mention metadata, plain when the HTML would outgrow the budget
func (a *MatrixAdapter) buildTextMessageContent(text, msgtype string) map[string]any {
	content := map[string]any{"msgtype": msgtype, "body": text}
	if ids := a.extractOutboundMentions(text); len(ids) > 0 {
		content["m.mentions"] = map[string]any{"user_ids": ids}
	}
	if a.allowRoomMentions && a.hasOutboundRoomMention(text) {
		mentions, ok := content["m.mentions"].(map[string]any)
		if !ok {
			mentions = map[string]any{}
			content["m.mentions"] = mentions
		}
		mentions["room"] = true
	}
	rendered := markdownToHTML(a.injectOutboundMentionLinks(text))
	if rendered != "" && rendered != text {
		content["format"] = "org.matrix.custom.html"
		content["formatted_body"] = rendered
		if contentBytes(content) > maxContentBytes {
			delete(content, "format")
			delete(content, "formatted_body")
		}
	}
	return content
}

// Reply and thread relation metadata applied to an outbound payload
func applyRelationMetadata(content map[string]any, replyTo string, metadata map[string]any) {
	threadID := ""
	if v := metadata["thread_id"]; pyTruthy(v) {
		threadID = strOf(v)
	}
	if replyTo != "" {
		content["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": replyTo}}
	}
	if threadID != "" {
		relatesTo, ok := content["m.relates_to"].(map[string]any)
		if !ok {
			relatesTo = map[string]any{}
		}
		relatesTo["rel_type"] = "m.thread"
		relatesTo["event_id"] = threadID
		relatesTo["is_falling_back"] = true
		if _, ok := relatesTo["m.in_reply_to"]; !ok {
			target := replyTo
			if target == "" {
				target = threadID
			}
			relatesTo["m.in_reply_to"] = map[string]any{"event_id": target}
		}
		content["m.relates_to"] = relatesTo
	}
}

// Distinct outbound user mentions in order, ignoring code spans and links
func (a *MatrixAdapter) extractOutboundMentions(text string) []string {
	protected, _ := protectOutboundMentionRegions(text)
	var ids []string
	m, _ := outboundMentionRE.FindStringMatch(protected)
	for m != nil {
		if id := group(*m, 1); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
		m, _ = outboundMentionRE.FindNextMatch(m)
	}
	return ids
}

// Whether outbound text mentions the room outside protected spans
func (a *MatrixAdapter) hasOutboundRoomMention(text string) bool {
	protected, _ := protectOutboundMentionRegions(text)
	return reSearch(outboundRoomRE, protected)
}

// Outbound mentions wrapped in matrix.to markdown links outside code spans and links
func (a *MatrixAdapter) injectOutboundMentionLinks(text string) string {
	if text == "" {
		return text
	}
	protected, placeholders := protectOutboundMentionRegions(text)
	linked := reSub(outboundMentionRE, protected, func(m regexp2.Match) string {
		id := group(m, 1)
		return "[" + id + "](https://matrix.to/#/" + id + ")"
	})
	for idx, original := range placeholders {
		linked = strings.ReplaceAll(linked, "\x00MENTION_PROTECTED"+itoa(idx)+"\x00", original)
	}
	return linked
}

// Text with fenced code, inline code and markdown links swapped for sentinel tokens, plus the originals
func protectOutboundMentionRegions(text string) (string, []string) {
	var placeholders []string
	protected := text
	for _, re := range protectedRegionREs {
		protected = reSub(re, protected, func(m regexp2.Match) string {
			idx := len(placeholders)
			placeholders = append(placeholders, m.String())
			return "\x00MENTION_PROTECTED" + itoa(idx) + "\x00"
		})
	}
	return protected, placeholders
}

// Whether the bot is mentioned, m.mentions user ids authoritative even without body text
func (a *MatrixAdapter) isBotMentioned(body, formattedBody string, mentionUserIDs []string) bool {
	if len(mentionUserIDs) > 0 && a.userID != "" && slices.Contains(mentionUserIDs, a.userID) {
		return true
	}
	if body == "" && formattedBody == "" {
		return false
	}
	if a.userID != "" && strings.Contains(body, a.userID) {
		return true
	}
	if localpart := a.userLocalpart(); localpart != "" {
		re, err := pyre.CompileIgnoreCase(`\b` + pyreEscape(localpart) + `\b`)
		if err == nil && reSearch(re, body) {
			return true
		}
	}
	return formattedBody != "" && a.userID != "" && strings.Contains(formattedBody, "matrix.to/#/"+a.userID)
}

// Mention check fed from event content with MSC3952 m.mentions authoritative
func (a *MatrixAdapter) contentMentionsBot(body string, content map[string]any) bool {
	var userIDs []string
	if mentions, ok := content["m.mentions"].(map[string]any); ok {
		if list, ok := mentions["user_ids"].([]any); ok {
			for _, v := range list {
				userIDs = append(userIDs, strOf(v))
			}
		}
	}
	formatted, _ := content["formatted_body"].(string)
	return a.isBotMentioned(body, formatted, userIDs)
}

// Localpart of the bot user id, empty when the id has no server part
func (a *MatrixAdapter) userLocalpart() string {
	if a.userID == "" || !strings.Contains(a.userID, ":") {
		return ""
	}
	return strings.TrimLeft(strings.SplitN(a.userID, ":", 2)[0], "@")
}

// Body with explicit full id and at localpart mentions removed and spacing normalized
func (a *MatrixAdapter) stripMention(body string) string {
	if body == "" {
		return ""
	}
	if a.userID != "" {
		body = strings.ReplaceAll(body, a.userID, "")
	}
	if localpart := a.userLocalpart(); localpart != "" {
		if re, err := pyre.CompileIgnoreCase(`(?<![\w])@` + pyreEscape(localpart) + `\b`); err == nil {
			body = reSub(re, body, func(regexp2.Match) string { return "" })
		}
	}
	body = reSub(mentionSpacingRE, body, func(regexp2.Match) string { return " " })
	body = reSub(mentionPunctuationRE, body, func(m regexp2.Match) string { return group(m, 1) })
	return pystr.Strip(body)
}

// Download URL of an mxc URI on the configured homeserver, other URLs unchanged
func (a *MatrixAdapter) mxcToHTTP(mxcURL string) string {
	if !strings.HasPrefix(mxcURL, "mxc://") {
		return mxcURL
	}
	return a.homeserver + "/_matrix/client/v1/media/download/" + mxcURL[6:]
}

// Markdown passed through with image markdown reduced to its URL since media uploads separately
func (a *MatrixAdapter) FormatMessage(content string) string {
	return reSub(markdownImageRE, content, func(m regexp2.Match) string { return group(m, 2) })
}

// Python re.escape of a literal
func pyreEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 128 && !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Decimal text of an int
func itoa(n int) string { return strconv.Itoa(n) }

// Python str of a JSON value, strings unchanged
func strOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}
