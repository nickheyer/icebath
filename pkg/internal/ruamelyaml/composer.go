package ruamelyaml

import (
	"fmt"
	"runtime/debug"
)

type NodeKind int8

const (
	ScalarNode NodeKind = iota
	SequenceNode
	MappingNode
)

// Node class id the constructor messages print
func (k NodeKind) ID() string {
	switch k {
	case SequenceNode:
		return "sequence"
	case MappingNode:
		return "mapping"
	}
	return "scalar"
}

// Key and value node of one mapping entry
type Pair struct {
	Key, Value *Node
}

// Composed node shared by every alias of its anchor, mutated by merge flattening like ruamel's nodes
type Node struct {
	Kind   NodeKind
	tag    string
	rawTag string
	lazy   bool
	Value  string
	Items  []*Node
	Pairs  []Pair
	Merge  []Pair
	Merged bool
	Start  *Mark
	End    *Mark
	Style  rune
	Flow   bool
	Anchor *string
	// Handle of the explicit tag token, nil for a resolved or verbatim tag
	Handle *string
	// Old style comment list the round trip parser attached, nil when none
	Comment []any
}

// Tag text of the node, a C parser tag percent decoded on every access as ruamel's Tag does
func (n *Node) Tag() string {
	if n.lazy {
		return uriDecodedSuffix(n.rawTag)
	}
	return n.tag
}

// Replaces the tag the way assigning node.tag a str does
func (n *Node) SetTag(tag string) {
	n.tag, n.lazy = tag, false
}

// Python warning raised during a load, with the category name, message text and the site warnings.warn names
type Warning struct {
	Category string
	Message  string
	Site     WarningSite
}

// Module line, source line and filter action of a warnings.warn call
type WarningSite struct {
	Where  string
	Source string
	Once   bool
}

// Composed single document of a stream, nil Root for an empty stream, with the version the constructor resolves under
type Document struct {
	Root     *Node
	Version  version
	Warnings []Warning
	Name     string
}

// Version the constructor sees, the (1, 1) or (1, 2) tuple int and float construction compare against
func (d *Document) IsYAML11() bool { return d.Version.eq(v11) }

func (d *Document) IsYAML12() bool { return d.Version.eq(v12) }

// State of the YAML instance a pure load runs in, the version a directive or explicit document start assigns
type loader struct {
	rt       bool
	version  version
	scanner  *scanner
	anchors  map[string]*Node
	warnings []Warning
}

func (l *loader) processingVersion() version {
	if l.rt {
		return v11
	}
	if l.scanner != nil && l.scanner.yamlVersion.set {
		return l.scanner.yamlVersion
	}
	if l.version.set {
		return l.version
	}
	return v12
}

// Assigns YAML.version, whose setter refuses a minor version other than 1 or 2
func (l *loader) setVersion(v version) {
	if v.set {
		if v.minor.Int64() != 1 && v.minor.Int64() != 2 || !v.minor.IsInt64() {
			panic(Raise("AssertionError", "version minor part can only be 2 or 1, got "+v.String()))
		}
	}
	l.version = v
}

type composer struct {
	l     *loader
	p     *parser
	depth int
}

// Collection levels the recursive Python composer nests from a shallow caller before RecursionError
func pureDepthLimit(kind NodeKind, flow bool) int {
	if kind == SequenceNode && flow {
		return 493
	}
	return 491
}

func composerError(context string, contextMark *Mark, problem string, problemMark *Mark) *MarkedError {
	e := &MarkedError{Class: "ComposerError", ContextMark: contextMark, ProblemMark: problemMark, Problem: strp(problem)}
	if context != "" {
		e.Context = strp(context)
	}
	return e
}

func (c *composer) getSingleNode() *Node {
	c.p.getEvent()
	var document *Node
	if !c.p.checkEvent(evStreamEnd) {
		document = c.composeDocument()
	}
	if !c.p.checkEvent(evStreamEnd) {
		ev := c.p.getEvent()
		panic(composerError("expected a single document in the stream", document.Start, "but found another document", ev.start))
	}
	c.p.getEvent()
	return document
}

func (c *composer) composeDocument() *Node {
	c.l.anchors = map[string]*Node{}
	c.p.getEvent()
	node := c.composeNode(siteDocument)
	c.p.getEvent()
	return node
}

// Caller lines of compose_node a ReusedAnchorWarning is attributed to with stacklevel 2
var (
	siteDocument = WarningSite{Where: "ruamel/yaml/composer.py:95", Source: "node = self.compose_node(None, None)"}
	siteSequence = WarningSite{Where: "ruamel/yaml/composer.py:173", Source: "node.value.append(self.compose_node(node, index))"}
	siteKey      = WarningSite{Where: "ruamel/yaml/composer.py:207", Source: "item_key = self.compose_node(node, None)"}
	siteValue    = WarningSite{Where: "ruamel/yaml/composer.py:212", Source: "item_value = self.compose_node(node, item_key)"}
)

