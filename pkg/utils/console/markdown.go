package consoleutil

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	east "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Document in markdown drawn for a terminal: centered and styled headings, emphasis, inline and syntax highlighted code, quotes, lists, tables, links and images
type Markdown string

func (m Markdown) render(o layout) []segments {
	src := []byte(m)
	r := &mdRenderer{src: src, links: o.links, soft: o.noWrap}
	lines := r.document(markdownParser().Parse(text.NewReader(src)), o.width)
	if !o.noWrap {
		for i, l := range lines {
			lines[i] = l.crop(o.width)
		}
	}
	return lines
}

func (m Markdown) measure(o layout) int { return o.width }

// Styles markdown draws in
var (
	mdBold        = Style{on: attrBold}
	mdItalic      = Style{on: attrItalic}
	mdStrike      = Style{on: attrStrike}
	mdCode        = Style{fg: basicColor(6), bg: basicColor(0), on: attrBold}
	mdQuote       = Style{fg: basicColor(5)}
	mdBullet      = Style{on: attrBold}
	mdNumber      = Style{fg: basicColor(6)}
	mdRule        = Style{on: attrDim}
	mdLink        = Style{fg: basicColor(4), on: attrUnderline}
	mdKbd         = Style{fg: basicColor(11), on: attrBold}
	mdTableBorder = Style{fg: basicColor(6)}
	mdTableHead   = Style{on: attrBold}
	mdTableHeader = Style{fg: basicColor(6), off: attrBold}
	mdHeadings    = [...]Style{
		{on: attrBold | attrUnderline},
		{fg: basicColor(5), on: attrUnderline},
		{fg: basicColor(5), on: attrBold},
		{fg: basicColor(5), on: attrItalic},
		{on: attrItalic},
		{on: attrDim},
	}
)

// Markdown parser with tables and strikethrough between two or more tildes, built on first use
var markdownParser = sync.OnceValue(func() parser.Parser {
	return goldmark.New(goldmark.WithExtensions(extension.Table),
		goldmark.WithParserOptions(parser.WithInlineParsers(util.Prioritized(strikeParser{}, 500)))).Parser()
})

type strikeDelimiter struct{}

func (strikeDelimiter) IsDelimiter(b byte) bool { return b == '~' }

func (strikeDelimiter) CanOpenCloser(opener, closer *parser.Delimiter) bool {
	return opener.Char == closer.Char
}

func (strikeDelimiter) OnMatch(consumes int) ast.Node { return east.NewStrikethrough() }

// Strikethrough between runs of two or more tildes, a single tilde staying literal so approximations such as ~5 never strike text
type strikeParser struct{}

func (strikeParser) Trigger() []byte { return []byte{'~'} }

// Delimiter of a tilde run, an odd run leading with a literal tilde
func (strikeParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	before := block.PrecendingCharacter()
	line, segment := block.PeekLine()
	node := parser.ScanDelimiter(line, before, 2, strikeDelimiter{})
	if node == nil {
		return nil
	}
	total := node.OriginalLength
	if total%2 == 1 {
		parent.AppendChild(parent, ast.NewTextSegment(segment.WithStop(segment.Start+1)))
		segment = segment.WithStart(segment.Start + 1)
		node.OriginalLength--
		node.Length--
	}
	node.Segment = segment.WithStop(segment.Start + node.OriginalLength)
	block.Advance(total)
	pc.PushDelimiter(node)
	return node
}

// Walker turning a markdown tree into styled lines
type mdRenderer struct {
	src []byte
	// whether output carries hyperlinks, link addresses written after their text when it does not
	links bool
	// top level text left unwrapped for the terminal to wrap, nested blocks wrapped but not padded
	soft bool
}

// Lines of the top level blocks with a blank line between each two
func (r *mdRenderer) document(doc ast.Node, width int) []segments {
	var out []segments
	first := true
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		lines, ok := r.block(n, width, Style{}, true)
		if !ok {
			continue
		}
		if !first {
			out = append(out, nil)
		}
		out = append(out, lines...)
		first = false
	}
	return out
}

// Wrapping of a paragraph or heading: unwrapped at the top level of soft output, unpadded below it, else justified to the width
func (r *mdRenderer) textWrap(top bool, justify string) wrapOptions {
	switch {
	case r.soft && top:
		return wrapOptions{justify: justify, noWrap: true}
	case r.soft && justify == "left":
		return wrapOptions{}
	}
	return wrapOptions{justify: justify}
}

