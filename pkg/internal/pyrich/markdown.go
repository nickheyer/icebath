package pyrich

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Renderable a panel draws, lines at a width plus the maximum width it measures within a limit
type Renderable interface {
	RenderLines(color bool, system string, width int) []string
	MeasureMax(width int) int
}

// Markdown document rendered as rich.markdown.Markdown does with the default theme and monokai code blocks
type Markdown struct {
	Source string
}

var markdownTheme = map[string]Style{
	"markdown.em": {Italic: 1}, "markdown.strong": {Bold: 1}, "markdown.code": {Bold: 1, Color: "cyan", Bgcolor: "black"},
	"markdown.code_block": {Color: "cyan", Bgcolor: "black"}, "markdown.block_quote": {Color: "magenta"},
	"markdown.item.bullet": {Bold: 1}, "markdown.item.number": {Color: "cyan"}, "markdown.hr": {Dim: 1},
	"markdown.h1": {Bold: 1, Underline: 1}, "markdown.h2": {Color: "magenta", Underline: 1}, "markdown.h3": {Color: "magenta", Bold: 1},
	"markdown.h4": {Color: "magenta", Italic: 1}, "markdown.h5": {Italic: 1}, "markdown.h6": {Dim: 1},
	"markdown.link": {Color: "bright_blue"}, "markdown.link_url": {Color: "blue", Underline: 1}, "markdown.s": {Strike: 1},
	"markdown.table.border": {Color: "cyan"}, "markdown.table.header": {Color: "cyan", Bold: -1}, "markdown.kbd": {Bold: 1, Color: "bright_yellow"},
	"table.header": {Bold: 1},
}

var oscRE = regexp.MustCompile("\x1b\\]8;[^\x1b]*\x1b\\\\")

type span struct {
	text  string
	style Style
}

type styledText []span

func (t styledText) plain() string {
	var b strings.Builder
	for _, s := range t {
		b.WriteString(s.text)
	}
	return b.String()
}

func (t styledText) add(s string, st Style) styledText {
	s = stripControlCodes(s)
	if s == "" {
		return t
	}
	if n := len(t); n > 0 && t[n-1].style == (Style{}) && st == (Style{}) {
		out := append(styledText{}, t...)
		out[n-1].text += s
		return out
	}
	return append(append(styledText{}, t...), span{s, st})
}

// Spans covering the rune range from to to
func (t styledText) slice(from, to int) styledText {
	var out styledText
	pos := 0
	for _, s := range t {
		n := utf8.RuneCountInString(s.text)
		a, b := max(from, pos), min(to, pos+n)
		if a < b {
			r := []rune(s.text)
			out = out.add(string(r[a-pos:b-pos]), s.style)
		}
		pos += n
	}
	return out
}

func (t styledText) runeLen() int { return utf8.RuneCountInString(t.plain()) }

func (t styledText) rstrip() styledText {
	p := []rune(t.plain())
	n := len(p)
	for n > 0 && unicode.IsSpace(p[n-1]) {
		n--
	}
	return t.slice(0, n)
}

// Cut to width cells, a split double width rune becoming a space, as rich set_cell_size crops
func (t styledText) crop(width int) styledText {
	if CellLen(t.plain()) <= width {
		return t
	}
	var out styledText
	cells := 0
	for _, s := range t {
		var b strings.Builder
		for _, r := range s.text {
			w := CellLen(string(r))
			if cells+w > width {
				b.WriteString(strings.Repeat(" ", width-cells))
				return out.add(b.String(), s.style)
			}
			b.WriteRune(r)
			cells += w
		}
		out = out.add(b.String(), s.style)
	}
	return out
}

func (t styledText) padTo(width int, st Style) styledText {
	return t.add(strings.Repeat(" ", max(0, width-CellLen(t.plain()))), st)
}

func (t styledText) expandTabs(size int) styledText {
	if !strings.Contains(t.plain(), "\t") {
		return t
	}
	var out styledText
	col := 0
	for _, s := range t {
		var b strings.Builder
		for _, r := range s.text {
			if r == '\t' {
				n := size - col%size
				b.WriteString(strings.Repeat(" ", n))
				col += n
				continue
			}
			b.WriteRune(r)
			col += CellLen(string(r))
		}
		out = out.add(b.String(), s.style)
	}
	return out
}

