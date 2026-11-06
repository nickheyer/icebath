package tools

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Parent and sibling names whose blank text the lxml HTML parser keeps, libxml2 allowPCData
var lxmlAllowPCData = setOf("a", "abbr", "acronym", "address", "applet", "b", "bdo", "big", "blockquote", "body", "button", "caption", "center", "cite", "code", "dd", "del", "dfn", "div", "dt", "em", "font", "form", "h1", "h2", "h3", "h4", "h5", "h6", "i", "iframe", "ins", "kbd", "label", "legend", "li", "map", "menu", "object", "ol", "p", "pre", "q", "s", "samp", "small", "span", "strike", "strong", "td", "th", "tt", "u", "ul", "var")

// lxml.html.defs.block_tags, the tags that make a fragment body a div rather than a span
var lxmlBlockTags = setOf("address", "blockquote", "center", "dir", "div", "dl", "fieldset", "form", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "isindex", "menu", "noframes", "noscript", "ol", "p", "pre", "table", "ul")

var lxmlFullHTMLRe = regexp.MustCompile(`(?i)^[\t\n\v\f\r \x{1c}-\x{1f}\x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}]*<(?:html|!doctype)`)

// lxmlTree is a parsed page with the document order every XPath result is sorted by
type lxmlTree struct {
	doc   *html.Node
	root  *html.Node
	order map[*html.Node]int
}

// Text made only of the four characters libxml2 counts as blank
func lxmlBlank(s string) bool {
	return strings.Trim(s, " \t\n\r") == ""
}

// Drops comments and the blank text the parser options remove_blank_text and remove_comments discard
func lxmlStripBlanks(doc *html.Node) {
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			switch {
			case c.Type == html.CommentNode:
				n.RemoveChild(c)
			case c.Type == html.TextNode && lxmlBlankRemoved(n, c):
				n.RemoveChild(c)
			default:
				walk(c)
			}
			c = next
		}
	}
	walk(doc)
}

// Applies the libxml2 areBlanks rule to a blank text node under its parent
func lxmlBlankRemoved(parent, text *html.Node) bool {
	if !lxmlBlank(text.Data) {
		return false
	}
	if parent.Type != html.ElementNode {
		return true
	}
	if parent.Data == "html" || parent.Data == "head" {
		return true
	}
	if parent.Data == "body" && lxmlHTML4Doctype(parent) {
		return true
	}
	prev := text.PrevSibling
	for prev != nil && prev.Type == html.CommentNode {
		prev = prev.PrevSibling
	}
	if prev == nil {
		return !lxmlAllowPCData[parent.Data]
	}
	if prev.Type == html.TextNode {
		return false
	}
	return !(prev.Type == html.ElementNode && lxmlAllowPCData[prev.Data])
}

// Reports an HTML 4.01 or HTML 4 strict public identifier on the document type
func lxmlHTML4Doctype(n *html.Node) bool {
	for n.Parent != nil {
		n = n.Parent
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.DoctypeNode {
			continue
		}
		for _, a := range c.Attr {
			if a.Key == "public" {
				id := strings.ToLower(a.Val)
				return id == "-//w3c//dtd html 4.01//en" || id == "-//w3c//dtd html 4//en"
			}
		}
	}
	return false
}

// Element children of a node, the len and indexing lxml gives an element
func lxmlElementChildren(n *html.Node) []*html.Node {
	var out []*html.Node
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			out = append(out, c)
		}
	}
	return out
}

// Text before the first child element, the lxml text attribute
func lxmlText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil && c.Type != html.ElementNode; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

// Text after an element up to its next element sibling, the lxml tail attribute
func lxmlTail(n *html.Node) string {
	var b strings.Builder
	for c := n.NextSibling; c != nil && c.Type != html.ElementNode; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}