func (c *composer) composeNode(site WarningSite) *Node {
	if c.p.checkEvent(evAlias) {
		ev := c.p.getEvent()
		alias := *ev.anchor
		n, ok := c.l.anchors[alias]
		if !ok {
			panic(composerError("", nil, "found undefined alias "+pyStrRepr(alias), ev.start))
		}
		return n
	}
	ev := c.p.peekEvent()
	if ev.anchor != nil {
		if first, dup := c.l.anchors[*ev.anchor]; dup {
			c.l.warnings = append(c.l.warnings, Warning{Category: "ReusedAnchorWarning", Site: site,
				Message: fmt.Sprintf("\nfound duplicate anchor %s\nfirst occurrence %s\nsecond occurrence %s", pyStrRepr(*ev.anchor), first.Start, ev.start)})
		}
	}
	if ev.kind == evSequenceStart || ev.kind == evMappingStart {
		kind := SequenceNode
		if ev.kind == evMappingStart {
			kind = MappingNode
		}
		c.depth++
		defer func() { c.depth-- }()
		if c.depth > pureDepthLimit(kind, ev.flow) {
			panic(Raise("RecursionError", "maximum recursion depth exceeded"))
		}
	}
	switch {
	case c.p.checkEvent(evScalar):
		return c.composeScalarNode(ev.anchor)
	case c.p.checkEvent(evSequenceStart):
		return c.composeSequenceNode(ev.anchor)
	case c.p.checkEvent(evMappingStart):
		return c.composeMappingNode(ev.anchor)
	}
	panic(Raise("UnboundLocalError", "cannot access local variable 'node' where it is not associated with a value"))
}

func (c *composer) nodeTag(ev *event, kind NodeKind) string {
	if ev.tag == nil || ev.tag.text == "!" {
		return resolve(c.l.processingVersion(), kind, ev.value, ev.implicit)
	}
	return ev.tag.text
}

func eventHandle(ev *event) *string {
	if ev.tag == nil || ev.tag.text == "!" {
		return nil
	}
	return ev.tag.handle
}

func commentSlots(c *tokenComment) []any {
	if c == nil {
		return nil
	}
	return c.slots
}

func (c *composer) composeScalarNode(anchor *string) *Node {
	ev := c.p.getEvent()
	n := &Node{Kind: ScalarNode, tag: c.nodeTag(ev, ScalarNode), Value: ev.value, Start: ev.start, End: ev.end, Style: ev.style, Anchor: anchor,
		Handle: eventHandle(ev), Comment: commentSlots(ev.comment)}
	if anchor != nil {
		c.l.anchors[*anchor] = n
	}
	return n
}

func (c *composer) composeSequenceNode(anchor *string) *Node {
	start := c.p.getEvent()
	n := &Node{Kind: SequenceNode, tag: c.nodeTag(start, SequenceNode), Start: start.start, Flow: start.flow, Anchor: anchor,
		Handle: eventHandle(start), Comment: commentSlots(start.comment)}
	if anchor != nil {
		c.l.anchors[*anchor] = n
	}
	for !c.p.checkEvent(evSequenceEnd) {
		n.Items = append(n.Items, c.composeNode(siteSequence))
	}
	end := c.p.getEvent()
	if n.Flow && end.comment != nil {
		n.Comment = end.comment.slots
	}
	n.End = end.end
	c.checkEndDocComment(end, n)
	return n
}

// Moves pre comments of a collection end event onto the node as its third comment slot
func (c *composer) checkEndDocComment(end *event, n *Node) {
	if end.comment != nil && len(end.comment.slots) > 1 && truthy(end.comment.slots[1]) {
		if n.Comment == nil {
			n.Comment = []any{nil, nil}
		}
		n.Comment = append(n.Comment, end.comment.slots[1])
		end.comment.slots[1] = nil
	}
}

func (c *composer) composeMappingNode(anchor *string) *Node {
	start := c.p.getEvent()
	n := &Node{Kind: MappingNode, tag: c.nodeTag(start, MappingNode), Start: start.start, Flow: start.flow, Anchor: anchor,
		Handle: eventHandle(start), Comment: commentSlots(start.comment)}
	if anchor != nil {
		c.l.anchors[*anchor] = n
	}
	for !c.p.checkEvent(evMappingEnd) {
		key := c.composeNode(siteKey)
		value := c.composeNode(siteValue)
		n.Pairs = append(n.Pairs, Pair{key, value})
	}
	end := c.p.getEvent()
	if n.Flow && end.comment != nil {
		n.Comment = end.comment.slots
	}
	n.End = end.end
	c.checkEndDocComment(end, n)
	return n
}

// Turns a Python exception raised as a panic into its error
func recoverPy(err *error) {
	if r := recover(); r != nil {
		recoverPanic(r, err)
	}
}

// Error of a recovered Python exception panic, any other panic raised again with its stack
func recoverPanic(r any, err *error) {
	switch e := r.(type) {
	case *MarkedError:
		*err = e
	case *ReaderError:
		*err = e
	case *PyException:
		*err = e
	default:
		panic(fmt.Sprintf("%v\n%s", r, debug.Stack()))
	}
}

// Single document of a pure ruamel load, the round trip scanner and parser when rt is set
func ComposePure(in Input, rt bool) (doc *Document, err error) {
	defer recoverPy(&err)
	l := &loader{rt: rt, version: v11}
	if rt {
		l.version = version{}
	}
	r, rerr := newReader(in)
	if rerr != nil {
		return nil, rerr
	}
	s := newScanner(l, r, rt)
	l.scanner = s
	p := newParser(l, s, rt)
	c := &composer{l: l, p: p}
	root := c.getSingleNode()
	return &Document{Root: root, Version: l.processingVersion(), Warnings: l.warnings, Name: r.name}, nil
}