// Lines of the text wrapped at width and justified, as rich Text.wrap with fold overflow
func (t styledText) wrap(width int, justify string, padStyle Style) []styledText {
	var out []styledText
	plain := []rune(t.plain())
	start := 0
	for i := 0; i <= len(plain); i++ {
		if i < len(plain) && plain[i] != '\n' {
			continue
		}
		line := t.slice(start, i).expandTabs(8)
		start = i + 1
		offsets := DivideLine(line.plain(), width, true)
		bounds := append(append([]int{0}, offsets...), line.runeLen())
		for k := 0; k+1 < len(bounds); k++ {
			piece := line.slice(bounds[k], bounds[k+1])
			if n := piece.runeLen(); n > width {
				p := []rune(piece.plain())
				ws := 0
				for ws < len(p) && unicode.IsSpace(p[len(p)-1-ws]) {
					ws++
				}
				piece = piece.slice(0, n-min(ws, n-width))
			}
			switch justify {
			case "left":
				piece = piece.crop(width).padTo(width, padStyle)
			case "center":
				piece = piece.rstrip().crop(width)
				left := (width - CellLen(piece.plain())) / 2
				piece = styledText{}.add(strings.Repeat(" ", left), padStyle).join(piece).padTo(width, padStyle)
			case "right":
				piece = piece.rstrip().crop(width)
				piece = styledText{}.add(strings.Repeat(" ", width-CellLen(piece.plain())), padStyle).join(piece)
			}
			out = append(out, piece.crop(width))
		}
	}
	return out
}

func (t styledText) join(o styledText) styledText {
	out := append(styledText{}, t...)
	for _, s := range o {
		out = out.add(s.text, s.style)
	}
	return out
}

func (t styledText) render(color bool, system string) string {
	if !color {
		return t.plain()
	}
	var b strings.Builder
	for _, s := range t {
		b.WriteString(s.style.Render(s.text, system))
	}
	return b.String()
}

type mdToken struct {
	typ, tag      string
	nesting       int
	content, info string
	href, src     string
	align         string
	start         int
}

type mdBuilder struct {
	src  []byte
	toks []mdToken
}

type mdStrike struct{}

func (mdStrike) IsDelimiter(b byte) bool                              { return b == '~' }
func (mdStrike) CanOpenCloser(o, c *parser.Delimiter) bool            { return o.Char == c.Char }
func (mdStrike) OnMatch(consumes int) gast.Node                       { return east.NewStrikethrough() }
func (mdStrikeParser) Trigger() []byte                                { return []byte{'~'} }
func (mdStrikeParser) CloseBlock(parent gast.Node, pc parser.Context) {}

type mdStrikeParser struct{}

// Strikethrough delimiter run of two or more tildes, an odd run leading with a literal tilde as markdown-it does
func (mdStrikeParser) Parse(parent gast.Node, block text.Reader, pc parser.Context) gast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	node := parser.ScanDelimiter(line, before, 2, mdStrike{})
	if node == nil {
		return nil
	}
	total := node.OriginalLength
	if total%2 == 1 {
		parent.AppendChild(parent, gast.NewTextSegment(segment.WithStop(segment.Start+1)))
		segment = segment.WithStart(segment.Start + 1)
		node.OriginalLength--
		node.Length--
	}
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(total)
	pc.PushDelimiter(node)
	return node
}

// Inline text with backslash escapes and entity references resolved, as markdown-it text tokens carry it
func mdDecode(b []byte) string {
	var out []byte
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\\' && i+1 < len(b) && util.IsPunct(b[i+1]) {
			out = append(out, b[i+1])
			i++
			continue
		}
		if c == '&' {
			if j := bytes.IndexByte(b[i:], ';'); j > 1 && j < 34 {
				ref := b[i : i+j+1]
				if r := util.ResolveNumericReferences(util.ResolveEntityNames(ref)); !bytes.Equal(r, ref) {
					out = append(out, r...)
					i += j
					continue
				}
			}
		}
		out = append(out, c)
	}
	return string(out)
}

func (b *mdBuilder) push(t mdToken) { b.toks = append(b.toks, t) }

func (b *mdBuilder) wrapped(typ, tag string, n gast.Node, body func()) {
	b.push(mdToken{typ: typ + "_open", tag: tag, nesting: 1})
	body()
	b.push(mdToken{typ: typ + "_close", tag: tag, nesting: -1})
}

