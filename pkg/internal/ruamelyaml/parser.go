package ruamelyaml

import (
	"math/big"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
)

type eventKind int8

const (
	evStreamStart eventKind = iota
	evStreamEnd
	evDocumentStart
	evDocumentEnd
	evAlias
	evScalar
	evSequenceStart
	evSequenceEnd
	evMappingStart
	evMappingEnd
)

type event struct {
	kind     eventKind
	start    *Mark
	end      *Mark
	anchor   *string
	tag      *tagValue
	implicit [2]bool
	value    string
	style    rune
	flow     bool
	explicit bool
	comment  *tokenComment
}

// Tag of a node as ruamel's Tag resolves it, the handle prefix joined to the percent decoded suffix
type tagValue struct {
	text   string
	handle *string
}

// Suffix with each percent and two characters replaced by the character of that hex value, as Tag.uri_decoded_suffix
func uriDecodedSuffix(suffix string) string {
	rs := []rune(suffix)
	var out []rune
	for idx := 0; idx < len(rs); {
		ch := rs[idx]
		idx++
		if ch != '%' {
			out = append(out, ch)
			continue
		}
		part := string(pySlice(rs, idx, idx+2))
		n, ok := pyint.ParseBase(part, 16)
		if !ok {
			panic(Raise("ValueError", "invalid literal for int() with base 16: "+pyStrRepr(part)))
		}
		if n.Sign() < 0 || n.Cmp(big.NewInt(0x10FFFF)) > 0 {
			panic(Raise("ValueError", "chr() arg not in range(0x110000)"))
		}
		out = append(out, rune(n.Int64()))
		idx += 2
	}
	return string(out)
}

var defaultTags = map[string]string{"!": "!", "!!": "tag:yaml.org,2002:"}

type parseState func() *event

// ruamel Parser and RoundTripParser over the scanner token stream
type parser struct {
	l          *loader
	s          *scanner
	rt         bool
	current    *event
	last       *event
	tagHandles map[string]string
	handleList []string
	states     []parseState
	marks      []*Mark
	state      parseState
}

func parserError(context string, contextMark *Mark, problem string, problemMark *Mark) *MarkedError {
	e := &MarkedError{Class: "ParserError", ContextMark: contextMark, ProblemMark: problemMark, Problem: strp(problem)}
	if context != "" {
		e.Context = strp(context)
	}
	return e
}

func newParser(l *loader, s *scanner, rt bool) *parser {
	p := &parser{l: l, s: s, rt: rt}
	p.state = p.parseStreamStart
	return p
}

func (p *parser) checkEvent(kinds ...eventKind) bool {
	if p.current == nil && p.state != nil {
		p.current = p.state()
	}
	if p.current != nil {
		if len(kinds) == 0 {
			return true
		}
		for _, k := range kinds {
			if p.current.kind == k {
				return true
			}
		}
	}
	return false
}

func (p *parser) peekEvent() *event {
	if p.current == nil && p.state != nil {
		p.current = p.state()
	}
	return p.current
}

func (p *parser) getEvent() *event {
	if p.current == nil && p.state != nil {
		p.current = p.state()
	}
	value := p.current
	p.last = value
	p.current = nil
	return value
}

func (p *parser) popState() parseState {
	st := p.states[len(p.states)-1]
	p.states = p.states[:len(p.states)-1]
	return st
}

func (p *parser) popMark() {
	p.marks = p.marks[:len(p.marks)-1]
}

func (p *parser) moveTokenComment(t *token, nt *token, empty bool) {
	if !p.rt {
		return
	}
	if nt == nil {
		nt = p.s.peekToken()
	}
	t.moveOldComment(nt, empty)
}

func (p *parser) parseStreamStart() *event {
	t := p.s.getToken()
	p.moveTokenComment(t, nil, false)
	p.state = p.parseImplicitDocumentStart
	return &event{kind: evStreamStart, start: t.start, end: t.end}
}