// Parses page text like lxml.html.fromstring with the ddgs parser options, fragments wrapped the lxml way
func lxmlFromString(text string) (*lxmlTree, error) {
	if pystr.Strip(text) == "" {
		return nil, verbatim("ParserError: Document is empty")
	}
	doc, err := html.Parse(strings.NewReader(text))
	if err != nil {
		return nil, err
	}
	lxmlStripBlanks(doc)
	t := &lxmlTree{doc: doc, order: map[*html.Node]int{}}
	var number func(n *html.Node)
	number = func(n *html.Node) {
		t.order[n] = len(t.order)
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			number(c)
		}
	}
	number(doc)
	var htmlEl, body *html.Node
	for c := doc.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode {
			htmlEl = c
		}
	}
	t.root = htmlEl
	if lxmlFullHTMLRe.MatchString(text) || htmlEl == nil {
		return t, nil
	}
	for _, c := range lxmlElementChildren(htmlEl) {
		if c.Data == "body" {
			body = c
			break
		}
	}
	if body == nil {
		return t, nil
	}
	kids := lxmlElementChildren(body)
	if len(kids) == 1 && pystr.Strip(lxmlText(body)) == "" && pystr.Strip(lxmlTail(kids[0])) == "" {
		t.root = kids[0]
		return t, nil
	}
	body.Data = "span"
	var block func(n *html.Node) bool
	block = func(n *html.Node) bool {
		if n.Type == html.ElementNode && lxmlBlockTags[n.Data] {
			return true
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if block(c) {
				return true
			}
		}
		return false
	}
	if block(body) {
		body.Data = "div"
	}
	t.root = body
	return t, nil
}

// xpNode is an XPath node, an attribute when attr is not negative
type xpNode struct {
	n    *html.Node
	attr int
}

// String value of a node, the concatenated descendant text for an element
func (x xpNode) str() string {
	if x.attr >= 0 {
		return x.n.Attr[x.attr].Val
	}
	if x.n.Type == html.TextNode {
		return x.n.Data
	}
	var b strings.Builder
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.TextNode {
				b.WriteString(c.Data)
			} else {
				walk(c)
			}
		}
	}
	walk(x.n)
	return b.String()
}

// xpExpr is a compiled XPath 1.0 expression node
type xpExpr interface {
	eval(t *lxmlTree, ctx xpContext) (any, error)
}

// xpContext is the context node with its proximity position and size
type xpContext struct {
	node      xpNode
	pos, size int
}

type xpStep struct {
	axis  string
	test  string
	preds []xpExpr
}

type xpPath struct {
	absolute bool
	filter   xpExpr
	steps    []xpStep
}

type xpBinary struct {
	op   string
	l, r xpExpr
}

type xpLiteral struct{ v any }

type xpCall struct {
	name string
	args []xpExpr
}

type xpFiltered struct {
	primary xpExpr
	preds   []xpExpr
}

// xpParser reads an XPath 1.0 expression into an evaluable tree
type xpParser struct {
	toks []string
	i    int
}

var xpTokenRe = regexp.MustCompile(`\s*(\.\.|::|//|!=|<=|>=|"[^"]*"|'[^']*'|[0-9]+(?:\.[0-9]*)?|\.[0-9]+|[A-Za-z_][\w.-]*|\S)`)

// Compiles an XPath expression, the XPathEvalError text of lxml for a malformed one
func compileXPath(src string) (xpExpr, error) {
	p := &xpParser{}
	for rest := src; strings.TrimSpace(rest) != ""; {
		m := xpTokenRe.FindStringSubmatchIndex(rest)
		if m == nil {
			return nil, fmt.Errorf("XPathEvalError: Invalid expression")
		}
		p.toks = append(p.toks, rest[m[2]:m[3]])
		rest = rest[m[1]:]
	}
	e, err := p.or()
	if err == nil && p.i < len(p.toks) {
		err = fmt.Errorf("XPathEvalError: Invalid expression")
	}
	return e, err
}

func (p *xpParser) peek() string {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return ""
}

func (p *xpParser) next() string {
	t := p.peek()
	p.i++
	return t
}

func (p *xpParser) or() (xpExpr, error) {
	return p.binary([]string{"or"}, p.and)
}

func (p *xpParser) and() (xpExpr, error) {
	return p.binary([]string{"and"}, p.equality)
}

func (p *xpParser) equality() (xpExpr, error) {
	return p.binary([]string{"=", "!="}, p.relational)
}