func (b *mdBuilder) children(n gast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		b.block(c)
	}
}

func linesOf(n gast.Node, src []byte) string {
	var buf bytes.Buffer
	for i := 0; i < n.Lines().Len(); i++ {
		seg := n.Lines().At(i)
		buf.Write(seg.Value(src))
	}
	return buf.String()
}

// Markdown-it block tokens of a goldmark node, inline content flattened the way rich walks it
func (b *mdBuilder) block(n gast.Node) {
	switch n := n.(type) {
	case *gast.Paragraph, *gast.TextBlock:
		b.wrapped("paragraph", "p", n, func() { b.inline(n) })
	case *gast.Heading:
		b.wrapped("heading", fmt.Sprintf("h%d", n.Level), n, func() { b.inline(n) })
	case *gast.Blockquote:
		b.wrapped("blockquote", "blockquote", n, func() { b.children(n) })
	case *gast.List:
		if n.IsOrdered() {
			b.push(mdToken{typ: "ordered_list_open", tag: "ol", nesting: 1, start: n.Start})
			b.children(n)
			b.push(mdToken{typ: "ordered_list_close", tag: "ol", nesting: -1})
		} else {
			b.wrapped("bullet_list", "ul", n, func() { b.children(n) })
		}
	case *gast.ListItem:
		b.wrapped("list_item", "li", n, func() { b.children(n) })
	case *gast.ThematicBreak:
		b.push(mdToken{typ: "hr", tag: "hr"})
	case *gast.FencedCodeBlock:
		info := ""
		if n.Info != nil {
			info = strings.TrimSpace(mdDecode(n.Info.Segment.Value(b.src)))
		}
		b.push(mdToken{typ: "fence", tag: "code", content: linesOf(n, b.src), info: info})
	case *gast.CodeBlock:
		b.push(mdToken{typ: "code_block", tag: "code", content: linesOf(n, b.src)})
	case *gast.HTMLBlock:
		content := linesOf(n, b.src)
		if n.HasClosure() {
			content += string(n.ClosureLine.Value(b.src))
		}
		b.push(mdToken{typ: "html_block", content: content})
	case *east.Table:
		b.wrapped("table", "table", n, func() {
			var rows []gast.Node
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if h, ok := c.(*east.TableHeader); ok {
					b.wrapped("thead", "thead", h, func() { b.row(h, "th") })
				} else {
					rows = append(rows, c)
				}
			}
			if len(rows) > 0 {
				b.wrapped("tbody", "tbody", n, func() {
					for _, r := range rows {
						b.row(r, "td")
					}
				})
			}
		})
	default:
		b.children(n)
	}
}

func (b *mdBuilder) row(r gast.Node, cell string) {
	b.wrapped("tr", "tr", r, func() {
		for c := r.FirstChild(); c != nil; c = c.NextSibling() {
			align := ""
			if tc, ok := c.(*east.TableCell); ok && tc.Alignment != east.AlignNone {
				align = tc.Alignment.String()
			}
			b.push(mdToken{typ: cell + "_open", tag: cell, nesting: 1, align: align})
			b.inline(c)
			b.push(mdToken{typ: cell + "_close", tag: cell, nesting: -1})
		}
	})
}

func (b *mdBuilder) inline(parent gast.Node) {
	start := len(b.toks)
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		b.inlineNode(c)
	}
	if len(b.toks) == start {
		b.push(mdToken{typ: "inline"})
	}
}

func rawText(n gast.Node, src []byte) string {
	var buf strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*gast.Text); ok {
			buf.Write(t.Segment.Value(src))
		} else if s, ok := c.(*gast.String); ok {
			buf.Write(s.Value)
		} else {
			buf.WriteString(rawText(c, src))
		}
	}
	return buf.String()
}

// Raw source of an image label between its brackets, the content markdown-it gives an image token
func (b *mdBuilder) imageLabel(n *gast.Image) string {
	lo, hi := -1, -1
	var visit func(gast.Node)
	visit = func(x gast.Node) {
		for c := x.FirstChild(); c != nil; c = c.NextSibling() {
			if t, ok := c.(*gast.Text); ok {
				if lo < 0 || t.Segment.Start < lo {
					lo = t.Segment.Start
				}
				hi = max(hi, t.Segment.Stop)
			}
			visit(c)
		}
	}
	visit(n)
	if lo < 0 {
		return ""
	}
	open := bytes.LastIndex(b.src[:lo], []byte("!["))
	end := bytes.IndexByte(b.src[hi:], ']')
	if open < 0 || end < 0 {
		return string(b.src[lo:hi])
	}
	return string(b.src[open+2 : hi+end])
}