func (p *parser) parseImplicitDocumentStart() *event {
	if !p.s.checkToken(tkDirective, tkDocumentStart, tkStreamEnd) {
		p.tagHandles = defaultTags
		t := p.s.peekToken()
		p.states = append(p.states, p.parseDocumentEnd)
		p.state = p.parseBlockNode
		return &event{kind: evDocumentStart, start: t.start, end: t.start}
	}
	return p.parseDocumentStart()
}

// Python repr of the one element tuple holding a token id
func tupleIDRepr(t *token) string { return "(" + pyStrRepr(t.id()) + ",)" }

func (p *parser) parseDocumentStart() *event {
	for p.s.checkToken(tkDocumentEnd) {
		p.s.getToken()
	}
	if !p.s.checkToken(tkStreamEnd) {
		p.processDirectives()
		if !p.s.checkToken(tkDocumentStart) {
			pt := p.s.peekToken()
			panic(parserError("", nil, "expected '<document start>', but found "+tupleIDRepr(pt), p.s.peekToken().start))
		}
		t := p.s.getToken()
		ev := &event{kind: evDocumentStart, start: t.start, end: t.end, explicit: true, comment: t.comment}
		p.states = append(p.states, p.parseDocumentEnd)
		p.state = p.parseDocumentContent
		return ev
	}
	t := p.s.getToken()
	ev := &event{kind: evStreamEnd, start: t.start, end: t.end, comment: t.comment}
	if len(p.states) > 0 || len(p.marks) > 0 {
		panic(Raise("AssertionError", ""))
	}
	p.state = nil
	return ev
}

func (p *parser) parseDocumentEnd() *event {
	t := p.s.peekToken()
	start, end := t.start, t.start
	explicit := false
	if p.s.checkToken(tkDocumentEnd) {
		t = p.s.getToken()
		pt := p.s.peekToken()
		if pt.kind != tkStreamEnd && t.end.Line == pt.start.Line {
			panic(parserError("", nil, "found non-comment content after document end marker, "+tupleIDRepr(p.s.peekToken()), p.s.peekToken().start))
		}
		end = t.end
		explicit = true
	}
	ev := &event{kind: evDocumentEnd, start: start, end: end, explicit: explicit}
	if p.l.processingVersion().eq(v11) {
		p.state = p.parseDocumentStart
	} else if explicit {
		p.state = p.parseImplicitDocumentStart
	} else {
		p.state = p.parseDocumentStart
	}
	return ev
}

func (p *parser) parseDocumentContent() *event {
	if p.s.checkToken(tkDirective, tkDocumentStart, tkDocumentEnd, tkStreamEnd) {
		ev := p.processEmptyScalar(p.s.peekToken().start, nil)
		p.state = p.popState()
		return ev
	}
	return p.parseBlockNode()
}

// Reads the directives of an explicit document, setting the loader version and tag handles as ruamel does
func (p *parser) processDirectives() {
	var yamlVersion version
	p.tagHandles = map[string]string{}
	p.handleList = nil
	for p.s.checkToken(tkDirective) {
		t := p.s.getToken()
		switch t.name {
		case "YAML":
			if yamlVersion.set {
				panic(parserError("", nil, "found duplicate YAML directive", t.start))
			}
			if t.ver.major.Cmp(big.NewInt(1)) != 0 {
				panic(parserError("", nil, "found incompatible YAML document (version 1.* is required)", t.start))
			}
			yamlVersion = t.ver
		case "TAG":
			if _, dup := p.tagHandles[t.tagHandle]; dup {
				panic(parserError("", nil, "duplicate tag handle "+pyStrRepr(t.tagHandle), t.start))
			}
			p.tagHandles[t.tagHandle] = t.tagPrefix
			p.handleList = append(p.handleList, t.tagHandle)
		}
	}
	p.l.setVersion(yamlVersion)
	for _, key := range []string{"!", "!!"} {
		if _, ok := p.tagHandles[key]; !ok {
			p.tagHandles[key] = defaultTags[key]
		}
	}
}

