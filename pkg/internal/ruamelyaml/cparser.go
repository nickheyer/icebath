package ruamelyaml

import (
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Composer of the ruamel.yaml.clib CParser, libyaml events turned into nodes resolved under YAML 1.1
type cComposer struct {
	p       *yamlParser
	ev      yamlEvent
	have    bool
	anchors map[string]*Node
	stack   int64
	name    string
}

// Failure of the C loader that the safe loader answers by reloading with the pure parser
type cYAMLError struct{ kind string }

func (e *cYAMLError) Error() string { return e.kind }

func (c *cComposer) next() {
	if c.have {
		return
	}
	if !yamlParserParse(c.p, &c.ev) {
		if c.p.errorType == yamlMemoryError {
			panic(Raise("MemoryError", ""))
		}
		panic(&cYAMLError{kind: "libyaml"})
	}
	c.have = true
}

func (c *cComposer) del() { c.have = false }

// Python str of C string bytes, the UnicodeDecodeError of invalid UTF-8
func pyFromString(b []byte) string {
	b = cString(b)
	text, err := pystr.DecodeUTF8(b)
	if err != nil {
		panic(Raise("UnicodeDecodeError", err.Error()))
	}
	return text
}

func (c *cComposer) mark(m yamlMark) *Mark {
	return &Mark{Name: c.name, Index: m.index, Line: m.line, Column: m.column}
}

func (c *cComposer) getSingleNode() *Node {
	c.next()
	c.del()
	c.next()
	var document *Node
	if c.ev.typ != yamlStreamEndEvent {
		document = c.composeDocument()
	}
	c.next()
	if c.ev.typ != yamlStreamEndEvent {
		panic(&cYAMLError{kind: "ComposerError"})
	}
	return document
}

func (c *cComposer) composeDocument() *Node {
	c.del()
	node := c.composeNode()
	c.next()
	c.del()
	c.anchors = map[string]*Node{}
	return node
}

// Native stack the Cython composer may use, each collection level charging its measured frame cost
const cStackBudget = int64(1_000_000_000_000)

// Collection levels of one kind CPython 3.14 nests on an 8 MiB main thread stack before its overflow check fires
var cStackLevels = map[[2]bool]int64{{false, true}: 28957, {false, false}: 28943, {true, true}: 27426, {true, false}: 27424}

// Frame cost of one collection level, a mapping level and a block level costing a little more
func cStackCost(kind NodeKind, flow bool) int64 {
	return cStackBudget*2/(2*cStackLevels[[2]bool{kind == MappingNode, flow}]+1) + 1
}

// RecursionError CPython raises when the composer recursion exhausts the C stack
func cStackOverflow(kind NodeKind, flow bool) *PyException {
	msg := "Stack overflow (used 8148 kB)"
	if kind == MappingNode || !flow {
		msg += " while calling a Python object"
	}
	return Raise("RecursionError", msg)
}

func (c *cComposer) composeNode() *Node {
	c.next()
	if c.ev.typ == yamlAliasEvent {
		anchor := pyFromString(c.ev.anchor)
		n, ok := c.anchors[anchor]
		if !ok {
			panic(&cYAMLError{kind: "ComposerError"})
		}
		c.del()
		return n
	}
	var anchor *string
	if (c.ev.typ == yamlScalarEvent || c.ev.typ == yamlSequenceStartEvent || c.ev.typ == yamlMappingStartEvent) && c.ev.anchor != nil {
		a := pyFromString(c.ev.anchor)
		anchor = &a
		if _, dup := c.anchors[a]; dup {
			panic(&cYAMLError{kind: "ComposerError"})
		}
	}
	if c.ev.typ == yamlSequenceStartEvent || c.ev.typ == yamlMappingStartEvent {
		kind := SequenceNode
		if c.ev.typ == yamlMappingStartEvent {
			kind = MappingNode
		}
		cost := cStackCost(kind, c.ev.flow)
		c.stack += cost
		if c.stack > cStackBudget {
			panic(cStackOverflow(kind, c.ev.flow))
		}
		defer func() { c.stack -= cost }()
	}
	switch c.ev.typ {
	case yamlScalarEvent:
		return c.composeScalar(anchor)
	case yamlSequenceStartEvent:
		return c.composeCollection(anchor, SequenceNode)
	case yamlMappingStartEvent:
		return c.composeCollection(anchor, MappingNode)
	}
	panic(Raise("UnboundLocalError", "cannot access local variable 'node' where it is not associated with a value"))
}

var cStyles = map[yamlScalarStyle]rune{
	yamlSingleQuotedScalarStyle: '\'', yamlDoubleQuotedScalarStyle: '"', yamlLiteralScalarStyle: '|', yamlFoldedScalarStyle: '>',
}

func (c *cComposer) composeScalar(anchor *string) *Node {
	start := c.mark(c.ev.startMark)
	value, err := pystr.DecodeUTF8(c.ev.value)
	if err != nil {
		panic(Raise("UnicodeDecodeError", err.Error()))
	}
	n := &Node{Kind: ScalarNode, Value: value, Start: start, Style: cStyles[c.ev.style], Anchor: anchor}
	if c.ev.tag == nil || string(cString(c.ev.tag)) == "!" {
		n.tag = resolve(v11, ScalarNode, value, [2]bool{c.ev.plainImplicit, c.ev.quotedImplicit})
	} else {
		n.rawTag, n.lazy = pyFromString(c.ev.tag), true
	}
	if anchor != nil {
		c.anchors[*anchor] = n
	}
	c.del()
	return n
}

func (c *cComposer) composeCollection(anchor *string, kind NodeKind) *Node {
	n := &Node{Kind: kind, Start: c.mark(c.ev.startMark), Flow: c.ev.flow, Anchor: anchor}
	if c.ev.tag == nil || string(cString(c.ev.tag)) == "!" {
		n.tag = resolve(v11, kind, "", [2]bool{c.ev.plainImplicit})
	} else {
		n.rawTag, n.lazy = pyFromString(c.ev.tag), true
	}
	if anchor != nil {
		c.anchors[*anchor] = n
	}
	c.del()
	c.next()
	end := yamlSequenceEndEvent
	if kind == MappingNode {
		end = yamlMappingEndEvent
	}
	for c.ev.typ != end {
		if kind == SequenceNode {
			n.Items = append(n.Items, c.composeNode())
		} else {
			key := c.composeNode()
			value := c.composeNode()
			n.Pairs = append(n.Pairs, Pair{key, value})
		}
		c.next()
	}
	c.del()
	return n
}

// Single document of the C loader, ok false when libyaml or the composer raised a YAMLError
func ComposeC(in Input) (doc *Document, ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if _, isC := r.(*cYAMLError); isC {
				doc, ok, err = nil, false, nil
				return
			}
			recoverPanic(r, &err)
		}
	}()
	var input string
	name := "<byte string>"
	if in.IsBytes {
		input = string(in.Bytes)
	} else {
		name = "<unicode string>"
		text := string(in.Text)
		if e := pystr.StrictUTF8EncodeError(text); e != nil {
			return nil, false, Raise("UnicodeEncodeError", e.Error())
		}
		input = text
	}
	c := &cComposer{p: yamlParserInitialize(input), anchors: map[string]*Node{}, name: name}
	root := c.getSingleNode()
	return &Document{Root: root, Version: v11, Name: name}, true, nil
}

func yamlParserInitialize(input string) *yamlParser {
	return &yamlParser{
		input:            input,
		buffer:           make([]byte, libyamlInputBufferSize),
		rawBuffer:        make([]byte, libyamlInputRawBufferSize),
		indentsCap:       libyamlInitialStackSize,
		simpleKeysCap:    libyamlInitialStackSize,
		statesCap:        libyamlInitialStackSize,
		marksCap:         libyamlInitialStackSize,
		tagDirectivesCap: libyamlInitialStackSize,
	}
}