func (b *mdBuilder) inlineNode(n gast.Node) {
	switch n := n.(type) {
	case *gast.Text:
		if n.IsRaw() {
			b.push(mdToken{typ: "text", content: string(n.Segment.Value(b.src))})
		} else {
			b.push(mdToken{typ: "text", content: mdDecode(n.Segment.Value(b.src))})
		}
		if n.HardLineBreak() {
			b.push(mdToken{typ: "hardbreak", tag: "br"})
		} else if n.SoftLineBreak() {
			b.push(mdToken{typ: "softbreak", tag: "br"})
		}
	case *gast.String:
		b.push(mdToken{typ: "text", content: string(n.Value)})
	case *gast.CodeSpan:
		b.push(mdToken{typ: "code_inline", tag: "code", content: strings.ReplaceAll(rawText(n, b.src), "\n", " ")})
	case *gast.Emphasis:
		tag := "em"
		if n.Level == 2 {
			tag = "strong"
		}
		b.push(mdToken{typ: tag + "_open", tag: tag, nesting: 1})
		b.inlineChildren(n)
		b.push(mdToken{typ: tag + "_close", tag: tag, nesting: -1})
	case *east.Strikethrough:
		b.push(mdToken{typ: "s_open", tag: "s", nesting: 1})
		b.inlineChildren(n)
		b.push(mdToken{typ: "s_close", tag: "s", nesting: -1})
	case *gast.Link:
		b.push(mdToken{typ: "link_open", tag: "a", nesting: 1, href: string(util.URLEscape(n.Destination, true))})
		b.inlineChildren(n)
		b.push(mdToken{typ: "link_close", tag: "a", nesting: -1})
	case *gast.AutoLink:
		url := string(n.URL(b.src))
		if n.AutoLinkType == gast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(url), "mailto:") {
			url = "mailto:" + url
		}
		b.push(mdToken{typ: "link_open", tag: "a", nesting: 1, href: string(util.URLEscape([]byte(url), true))})
		b.push(mdToken{typ: "text", content: string(n.Label(b.src))})
		b.push(mdToken{typ: "link_close", tag: "a", nesting: -1})
	case *gast.Image:
		b.push(mdToken{typ: "image", tag: "img", src: string(util.URLEscape(n.Destination, true)), content: b.imageLabel(n)})
	case *gast.RawHTML:
		var buf strings.Builder
		for i := 0; i < n.Segments.Len(); i++ {
			seg := n.Segments.At(i)
			buf.Write(seg.Value(b.src))
		}
		b.push(mdToken{typ: "html_inline", content: buf.String()})
	default:
		b.inlineChildren(n)
	}
}

func (b *mdBuilder) inlineChildren(n gast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		b.inlineNode(c)
	}
}

func mdParse(src string) []mdToken {
	md := goldmark.New(goldmark.WithExtensions(extension.Table),
		goldmark.WithParserOptions(parser.WithInlineParsers(util.Prioritized(mdStrikeParser{}, 500))))
	b := &mdBuilder{src: []byte(src)}
	b.block(md.Parser().Parse(text.NewReader(b.src)))
	return b.toks
}

type mdElem struct {
	kind, tag, justify, lexer string
	style                     Style
	text                      styledText
	elements                  []*mdElem
	ordered                   bool
	start                     int
	header                    *mdElem
	src, link                 string
}

type mdContext struct {
	styles []Style
	stack  []*mdElem
}

func (c *mdContext) current() Style { return c.styles[len(c.styles)-1] }

func (c *mdContext) enter(st Style) Style {
	c.styles = append(c.styles, c.current().Add(st))
	return c.current()
}

func (c *mdContext) leave() { c.styles = c.styles[:len(c.styles)-1] }

func (c *mdContext) top() *mdElem { return c.stack[len(c.stack)-1] }