func (p *parser) parseBlockNode() *event { return p.parseNode(true, false) }

func (p *parser) parseFlowNode() *event { return p.parseNode(false, false) }

func (p *parser) parseBlockNodeOrIndentlessSequence() *event { return p.parseNode(true, true) }

// Resolved text of a tag token under the current handles, the undefined handle a ParserError
func (p *parser) makeTag(t *token, start, tagMark *Mark) *tagValue {
	if t.handle != nil {
		prefix, ok := p.tagHandles[*t.handle]
		if !ok {
			panic(parserError("while parsing a node", start, "found undefined tag handle "+pyStrRepr(*t.handle), tagMark))
		}
		return &tagValue{text: prefix + uriDecodedSuffix(t.suffix), handle: t.handle}
	}
	return &tagValue{text: uriDecodedSuffix(t.suffix)}
}

func (p *parser) parseNode(block, indentlessSequence bool) *event {
	if p.s.checkToken(tkAlias) {
		t := p.s.getToken()
		v := t.value
		p.state = p.popState()
		return &event{kind: evAlias, anchor: &v, start: t.start, end: t.end}
	}
	var anchor *string
	var tagTok *token
	var start, end, tagMark *Mark
	if p.s.checkToken(tkAnchor) {
		t := p.s.getToken()
		p.moveTokenComment(t, nil, false)
		start, end = t.start, t.end
		v := t.value
		anchor = &v
		if p.s.checkToken(tkTag) {
			t = p.s.getToken()
			tagMark, end = t.start, t.end
			tagTok = t
		}
	} else if p.s.checkToken(tkTag) {
		t := p.s.getToken()
		p.moveTagTokenComment(t)
		start, tagMark, end = t.start, t.start, t.end
		tagTok = t
		if p.s.checkToken(tkAnchor) {
			t = p.s.getToken()
			start, tagMark, end = t.start, t.start, t.end
			v := t.value
			anchor = &v
		}
	}
	var tag *tagValue
	if tagTok != nil {
		tag = p.makeTag(tagTok, start, tagMark)
	}
	if start == nil {
		start = p.s.peekToken().start
		end = start
	}
	implicit := tag == nil || tag.text == "!"
	if indentlessSequence && p.s.checkToken(tkBlockEntry) {
		var comment *tokenComment
		pt := p.s.peekToken()
		if pt.comment != nil && truthy(pt.comment.slots[0]) {
			comment = &tokenComment{slots: []any{pt.comment.slots[0], []any{}}}
			pt.comment.slots[0] = nil
		} else if pt.comment != nil && pt.comment.slots[0] == nil && truthy(pt.comment.slots[1]) {
			comment = &tokenComment{slots: []any{nil, pt.comment.slots[1]}}
			pt.comment.slots[1] = nil
		}
		end = p.s.peekToken().end
		p.state = p.parseIndentlessSequenceEntry
		return &event{kind: evSequenceStart, anchor: anchor, tag: tag, implicit: [2]bool{implicit}, start: start, end: end, comment: comment}
	}
	var ev *event
	switch {
	case p.s.checkToken(tkScalar):
		t := p.s.getToken()
		end = t.end
		var dimplicit [2]bool
		switch {
		case (t.plain && tag == nil) || (tag != nil && tag.text == "!"):
			dimplicit = [2]bool{true, false}
		case tag == nil:
			dimplicit = [2]bool{false, true}
		}
		ev = &event{kind: evScalar, anchor: anchor, tag: tag, implicit: dimplicit, value: t.value, start: start, end: end, style: t.style, comment: t.comment}
		p.state = p.popState()
	case p.s.checkToken(tkFlowSequenceStart):
		pt := p.s.peekToken()
		ev = &event{kind: evSequenceStart, anchor: anchor, tag: tag, implicit: [2]bool{implicit}, start: start, end: pt.end, flow: true, comment: pt.comment}
		p.state = p.parseFlowSequenceFirstEntry
	case p.s.checkToken(tkFlowMappingStart):
		pt := p.s.peekToken()
		ev = &event{kind: evMappingStart, anchor: anchor, tag: tag, implicit: [2]bool{implicit}, start: start, end: pt.end, flow: true, comment: pt.comment}
		p.state = p.parseFlowMappingFirstKey
	case block && p.s.checkToken(tkBlockSequenceStart):
		end = p.s.peekToken().start
		pt := p.s.peekToken()
		comment := pt.comment
		if comment == nil || comment.slots[1] == nil {
			comment = pt.splitOldComment()
		}
		ev = &event{kind: evSequenceStart, anchor: anchor, tag: tag, implicit: [2]bool{implicit}, start: start, end: end, comment: comment}
		p.state = p.parseBlockSequenceFirstEntry
	case block && p.s.checkToken(tkBlockMappingStart):
		end = p.s.peekToken().start
		comment := p.s.peekToken().comment
		ev = &event{kind: evMappingStart, anchor: anchor, tag: tag, implicit: [2]bool{implicit}, start: start, end: end, comment: comment}
		p.state = p.parseBlockMappingFirstKey
	case anchor != nil || tag != nil:
		ev = &event{kind: evScalar, anchor: anchor, tag: tag, implicit: [2]bool{implicit, false}, value: "", start: start, end: end}
		p.state = p.popState()
	default:
		node := "flow"
		if block {
			node = "block"
		}
		t := p.s.peekToken()
		panic(parserError("while parsing a "+node+" node", start, "expected the node content, but found "+pyStrRepr(t.id()), t.start))
	}
	return ev
}