// Lines of one block at width, false for a block that draws nothing
func (r *mdRenderer) block(n ast.Node, width int, ctx Style, top bool) ([]segments, bool) {
	switch n := n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return wrap(r.inlineText(n, ctx), width, r.textWrap(top, "left")), true
	case *ast.Heading:
		justify := "left"
		if n.Level == 1 {
			justify = "center"
		}
		st := ctx.add(mdHeadings[min(max(n.Level, 1), len(mdHeadings))-1])
		return wrap(r.inlineText(n, st), width, r.textWrap(top, justify)), true
	case *ast.Blockquote:
		st := ctx.add(mdQuote)
		bar := segment{"▌ ", st}
		return prefixed(r.children(n, width-4, st, st), bar, bar), true
	case *ast.List:
		return r.list(n, width, ctx), true
	case *ast.FencedCodeBlock:
		return codeLines(r.code(n), string(n.Language(r.src)), width), true
	case *ast.CodeBlock:
		return codeLines(r.code(n), "", width), true
	case *ast.ThematicBreak:
		return []segments{{{strings.Repeat("─", max(0, width)), mdRule}}}, true
	case *ast.HTMLBlock:
		return nil, false
	case *east.Table:
		lines := r.table(n, width, ctx)
		return lines, lines != nil
	}
	if !n.HasChildren() {
		return nil, false
	}
	var out []segments
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if lines, ok := r.block(c, width, ctx, top); ok {
			out = append(out, lines...)
		}
	}
	return out, true
}

// Lines of the blocks under n one after another at width, each cut to the width with base laid under it and, unless soft, padded to the width
func (r *mdRenderer) children(n ast.Node, width int, ctx, base Style) []segments {
	var out []segments
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		lines, ok := r.block(c, width, ctx, false)
		if !ok {
			continue
		}
		for _, l := range lines {
			l = l.crop(width).over(base)
			if !r.soft {
				l = l.padTo(width, base)
			}
			out = append(out, l)
		}
	}
	return out
}

// Lines with first before the first one and rest before every other
func prefixed(lines []segments, first, rest segment) []segments {
	out := make([]segments, len(lines))
	for i, l := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		out[i] = append(segments{p}, l...)
	}
	return out
}

// Lines of a list, items under a bullet or a right aligned number
func (r *mdRenderer) list(n *ast.List, width int, ctx Style) []segments {
	var out []segments
	if !n.IsOrdered() {
		for item := n.FirstChild(); item != nil; item = item.NextSibling() {
			out = append(out, prefixed(r.children(item, width-3, ctx, ctx), segment{" • ", mdBullet}, segment{"   ", mdBullet})...)
		}
		return out
	}
	numberWidth := len(strconv.Itoa(n.Start+n.ChildCount())) + 2
	i := 0
	for item := n.FirstChild(); item != nil; item = item.NextSibling() {
		numeral := segment{fmt.Sprintf("%*d ", numberWidth-1, n.Start+i), mdNumber}
		out = append(out, prefixed(r.children(item, width-numberWidth, ctx, ctx), numeral, segment{spaces(numberWidth), mdNumber})...)
		i++
	}
	return out
}

// Source of a code block with control characters removed and trailing space trimmed
func (r *mdRenderer) code(n ast.Node) string {
	var b strings.Builder
	lines := n.Lines()
	for i := range lines.Len() {
		seg := lines.At(i)
		b.Write(seg.Value(r.src))
	}
	return strings.TrimRightFunc(stripEscapes(stripControl(b.String())), unicode.IsSpace)
}

// Lines of a table with a rule under its header, cells set one space apart and edged by blank rows
func (r *mdRenderer) table(n *east.Table, width int, ctx Style) []segments {
	var rows [][]segments
	var justify []string
	header := false
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		var row []segments
		for cell := c.FirstChild(); cell != nil; cell = cell.NextSibling() {
			row = append(row, r.inlineText(cell, ctx))
			if _, ok := c.(*east.TableHeader); ok {
				justify = append(justify, cellJustify(cell))
			}
		}
		if _, ok := c.(*east.TableHeader); ok {
			for i, t := range row {
				h := make(segments, len(t))
				for k, s := range t {
					h[k] = segment{s.text, mdTableHead.add(s.style).add(mdTableHeader)}
				}
				row[i] = h
			}
			rows = append([][]segments{row}, rows...)
			header = true
			continue
		}
		rows = append(rows, row)
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
	measure := func(i, limit int) int {
		widest := 0
		for _, row := range rows {
			var cm measurement
			if i < len(row) {
				cm = measureText(row[i])
			}
			widest = max(widest, cm.max+padRight(i))
		}
		return min(widest, limit)
	}
	limit := width - extra
	widths := make([]int, ncol)
	wrapable := make([]bool, ncol)
	for i := range widths {
		widths[i] = max(1, measure(i, limit))
		wrapable[i] = true
	}
	if sum(widths) > limit {
		widths = fitWidths(widths, wrapable, limit)
		for i := range widths {
			widths[i] = measure(i, widths[i])
		}
	}
	tableWidth := extra + sum(widths)
	edge := segments{{spaces(tableWidth), mdTableBorder}}
	out := []segments{edge}
	for ri, row := range rows {
		base := Style{}
		if ri == 0 && header {
			base = mdTableHead
		}
		cellLines := make([][]segments, ncol)
		height := 1
		for i := range ncol {
			var cell segments
			if i < len(row) {
				cell = row[i]
			}
			inner := widths[i] - padRight(i)
			lines := wrap(cell, inner, wrapOptions{justify: justify[i], pad: base})
			for k, l := range lines {
				lines[k] = l.padTo(inner, base).with(spaces(padRight(i)), base)
			}
			cellLines[i] = lines
			height = max(height, len(lines))
		}
		for h := range height {
			line := segments{{" ", mdTableBorder}}
			for i := range ncol {
				if i > 0 {
					line = line.with(" ", mdTableBorder)
				}
				if h < len(cellLines[i]) {
					line = line.join(cellLines[i][h])
				} else {
					line = line.with(spaces(widths[i]), Style{})
				}
			}
			out = append(out, line.with(" ", mdTableBorder))
		}
		if ri == 0 && header {
			parts := make([]string, ncol)
			for i, w := range widths {
				parts[i] = strings.Repeat("─", w)
			}
			out = append(out, segments{{" " + strings.Join(parts, "─") + " ", mdTableBorder}})
		}
	}
	return append(out, edge)
}