func newMdElem(t mdToken) *mdElem {
	switch t.typ {
	case "paragraph_open":
		return &mdElem{kind: "paragraph", justify: "left"}
	case "heading_open":
		return &mdElem{kind: "heading", tag: t.tag}
	case "fence", "code_block":
		lexer, _, _ := strings.Cut(t.info, " ")
		if lexer == "" {
			lexer = "text"
		}
		return &mdElem{kind: "code", lexer: lexer}
	case "blockquote_open":
		return &mdElem{kind: "quote"}
	case "hr":
		return &mdElem{kind: "hr"}
	case "bullet_list_open":
		return &mdElem{kind: "list", start: 1}
	case "ordered_list_open":
		return &mdElem{kind: "list", ordered: true, start: t.start}
	case "list_item_open":
		return &mdElem{kind: "item"}
	case "image":
		return &mdElem{kind: "image", src: t.src}
	case "table_open":
		return &mdElem{kind: "table"}
	case "thead_open", "tbody_open", "tr_open":
		return &mdElem{kind: strings.TrimSuffix(t.typ, "_open")}
	case "td_open", "th_open":
		justify := t.align
		if justify == "" {
			justify = "default"
		}
		return &mdElem{kind: "td", justify: justify}
	}
	return &mdElem{kind: "unknown"}
}

var textElements = map[string]string{"paragraph": "markdown.paragraph", "code": "markdown.code_block", "quote": "markdown.block_quote", "item": "markdown.item", "image": "none"}

func (e *mdElem) onEnter(c *mdContext) {
	switch {
	case e.kind == "heading":
		e.text = nil
		c.enter(markdownTheme["markdown."+e.tag])
	case e.kind == "image":
		e.link = c.current().Link
		e.text = nil
		e.style = c.enter(Style{})
	case textElements[e.kind] != "":
		e.style = c.enter(markdownTheme[textElements[e.kind]])
		e.text = nil
	}
}

func (e *mdElem) onText(c *mdContext, s string) {
	if e.kind == "heading" || e.kind == "td" || textElements[e.kind] != "" {
		e.text = e.text.add(s, c.current())
	}
}

func (e *mdElem) onLeave(c *mdContext) {
	if e.kind == "heading" || textElements[e.kind] != "" {
		c.leave()
	}
}

func (e *mdElem) newLine() bool { return e.kind != "hr" && e.kind != "image" }

func (e *mdElem) onChildClose(child *mdElem) bool {
	switch e.kind {
	case "quote", "item", "list", "thead", "tbody", "tr":
		e.elements = append(e.elements, child)
		return false
	case "table":
		if child.kind == "thead" {
			e.header = child
		} else {
			e.elements = append(e.elements, child)
		}
		return false
	}
	return true
}

// Lines of child renderables cropped and padded to width with the base style applied, as rich render_lines
func renderAll(elems []*mdElem, width int, base Style) []styledText {
	var out []styledText
	for _, el := range elems {
		lines, _ := el.render(width)
		for _, l := range lines {
			line := styledText{}
			for _, s := range l.crop(width) {
				line = line.add(s.text, base.Add(s.style))
			}
			out = append(out, line.padTo(width, base))
		}
	}
	return out
}

func prefixed(lines []styledText, first, rest span) []styledText {
	out := make([]styledText, len(lines))
	for i, l := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		out[i] = styledText{}.add(p.text, p.style).join(l)
	}
	return out
}

// Lines of one element at width and whether its last line stays open for the next renderable
func (e *mdElem) render(width int) ([]styledText, bool) {
	switch e.kind {
	case "paragraph":
		return e.text.wrap(width, e.justify, Style{}), false
	case "heading":
		justify := "left"
		if e.tag == "h1" {
			justify = "center"
		}
		return e.text.wrap(width, justify, Style{}), false
	case "code":
		return syntaxLines(strings.TrimRightFunc(e.text.plain(), unicode.IsSpace), e.lexer, width), false
	case "quote":
		pad := span{"▌ ", e.style}
		return prefixed(renderAll(e.elements, width-4, e.style), pad, pad), false
	case "hr":
		rule := styledText{}.add(strings.Repeat("-", width+1), markdownTheme["markdown.hr"]).crop(width)
		return []styledText{rule, nil}, false
	case "list":
		var out []styledText
		bullet := markdownTheme["markdown.item.bullet"]
		number := markdownTheme["markdown.item.number"]
		last := e.start + len(e.elements)
		for i, item := range e.elements {
			if !e.ordered {
				out = append(out, prefixed(renderAll(item.elements, width-3, item.style), span{" • ", bullet}, span{"   ", bullet})...)
				continue
			}
			nw := len(strconv.Itoa(last)) + 2
			numeral := fmt.Sprintf("%*d ", nw-1, e.start+i)
			out = append(out, prefixed(renderAll(item.elements, width-nw, item.style), span{numeral, number}, span{strings.Repeat(" ", nw), number})...)
		}
		return out, false
	case "image":
		title := e.text
		if title.plain() == "" {
			dest := strings.Trim(e.src, "/")
			title = styledText{}.add(dest[strings.LastIndex(dest, "/")+1:], Style{})
		}
		link := e.link
		if link == "" {
			link = e.src
		}
		linked := styledText{}
		for _, s := range title {
			st := s.style
			st.Link = link
			linked = linked.add(s.text, st)
		}
		return []styledText{styledText{}.add("🌆 ", Style{}).join(linked).add(" ", Style{})}, true
	case "table":
		return e.renderTable(width), false
	}
	return nil, false
}