func (p *xpParser) relational() (xpExpr, error) {
	return p.binary([]string{"<", ">", "<=", ">="}, p.union)
}

// Left associative chain of one precedence level
func (p *xpParser) binary(ops []string, sub func() (xpExpr, error)) (xpExpr, error) {
	l, err := sub()
	if err != nil {
		return nil, err
	}
	for slices.Contains(ops, p.peek()) {
		op := p.next()
		r, err := sub()
		if err != nil {
			return nil, err
		}
		l = &xpBinary{op: op, l: l, r: r}
	}
	return l, nil
}

func (p *xpParser) union() (xpExpr, error) {
	return p.binary([]string{"|"}, p.path)
}

// Location path or filter expression with its trailing steps
func (p *xpParser) path() (xpExpr, error) {
	t := p.peek()
	path := &xpPath{}
	switch {
	case t == "(" || strings.HasPrefix(t, `"`) || strings.HasPrefix(t, "'") || (t != "" && (t[0] >= '0' && t[0] <= '9' || (t[0] == '.' && len(t) > 1 && t != ".."))):
		prim, err := p.primary()
		if err != nil {
			return nil, err
		}
		path.filter = prim
	case p.i+1 < len(p.toks) && p.toks[p.i+1] == "(" && !slices.Contains([]string{"text", "node", "comment", "processing-instruction"}, t) && t != "" && (t[0] == '_' || t[0] >= 'A' && t[0] <= 'z'):
		prim, err := p.primary()
		if err != nil {
			return nil, err
		}
		path.filter = prim
	case t == "/" || t == "//":
		path.absolute = true
		p.next()
		if t == "//" {
			path.steps = append(path.steps, xpStep{axis: "descendant-or-self", test: "node()"})
		} else if !p.stepStarts() {
			return path, nil
		}
	}
	if path.filter == nil || p.peek() == "/" || p.peek() == "//" {
		if path.filter != nil {
			sep := p.next()
			if sep == "//" {
				path.steps = append(path.steps, xpStep{axis: "descendant-or-self", test: "node()"})
			}
		}
		for {
			step, err := p.step()
			if err != nil {
				return nil, err
			}
			path.steps = append(path.steps, step)
			if p.peek() == "/" {
				p.next()
			} else if p.peek() == "//" {
				p.next()
				path.steps = append(path.steps, xpStep{axis: "descendant-or-self", test: "node()"})
			} else {
				break
			}
		}
	}
	if path.filter != nil && len(path.steps) == 0 {
		return path.filter, nil
	}
	return path, nil
}

// Reports whether the next token can begin a location step
func (p *xpParser) stepStarts() bool {
	t := p.peek()
	return t == "." || t == ".." || t == "@" || t == "*" || (t != "" && (t[0] == '_' || (t[0]|0x20) >= 'a' && (t[0]|0x20) <= 'z'))
}

func (p *xpParser) step() (xpStep, error) {
	t := p.next()
	switch t {
	case ".":
		return xpStep{axis: "self", test: "node()"}, nil
	case "..":
		return xpStep{axis: "parent", test: "node()"}, nil
	}
	step := xpStep{axis: "child"}
	if t == "@" {
		step.axis = "attribute"
		t = p.next()
	} else if p.peek() == "::" {
		step.axis = t
		p.next()
		t = p.next()
	}
	if t == "" || t == "]" || t == ")" {
		return step, fmt.Errorf("XPathEvalError: Invalid expression")
	}
	if p.peek() == "(" && slices.Contains([]string{"text", "node", "comment", "processing-instruction"}, t) {
		p.next()
		if p.next() != ")" {
			return step, fmt.Errorf("XPathEvalError: Invalid expression")
		}
		t += "()"
	}
	step.test = t
	for p.peek() == "[" {
		p.next()
		e, err := p.or()
		if err != nil {
			return step, err
		}
		if p.next() != "]" {
			return step, fmt.Errorf("XPathEvalError: Invalid predicate")
		}
		step.preds = append(step.preds, e)
	}
	return step, nil
}