// Moves the comment of a tag token, swallowing the comment overlap NotImplementedError like the parser does
func (p *parser) moveTagTokenComment(t *token) {
	if !p.rt {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			if e, ok := r.(*PyException); ok && e.Class == "NotImplementedError" {
				return
			}
			panic(r)
		}
	}()
	p.moveTokenComment(t, nil, false)
}

func (p *parser) parseBlockSequenceFirstEntry() *event {
	t := p.s.getToken()
	p.marks = append(p.marks, t.start)
	return p.parseBlockSequenceEntry()
}

func (p *parser) parseBlockSequenceEntry() *event {
	if p.s.checkToken(tkBlockEntry) {
		t := p.s.getToken()
		p.moveTokenComment(t, nil, false)
		if !p.s.checkToken(tkBlockEntry, tkBlockEnd) {
			p.states = append(p.states, p.parseBlockSequenceEntry)
			return p.parseBlockNode()
		}
		p.state = p.parseBlockSequenceEntry
		return p.processEmptyScalar(t.end, nil)
	}
	if !p.s.checkToken(tkBlockEnd) {
		t := p.s.peekToken()
		panic(parserError("while parsing a block collection", p.marks[len(p.marks)-1], "expected <block end>, but found "+pyStrRepr(t.id()), t.start))
	}
	t := p.s.getToken()
	ev := &event{kind: evSequenceEnd, start: t.start, end: t.end, comment: t.comment}
	p.state = p.popState()
	p.popMark()
	return ev
}

func (p *parser) parseIndentlessSequenceEntry() *event {
	if p.s.checkToken(tkBlockEntry) {
		t := p.s.getToken()
		p.moveTokenComment(t, nil, false)
		if !p.s.checkToken(tkBlockEntry, tkKey, tkValue, tkBlockEnd) {
			p.states = append(p.states, p.parseIndentlessSequenceEntry)
			return p.parseBlockNode()
		}
		p.state = p.parseIndentlessSequenceEntry
		return p.processEmptyScalar(t.end, nil)
	}
	t := p.s.peekToken()
	ev := &event{kind: evSequenceEnd, start: t.start, end: t.start, comment: t.comment}
	p.state = p.popState()
	return ev
}