// Table drawn with the simple box, no edge padding and collapsed padding as rich.markdown builds it
func (e *mdElem) renderTable(maxWidth int) []styledText {
	var rows [][]styledText
	var justify []string
	if e.header != nil && len(e.header.elements) > 0 {
		var cells []styledText
		for _, c := range e.header.elements[0].elements {
			h := styledText{}
			for _, s := range c.text {
				h = h.add(s.text, markdownTheme["table.header"].Add(s.style).Add(markdownTheme["markdown.table.header"]))
			}
			cells = append(cells, h)
			justify = append(justify, c.justify)
		}
		rows = append(rows, cells)
	}
	for _, body := range e.elements {
		for _, r := range body.elements {
			var cells []styledText
			for _, c := range r.elements {
				cells = append(cells, c.text)
			}
			rows = append(rows, cells)
		}
	}
	ncol := len(justify)
	if ncol == 0 {
		return nil
	}
	padRight := func(i int) int {
		if i == ncol-1 {
			return 0
		}
		return 1
	}
	extra := 2 + ncol - 1
	limit := maxWidth - extra
	measure := func(i, lim int) measurement {
		m := measurement{0, 0}
		for _, r := range rows {
			var cm measurement
			if i < len(r) {
				cm = measureMarkup(Escape(r[i].plain()))
			}
			cm.min, cm.max = cm.min+padRight(i), cm.max+padRight(i)
			m.min, m.max = max(m.min, cm.min), max(m.max, cm.max)
		}
		return measurement{min(m.min, lim), min(m.max, lim)}
	}
	widths := make([]int, ncol)
	total := 0
	for i := range widths {
		widths[i] = max(1, measure(i, limit).max)
		total += widths[i]
	}
	if total > limit {
		wrapable := make([]bool, ncol)
		for i := range wrapable {
			wrapable[i] = true
		}
		widths = collapseWidths(widths, wrapable, limit)
		total = 0
		for _, w := range widths {
			total += w
		}
		if total > limit {
			ones := make([]int, ncol)
			for i := range ones {
				ones[i] = 1
			}
			widths = ratioReduce(total-limit, ones, widths, widths)
		}
		for i := range widths {
			widths[i] = measure(i, widths[i]).max
		}
	}
	border := markdownTheme["markdown.table.border"]
	tableWidth := extra
	for _, w := range widths {
		tableWidth += w
	}
	edge := styledText{}.add(strings.Repeat(" ", tableWidth), border)
	out := []styledText{edge}
	for ri, r := range rows {
		cellLines := make([][]styledText, ncol)
		height := 1
		for i := range ncol {
			cell := styledText{}
			if i < len(r) {
				cell = r[i]
			}
			base := Style{}
			if ri == 0 && e.header != nil {
				base = markdownTheme["table.header"]
			}
			cw := widths[i] - padRight(i)
			j := justify[i]
			if j == "default" {
				j = "left"
			}
			lines := cell.wrap(cw, j, base)
			for k := range lines {
				lines[k] = lines[k].padTo(cw, base).add(strings.Repeat(" ", padRight(i)), base)
			}
			cellLines[i] = lines
			height = max(height, len(lines))
		}
		for h := range height {
			line := styledText{}.add(" ", border)
			for i := range ncol {
				if i > 0 {
					line = line.add(" ", border)
				}
				if h < len(cellLines[i]) {
					line = line.join(cellLines[i][h])
				} else {
					line = line.add(strings.Repeat(" ", widths[i]), Style{})
				}
			}
			out = append(out, line.add(" ", border))
		}
		if ri == 0 && e.header != nil {
			parts := make([]string, ncol)
			for i, w := range widths {
				parts[i] = strings.Repeat("─", w)
			}
			out = append(out, styledText{}.add(" "+strings.Join(parts, "─")+" ", border))
		}
	}
	return append(out, edge)
}