func (p *xpParser) primary() (xpExpr, error) {
	t := p.next()
	var prim xpExpr
	switch {
	case t == "(":
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, fmt.Errorf("XPathEvalError: Invalid expression")
		}
		prim = e
	case strings.HasPrefix(t, `"`) || strings.HasPrefix(t, "'"):
		prim = &xpLiteral{v: t[1 : len(t)-1]}
	case t != "" && (t[0] >= '0' && t[0] <= '9' || t[0] == '.'):
		f, _ := strconv.ParseFloat(t, 64)
		prim = &xpLiteral{v: f}
	default:
		call := &xpCall{name: t}
		p.next()
		for p.peek() != ")" {
			a, err := p.or()
			if err != nil {
				return nil, err
			}
			call.args = append(call.args, a)
			if p.peek() == "," {
				p.next()
			} else if p.peek() != ")" {
				return nil, fmt.Errorf("XPathEvalError: Invalid expression")
			}
		}
		p.next()
		prim = call
	}
	var preds []xpExpr
	for p.peek() == "[" {
		p.next()
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next() != "]" {
			return nil, fmt.Errorf("XPathEvalError: Invalid predicate")
		}
		preds = append(preds, e)
	}
	if len(preds) > 0 {
		return &xpFiltered{primary: prim, preds: preds}, nil
	}
	return prim, nil
}

func (l *xpLiteral) eval(*lxmlTree, xpContext) (any, error) { return l.v, nil }

// Sorts and deduplicates a node set into document order
func (t *lxmlTree) docOrder(nodes []xpNode) []xpNode {
	seen := map[xpNode]bool{}
	out := nodes[:0:0]
	for _, n := range nodes {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := t.order[out[i].n], t.order[out[j].n]
		if a != b {
			return a < b
		}
		return out[i].attr < out[j].attr
	})
	return out
}

// Nodes along an axis from one context node in axis order
func axisNodes(x xpNode, axis string) ([]xpNode, error) {
	if x.attr >= 0 {
		switch axis {
		case "self", "descendant-or-self":
			return []xpNode{x}, nil
		case "parent", "ancestor":
			return []xpNode{{n: x.n, attr: -1}}, nil
		}
		return nil, nil
	}
	var out []xpNode
	var desc func(n *html.Node)
	desc = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			out = append(out, xpNode{n: c, attr: -1})
			desc(c)
		}
	}
	switch axis {
	case "child":
		for c := x.n.FirstChild; c != nil; c = c.NextSibling {
			out = append(out, xpNode{n: c, attr: -1})
		}
	case "descendant":
		desc(x.n)
	case "descendant-or-self":
		out = append(out, x)
		desc(x.n)
	case "self":
		out = append(out, x)
	case "parent":
		if x.n.Parent != nil {
			out = append(out, xpNode{n: x.n.Parent, attr: -1})
		}
	case "attribute":
		if x.n.Type == html.ElementNode {
			for i := range x.n.Attr {
				out = append(out, xpNode{n: x.n, attr: i})
			}
		}
	default:
		return nil, fmt.Errorf("XPathEvalError: Unsupported axis %s", axis)
	}
	return out, nil
}

// Reports whether a node passes a node test on an axis
func nodeTestMatch(x xpNode, axis, test string) bool {
	if axis == "attribute" {
		return x.attr >= 0 && (test == "*" || test == "node()" || x.n.Attr[x.attr].Key == test)
	}
	if x.attr >= 0 {
		return test == "node()"
	}
	switch test {
	case "node()":
		return true
	case "text()":
		return x.n.Type == html.TextNode
	case "comment()":
		return x.n.Type == html.CommentNode
	case "processing-instruction()":
		return false
	case "*":
		return x.n.Type == html.ElementNode
	}
	return x.n.Type == html.ElementNode && x.n.Data == test
}