func (p *parser) parseBlockMappingFirstKey() *event {
	t := p.s.getToken()
	p.marks = append(p.marks, t.start)
	return p.parseBlockMappingKey()
}

func (p *parser) parseBlockMappingKey() *event {
	if p.s.checkToken(tkKey) {
		t := p.s.getToken()
		p.moveTokenComment(t, nil, false)
		if !p.s.checkToken(tkKey, tkValue, tkBlockEnd) {
			p.states = append(p.states, p.parseBlockMappingValue)
			return p.parseBlockNodeOrIndentlessSequence()
		}
		p.state = p.parseBlockMappingValue
		return p.processEmptyScalar(t.end, nil)
	}
	if p.l.processingVersion().greater(v11) && p.s.checkToken(tkValue) {
		p.state = p.parseBlockMappingValue
		return p.processEmptyScalar(p.s.peekToken().start, nil)
	}
	if !p.s.checkToken(tkBlockEnd) {
		t := p.s.peekToken()
		panic(parserError("while parsing a block mapping", p.marks[len(p.marks)-1], "expected <block end>, but found "+pyStrRepr(t.id()), t.start))
	}
	t := p.s.getToken()
	p.moveTokenComment(t, nil, false)
	ev := &event{kind: evMappingEnd, start: t.start, end: t.end, comment: t.comment}
	p.state = p.popState()
	p.popMark()
	return ev
}

func (p *parser) parseBlockMappingValue() *event {
	if p.s.checkToken(tkValue) {
		t := p.s.getToken()
		if p.s.checkToken(tkValue) {
			p.moveTokenComment(t, nil, false)
		} else if !p.s.checkToken(tkKey) {
			p.moveTokenComment(t, nil, true)
		}
		if !p.s.checkToken(tkKey, tkValue, tkBlockEnd) {
			p.states = append(p.states, p.parseBlockMappingKey)
			return p.parseBlockNodeOrIndentlessSequence()
		}
		p.state = p.parseBlockMappingKey
		comment := t.comment
		if comment == nil {
			t = p.s.peekToken()
			comment = t.comment
			if comment != nil && len(comment.slots) > 0 {
				t.comment = &tokenComment{slots: []any{nil, comment.slots[1]}}
				comment = &tokenComment{slots: []any{comment.slots[0], nil}}
			}
		}
		return p.processEmptyScalar(t.end, comment)
	}
	p.state = p.parseBlockMappingKey
	t := p.s.peekToken()
	return p.processEmptyScalar(t.start, nil)
}

func (p *parser) parseFlowSequenceFirstEntry() *event {
	t := p.s.getToken()
	p.marks = append(p.marks, t.start)
	return p.parseFlowSequenceEntry(true)
}

func (p *parser) parseFlowSequenceEntryNext() *event { return p.parseFlowSequenceEntry(false) }

func (p *parser) parseFlowSequenceEntry(first bool) *event {
	if !p.s.checkToken(tkFlowSequenceEnd) {
		if !first {
			if p.s.checkToken(tkFlowEntry) {
				p.s.getToken()
			} else {
				t := p.s.peekToken()
				panic(parserError("while parsing a flow sequence", p.marks[len(p.marks)-1], "expected ',' or ']', but got "+pyStrRepr(t.id()), t.start))
			}
		}
		if p.s.checkToken(tkKey) {
			t := p.s.peekToken()
			p.state = p.parseFlowSequenceEntryMappingKey
			return &event{kind: evMappingStart, implicit: [2]bool{true}, start: t.start, end: t.end, flow: true}
		} else if !p.s.checkToken(tkFlowSequenceEnd) {
			p.states = append(p.states, p.parseFlowSequenceEntryNext)
			return p.parseFlowNode()
		}
	}
	t := p.s.getToken()
	ev := &event{kind: evSequenceEnd, start: t.start, end: t.end, comment: t.comment}
	p.state = p.popState()
	p.popMark()
	return ev
}