// Pygments lexer for an alias, matched case insensitively against chroma lexer aliases, nil when unknown
func pygmentsLexer(name string) chroma.Lexer {
	name = strings.ToLower(name)
	for _, l := range lexers.GlobalLexerRegistry.Lexers {
		for _, a := range l.Config().Aliases {
			if strings.ToLower(a) == name {
				return l
			}
		}
	}
	return nil
}

const monokaiBackground = "#272822"

var monokaiDefs = map[chroma.TokenType]string{
	chroma.Error: "#ed007e bg:#1e0010", chroma.Comment: "#959077", chroma.Keyword: "#66d9ef", chroma.KeywordNamespace: "#ff4689",
	chroma.Operator: "#ff4689", chroma.Punctuation: "#f8f8f2", chroma.Name: "#f8f8f2", chroma.NameAttribute: "#a6e22e",
	chroma.NameBuiltin: "#a6e22e", chroma.NameClass: "#a6e22e", chroma.NameConstant: "#66d9ef", chroma.NameDecorator: "#a6e22e",
	chroma.NameException: "#a6e22e", chroma.NameFunction: "#a6e22e", chroma.NameOther: "#a6e22e", chroma.NameTag: "#ff4689",
	chroma.LiteralNumber: "#ae81ff", chroma.Literal: "#ae81ff", chroma.LiteralDate: "#e6db74", chroma.LiteralString: "#e6db74",
	chroma.LiteralStringEscape: "#ae81ff", chroma.GenericDeleted: "#ff4689", chroma.GenericEmph: "italic", chroma.GenericError: "#ff4689",
	chroma.GenericInserted: "#a6e22e", chroma.GenericOutput: "#66d9ef", chroma.GenericPrompt: "bold #ff4689", chroma.GenericStrong: "bold",
	chroma.GenericSubheading: "#959077", chroma.GenericTraceback: "#66d9ef",
}

// Rich style of a token under the pygments monokai theme, each level of the token hierarchy overriding its parent
func monokaiStyle(tt chroma.TokenType) Style {
	color, bgcolor := "f8f8f2", ""
	var bold, italic, underline bool
	for _, level := range []chroma.TokenType{tt.Category(), tt.SubCategory(), tt} {
		def, ok := monokaiDefs[level]
		if !ok {
			continue
		}
		for _, f := range strings.Fields(def) {
			switch {
			case f == "noinherit":
				color, bgcolor, bold, italic, underline = "f8f8f2", "", false, false, false
			case f == "bold", f == "nobold":
				bold = f == "bold"
			case f == "italic", f == "noitalic":
				italic = f == "italic"
			case f == "underline", f == "nounderline":
				underline = f == "underline"
			case strings.HasPrefix(f, "bg:#"):
				bgcolor = f[4:]
			case strings.HasPrefix(f, "#"):
				color = f[1:]
			}
		}
	}
	flag := func(on bool) int8 {
		if on {
			return 1
		}
		return -1
	}
	st := Style{Color: "#" + color, Bgcolor: monokaiBackground, Bold: flag(bold), Italic: flag(italic), Underline: flag(underline)}
	if bgcolor != "" {
		st.Bgcolor = "#" + bgcolor
	}
	return st
}