// Keeps the nodes each predicate accepts, positions counted within the current list
func (t *lxmlTree) applyPreds(nodes []xpNode, preds []xpExpr) ([]xpNode, error) {
	for _, pred := range preds {
		kept := nodes[:0:0]
		for i, n := range nodes {
			v, err := pred.eval(t, xpContext{node: n, pos: i + 1, size: len(nodes)})
			if err != nil {
				return nil, err
			}
			if f, ok := v.(float64); ok {
				if f == float64(i+1) {
					kept = append(kept, n)
				}
			} else if xpBool(v) {
				kept = append(kept, n)
			}
		}
		nodes = kept
	}
	return nodes, nil
}

func (p *xpPath) eval(t *lxmlTree, ctx xpContext) (any, error) {
	var cur []xpNode
	switch {
	case p.filter != nil:
		v, err := p.filter.eval(t, ctx)
		if err != nil {
			return nil, err
		}
		set, ok := v.([]xpNode)
		if !ok {
			return nil, fmt.Errorf("XPathEvalError: Invalid type")
		}
		cur = set
	case p.absolute:
		cur = []xpNode{{n: t.doc, attr: -1}}
	default:
		cur = []xpNode{ctx.node}
	}
	for _, s := range p.steps {
		var next []xpNode
		for _, c := range cur {
			cands, err := axisNodes(c, s.axis)
			if err != nil {
				return nil, err
			}
			matched := cands[:0:0]
			for _, n := range cands {
				if nodeTestMatch(n, s.axis, s.test) {
					matched = append(matched, n)
				}
			}
			kept, err := t.applyPreds(matched, s.preds)
			if err != nil {
				return nil, err
			}
			next = append(next, kept...)
		}
		cur = t.docOrder(next)
	}
	return cur, nil
}

func (f *xpFiltered) eval(t *lxmlTree, ctx xpContext) (any, error) {
	v, err := f.primary.eval(t, ctx)
	if err != nil {
		return nil, err
	}
	set, ok := v.([]xpNode)
	if !ok {
		return nil, fmt.Errorf("XPathEvalError: Invalid type")
	}
	return t.applyPreds(t.docOrder(set), f.preds)
}

func (b *xpBinary) eval(t *lxmlTree, ctx xpContext) (any, error) {
	l, err := b.l.eval(t, ctx)
	if err != nil {
		return nil, err
	}
	if b.op == "or" && xpBool(l) {
		return true, nil
	}
	if b.op == "and" && !xpBool(l) {
		return false, nil
	}
	r, err := b.r.eval(t, ctx)
	if err != nil {
		return nil, err
	}
	switch b.op {
	case "or", "and":
		return xpBool(r), nil
	case "|":
		ls, ok1 := l.([]xpNode)
		rs, ok2 := r.([]xpNode)
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("XPathEvalError: Invalid type")
		}
		return t.docOrder(append(append([]xpNode{}, ls...), rs...)), nil
	}
	return xpCompare(b.op, l, r), nil
}

// XPath 1.0 comparison of two values, node sets compared member by member
func xpCompare(op string, l, r any) bool {
	if ls, ok := l.([]xpNode); ok {
		for _, n := range ls {
			if xpCompare(op, n.str(), r) {
				return true
			}
		}
		return false
	}
	if rs, ok := r.([]xpNode); ok {
		for _, n := range rs {
			if xpCompare(op, l, n.str()) {
				return true
			}
		}
		return false
	}
	if op == "=" || op == "!=" {
		var eq bool
		_, lb := l.(bool)
		_, rb := r.(bool)
		_, lf := l.(float64)
		_, rf := r.(float64)
		switch {
		case lb || rb:
			eq = xpBool(l) == xpBool(r)
		case lf || rf:
			eq = xpNumber(l) == xpNumber(r)
		default:
			eq = xpString(l) == xpString(r)
		}
		return eq == (op == "=")
	}
	a, c := xpNumber(l), xpNumber(r)
	switch op {
	case "<":
		return a < c
	case ">":
		return a > c
	case "<=":
		return a <= c
	}
	return a >= c
}

// XPath boolean() of a value
func xpBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	case []xpNode:
		return len(x) > 0
	}
	return false
}

// XPath string() of a value, the first node's string value for a node set
func xpString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		switch {
		case math.IsNaN(x):
			return "NaN"
		case math.IsInf(x, 1):
			return "Infinity"
		case math.IsInf(x, -1):
			return "-Infinity"
		case x == math.Trunc(x):
			return strconv.FormatFloat(x, 'f', 0, 64)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []xpNode:
		if len(x) == 0 {
			return ""
		}
		return x[0].str()
	}
	return ""
}