func (p *parser) parseFlowSequenceEntryMappingKey() *event {
	t := p.s.getToken()
	if !p.s.checkToken(tkValue, tkFlowEntry, tkFlowSequenceEnd) {
		p.states = append(p.states, p.parseFlowSequenceEntryMappingValue)
		return p.parseFlowNode()
	}
	p.state = p.parseFlowSequenceEntryMappingValue
	return p.processEmptyScalar(t.end, nil)
}

func (p *parser) parseFlowSequenceEntryMappingValue() *event {
	if p.s.checkToken(tkValue) {
		t := p.s.getToken()
		if !p.s.checkToken(tkFlowEntry, tkFlowSequenceEnd) {
			p.states = append(p.states, p.parseFlowSequenceEntryMappingEnd)
			return p.parseFlowNode()
		}
		p.state = p.parseFlowSequenceEntryMappingEnd
		return p.processEmptyScalar(t.end, nil)
	}
	p.state = p.parseFlowSequenceEntryMappingEnd
	t := p.s.peekToken()
	return p.processEmptyScalar(t.start, nil)
}

func (p *parser) parseFlowSequenceEntryMappingEnd() *event {
	p.state = p.parseFlowSequenceEntryNext
	t := p.s.peekToken()
	return &event{kind: evMappingEnd, start: t.start, end: t.start}
}

func (p *parser) parseFlowMappingFirstKey() *event {
	t := p.s.getToken()
	p.marks = append(p.marks, t.start)
	return p.parseFlowMappingKey(true)
}

func (p *parser) parseFlowMappingKeyNext() *event { return p.parseFlowMappingKey(false) }

func (p *parser) parseFlowMappingKey(first bool) *event {
	if !p.s.checkToken(tkFlowMappingEnd) {
		if !first {
			if p.s.checkToken(tkFlowEntry) {
				p.s.getToken()
			} else {
				t := p.s.peekToken()
				panic(parserError("while parsing a flow mapping", p.marks[len(p.marks)-1], "expected ',' or '}', but got "+pyStrRepr(t.id()), t.start))
			}
		}
		if p.s.checkToken(tkKey) {
			t := p.s.getToken()
			if !p.s.checkToken(tkValue, tkFlowEntry, tkFlowMappingEnd) {
				p.states = append(p.states, p.parseFlowMappingValue)
				return p.parseFlowNode()
			}
			p.state = p.parseFlowMappingValue
			return p.processEmptyScalar(t.end, nil)
		} else if p.l.processingVersion().greater(v11) && p.s.checkToken(tkValue) {
			p.state = p.parseFlowMappingValue
			return p.processEmptyScalar(p.s.peekToken().end, nil)
		} else if !p.s.checkToken(tkFlowMappingEnd) {
			p.states = append(p.states, p.parseFlowMappingEmptyValue)
			return p.parseFlowNode()
		}
	}
	t := p.s.getToken()
	ev := &event{kind: evMappingEnd, start: t.start, end: t.end, comment: t.comment}
	p.state = p.popState()
	p.popMark()
	return ev
}

func (p *parser) parseFlowMappingValue() *event {
	if p.s.checkToken(tkValue) {
		t := p.s.getToken()
		if !p.s.checkToken(tkFlowEntry, tkFlowMappingEnd) {
			p.states = append(p.states, p.parseFlowMappingKeyNext)
			return p.parseFlowNode()
		}
		p.state = p.parseFlowMappingKeyNext
		return p.processEmptyScalar(t.end, nil)
	}
	p.state = p.parseFlowMappingKeyNext
	t := p.s.peekToken()
	return p.processEmptyScalar(t.start, nil)
}

func (p *parser) parseFlowMappingEmptyValue() *event {
	p.state = p.parseFlowMappingKeyNext
	return p.processEmptyScalar(p.s.peekToken().start, nil)
}

func (p *parser) processEmptyScalar(mark *Mark, comment *tokenComment) *event {
	return &event{kind: evScalar, implicit: [2]bool{true, false}, value: "", start: mark, end: mark, comment: comment}
}