// Lines of a monokai Syntax block with word wrap and one cell of padding on every side
func syntaxLines(code, lexerName string, width int) []styledText {
	bg := Style{Bgcolor: monokaiBackground}
	endsNL := strings.HasSuffix(code, "\n")
	processed := code
	if !endsNL {
		processed += "\n"
	}
	processed = string(styledText{}.add(processed, Style{}).expandTabsPerLine(4))
	highlighted := styledText{}
	if lexer := pygmentsLexer(lexerName); lexer == nil {
		highlighted = highlighted.add(processed, bg)
	} else if it, err := lexer.Tokenise(nil, processed); err != nil {
		highlighted = highlighted.add(processed, bg)
	} else {
		for tok := it(); tok != chroma.EOF; tok = it() {
			highlighted = highlighted.add(tok.Value, monokaiStyle(tok.Type))
		}
	}
	plain := highlighted.plain()
	if !endsNL && strings.HasSuffix(plain, "\n") {
		highlighted = highlighted.slice(0, utf8.RuneCountInString(plain)-1)
	}
	codeWidth := width - 2
	blank := styledText{}.add(strings.Repeat(" ", max(0, width)), bg)
	out := []styledText{blank}
	pad := span{" ", bg}
	start := 0
	runes := []rune(highlighted.plain())
	for i := 0; i <= len(runes); i++ {
		if i < len(runes) && runes[i] != '\n' {
			continue
		}
		for _, w := range highlighted.slice(start, i).wrap(codeWidth, "left", bg) {
			out = append(out, styledText{}.add(pad.text, pad.style).join(w.padTo(codeWidth, bg)).add(pad.text, pad.style))
		}
		start = i + 1
	}
	return append(out, blank)
}

// String with tabs expanded per line to multiples of size, as Python str.expandtabs
func (t styledText) expandTabsPerLine(size int) string {
	var b strings.Builder
	col := 0
	for _, r := range t.plain() {
		switch r {
		case '\t':
			n := size - col%size
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case '\n', '\r':
			b.WriteRune(r)
			col = 0
		default:
			b.WriteRune(r)
			col++
		}
	}
	return b.String()
}

// Lines the markdown renders at width as rich prints it, image lines running into the next renderable
func (m *Markdown) lines(width int) []styledText {
	c := &mdContext{styles: []Style{{}}}
	var out []styledText
	open, newLine := false, false
	emit := func(lines []styledText, keepOpen bool) {
		for i, l := range lines {
			if i == 0 && open && len(out) > 0 {
				out[len(out)-1] = out[len(out)-1].join(l)
				continue
			}
			out = append(out, l)
		}
		if len(lines) > 0 {
			open = keepOpen
		}
	}
	lineBreak := func() {
		if open {
			open = false
			return
		}
		out = append(out, nil)
	}
	for _, t := range mdParse(m.Source) {
		switch {
		case t.typ == "text":
			c.top().onText(c, t.content)
		case t.typ == "hardbreak":
			c.top().onText(c, "\n")
		case t.typ == "softbreak":
			c.top().onText(c, " ")
		case t.typ == "link_open":
			st := markdownTheme["markdown.link_url"]
			st.Link = t.href
			c.enter(st)
		case t.typ == "link_close":
			c.leave()
		case t.typ == "html_inline":
			if t.content == "<kbd>" {
				c.enter(markdownTheme["markdown.kbd"])
			} else if t.content == "</kbd>" {
				c.leave()
			}
		case t.tag == "em" || t.tag == "strong" || t.tag == "s" || (t.tag == "code" && t.typ != "fence" && t.typ != "code_block"):
			switch t.nesting {
			case 1:
				c.enter(markdownTheme["markdown."+t.tag])
			case -1:
				c.leave()
			default:
				c.enter(markdownTheme["markdown."+t.tag])
				if t.content != "" {
					c.top().onText(c, t.content)
				}
				c.leave()
			}
		default:
			el := newMdElem(t)
			if t.nesting >= 0 {
				c.stack = append(c.stack, el)
				el.onEnter(c)
			}
			if t.nesting == -1 {
				el = c.top()
				c.stack = c.stack[:len(c.stack)-1]
				if len(c.stack) == 0 || c.top().onChildClose(el) {
					if newLine {
						lineBreak()
					}
					emit(el.render(width))
				}
			} else if t.nesting == 0 {
				c.stack = c.stack[:len(c.stack)-1]
				el.onText(c, t.content)
				if len(c.stack) == 0 || c.top().onChildClose(el) {
					if newLine && t.typ != "inline" {
						lineBreak()
					}
					emit(el.render(width))
				}
			}
			if t.nesting <= 0 {
				el.onLeave(c)
				newLine = el.newLine()
			}
		}
	}
	return out
}

// Rendered lines at width, each cropped to width as a console renders them
func (m *Markdown) RenderLines(color bool, system string, width int) []string {
	var out []string
	for _, l := range m.lines(width) {
		out = append(out, l.crop(width).render(color, system))
	}
	return out
}

// Markdown measures as wide as the space it is given, having no measure of its own
func (m *Markdown) MeasureMax(width int) int { return width }