var xpNumberRe = regexp.MustCompile(`^[\t\n\r ]*-?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)[\t\n\r ]*$`)

// XPath number() of a value, NaN for text that is not a number
func xpNumber(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	}
	s := xpString(v)
	if !xpNumberRe.MatchString(s) {
		return math.NaN()
	}
	f, _ := strconv.ParseFloat(strings.Trim(s, "\t\n\r "), 64)
	return f
}

func (c *xpCall) eval(t *lxmlTree, ctx xpContext) (any, error) {
	args := make([]any, len(c.args))
	for i, a := range c.args {
		v, err := a.eval(t, ctx)
		if err != nil {
			return nil, err
		}
		args[i] = v
	}
	argc := func(n int) error {
		if len(args) != n {
			return fmt.Errorf("XPathEvalError: Invalid number of arguments")
		}
		return nil
	}
	switch c.name {
	case "position":
		return float64(ctx.pos), argc(0)
	case "last":
		return float64(ctx.size), argc(0)
	case "true":
		return true, argc(0)
	case "false":
		return false, argc(0)
	case "not":
		if err := argc(1); err != nil {
			return nil, err
		}
		return !xpBool(args[0]), nil
	case "boolean":
		if err := argc(1); err != nil {
			return nil, err
		}
		return xpBool(args[0]), nil
	case "contains", "starts-with":
		if err := argc(2); err != nil {
			return nil, err
		}
		if c.name == "contains" {
			return strings.Contains(xpString(args[0]), xpString(args[1])), nil
		}
		return strings.HasPrefix(xpString(args[0]), xpString(args[1])), nil
	case "string", "normalize-space", "string-length", "number":
		if len(args) > 1 {
			return nil, fmt.Errorf("XPathEvalError: Invalid number of arguments")
		}
		v := any([]xpNode{ctx.node})
		if len(args) == 1 {
			v = args[0]
		}
		switch c.name {
		case "string":
			return xpString(v), nil
		case "number":
			return xpNumber(v), nil
		case "string-length":
			return float64(len([]rune(xpString(v)))), nil
		}
		return strings.Join(strings.FieldsFunc(xpString(v), func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }), " "), nil
	case "concat":
		if len(args) < 2 {
			return nil, fmt.Errorf("XPathEvalError: Invalid number of arguments")
		}
		var b strings.Builder
		for _, a := range args {
			b.WriteString(xpString(a))
		}
		return b.String(), nil
	case "count":
		if err := argc(1); err != nil {
			return nil, err
		}
		set, ok := args[0].([]xpNode)
		if !ok {
			return nil, fmt.Errorf("XPathEvalError: Invalid type")
		}
		return float64(len(set)), nil
	}
	return nil, fmt.Errorf("XPathEvalError: Unregistered function")
}

// Runs an XPath against a context node, the string results lxml returns for text and attribute nodes
func (t *lxmlTree) xpath(ctx *html.Node, expr string) ([]xpNode, error) {
	e, err := compileXPath(expr)
	if err != nil {
		return nil, err
	}
	v, err := e.eval(t, xpContext{node: xpNode{n: ctx, attr: -1}, pos: 1, size: 1})
	if err != nil {
		return nil, err
	}
	set, ok := v.([]xpNode)
	if !ok {
		return nil, fmt.Errorf("XPathEvalError: result is not a node set")
	}
	return set, nil
}

// Joined string results of an XPath, the "".join(item.xpath(value)) ddgs reads each field with
func (t *lxmlTree) xpathText(ctx *html.Node, expr string) (string, error) {
	set, err := t.xpath(ctx, expr)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, n := range set {
		if n.attr < 0 && n.n.Type == html.ElementNode {
			return "", fmt.Errorf("TypeError: sequence item 0: expected str instance, lxml.html.HtmlElement found")
		}
		b.WriteString(n.str())
	}
	return b.String(), nil
}