func cellJustify(cell ast.Node) string {
	if c, ok := cell.(*east.TableCell); ok {
		switch c.Alignment {
		case east.AlignRight:
			return "right"
		case east.AlignCenter:
			return "center"
		}
	}
	return "left"
}

// Styled inline content of a block, links written out when output cannot carry them
func (r *mdRenderer) inlineText(parent ast.Node, st Style) segments {
	var out segments
	r.inlines(parent, st, "", &out)
	if !r.links {
		out = inlineLinks(out)
	}
	return out
}

func (r *mdRenderer) text(out *segments, s string, st Style) {
	if s = stripEscapes(stripControl(s)); s != "" {
		*out = append(*out, segment{s, st})
	}
}

// Appends the inline nodes under parent in style st, link the address of an enclosing link
func (r *mdRenderer) inlines(parent ast.Node, st Style, link string, out *segments) {
	var kbd []Style
	cur := st
	for c := parent.FirstChild(); c != nil; c = c.NextSibling() {
		switch n := c.(type) {
		case *ast.Text:
			value := n.Value(r.src)
			if n.IsRaw() {
				r.text(out, string(value), cur)
			} else {
				r.text(out, mdDecode(value), cur)
			}
			if n.HardLineBreak() {
				r.text(out, "\n", cur)
			} else if n.SoftLineBreak() {
				r.text(out, " ", cur)
			}
		case *ast.String:
			r.text(out, string(n.Value), cur)
		case *ast.CodeSpan:
			r.text(out, strings.ReplaceAll(rawText(n, r.src), "\n", " "), cur.add(mdCode))
		case *ast.Emphasis:
			em := mdItalic
			if n.Level >= 2 {
				em = mdBold
			}
			r.inlines(n, cur.add(em), link, out)
		case *east.Strikethrough:
			r.inlines(n, cur.add(mdStrike), link, out)
		case *ast.Link:
			dest := string(util.URLEscape(n.Destination, true))
			r.inlines(n, cur.add(mdLink).withLink(dest), dest, out)
		case *ast.AutoLink:
			url := string(n.URL(r.src))
			if n.AutoLinkType == ast.AutoLinkEmail && !strings.HasPrefix(strings.ToLower(url), "mailto:") {
				url = "mailto:" + url
			}
			r.text(out, string(n.Label(r.src)), cur.add(mdLink).withLink(string(util.URLEscape([]byte(url), true))))
		case *ast.Image:
			r.image(n, cur, link, out)
		case *ast.RawHTML:
			var b strings.Builder
			for i := range n.Segments.Len() {
				seg := n.Segments.At(i)
				b.Write(seg.Value(r.src))
			}
			switch strings.ToLower(strings.TrimSpace(b.String())) {
			case "<kbd>":
				kbd = append(kbd, cur)
				cur = cur.add(mdKbd)
			case "</kbd>":
				if len(kbd) > 0 {
					cur, kbd = kbd[len(kbd)-1], kbd[:len(kbd)-1]
				}
			}
		default:
			r.inlines(n, cur, link, out)
		}
	}
}

// Appends an image as a picture mark and its alt text, linked to the enclosing link or the image, named by its file when it has no alt text
func (r *mdRenderer) image(n *ast.Image, st Style, link string, out *segments) {
	src := string(util.URLEscape(n.Destination, true))
	if link == "" {
		link = src
	}
	linked := st.withLink(link)
	var title segments
	r.inlines(n, linked, link, &title)
	if strings.TrimSpace(title.plain()) == "" {
		dest := strings.Trim(src, "/")
		title = segments{{dest[strings.LastIndex(dest, "/")+1:], linked}}
	}
	r.text(out, "🌆 ", st)
	*out = append(*out, title...)
}

// Source text of the text nodes under n, escapes left as written
func rawText(n ast.Node, src []byte) string {
	var b strings.Builder
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		switch t := c.(type) {
		case *ast.Text:
			b.Write(t.Value(src))
		case *ast.String:
			b.Write(t.Value)
		default:
			b.WriteString(rawText(c, src))
		}
	}
	return b.String()
}

// Inline text with backslash escapes and character references resolved
func mdDecode(b []byte) string {
	out := make([]byte, 0, len(b))
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
				if resolved := util.ResolveNumericReferences(util.ResolveEntityNames(ref)); !bytes.Equal(resolved, ref) {
					out = append(out, resolved...)
					i += j
					continue
				}
			}
		}
		out = append(out, c)
	}
	return string(out)
}
