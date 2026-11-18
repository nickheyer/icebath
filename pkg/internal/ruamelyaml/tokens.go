package ruamelyaml

import "strings"

type tokenKind int8

const (
	tkStreamStart tokenKind = iota
	tkStreamEnd
	tkDirective
	tkDocumentStart
	tkDocumentEnd
	tkBlockSequenceStart
	tkBlockMappingStart
	tkBlockEnd
	tkFlowSequenceStart
	tkFlowMappingStart
	tkFlowSequenceEnd
	tkFlowMappingEnd
	tkKey
	tkValue
	tkBlockEntry
	tkFlowEntry
	tkAlias
	tkAnchor
	tkTag
	tkScalar
	tkComment
)

// Token id text the parser errors print
var tokenIDs = map[tokenKind]string{
	tkStreamStart: "<stream start>", tkStreamEnd: "<stream end>", tkDirective: "<directive>",
	tkDocumentStart: "<document start>", tkDocumentEnd: "<document end>",
	tkBlockSequenceStart: "<block sequence start>", tkBlockMappingStart: "<block mapping start>", tkBlockEnd: "<block end>",
	tkFlowSequenceStart: "[", tkFlowMappingStart: "{", tkFlowSequenceEnd: "]", tkFlowMappingEnd: "}",
	tkKey: "?", tkValue: ":", tkBlockEntry: "-", tkFlowEntry: ",",
	tkAlias: "<alias>", tkAnchor: "<anchor>", tkTag: "<tag>", tkScalar: "<scalar>", tkComment: "<comment>",
}

// Old style token comment, a post comment slot and a pre comment list, five slots for the empty value case
type tokenComment struct {
	slots []any
}

type token struct {
	kind  tokenKind
	start *Mark
	end   *Mark
	// directive name and value
	name       string
	hasValue   bool
	ver        version
	tagHandle  string
	tagPrefix  string
	// alias, anchor, scalar and comment text
	value string
	plain bool
	style rune
	// tag handle, nil for a verbatim or bare tag, and suffix
	handle *string
	suffix string
	// encoding of the stream start token
	encoding string
	comment  *tokenComment
}

func (t *token) id() string { return tokenIDs[t.kind] }

func newToken(kind tokenKind, start, end *Mark) *token { return &token{kind: kind, start: start, end: end} }

// Assigns the post comment slot, refusing a comment already in the new style
func (t *token) addPostComment(c any) {
	if t.comment == nil {
		t.comment = &tokenComment{slots: []any{nil, nil}}
	} else if n := len(t.comment.slots); n != 2 && n != 5 {
		panic(Raise("AssertionError", ""))
	}
	t.comment.slots[0] = c
}

// Assigns the pre comment list, refusing a second assignment
func (t *token) addPreComments(c []any) {
	if t.comment == nil {
		t.comment = &tokenComment{slots: []any{nil, nil}}
	} else if len(t.comment.slots) != 2 {
		panic(Raise("AssertionError", ""))
	}
	if t.comment.slots[1] != nil {
		panic(Raise("AssertionError", ""))
	}
	t.comment.slots[1] = c
}

func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case []any:
		return len(x) > 0
	case *token:
		return x != nil
	case string:
		return x != ""
	}
	return true
}

// Moves this token's comment onto target the way Token.move_old_comment does
func (t *token) moveOldComment(target *token, empty bool) {
	if t.comment == nil {
		return
	}
	c := t.comment
	if target.kind == tkStreamEnd || target.kind == tkDocumentStart {
		return
	}
	t.comment = nil
	tc := target.comment
	if tc == nil || len(tc.slots) == 0 {
		if empty {
			c = &tokenComment{slots: []any{c.slots[0], c.slots[1], nil, nil, c.slots[0]}}
		}
		target.comment = c
		return
	}
	if (truthy(c.slots[0]) && truthy(tc.slots[0])) || (truthy(c.slots[1]) && truthy(tc.slots[1])) {
		cl, ok1 := c.slots[1].([]any)
		tl, ok2 := tc.slots[1].([]any)
		if ok1 && ok2 {
			c.slots[1] = append(cl, tl...)
		} else {
			panic(Raise("NotImplementedError", "overlap in comment "+commentRepr(c)+" "+commentRepr(tc)))
		}
	}
	if truthy(c.slots[0]) {
		tc.slots[0] = c.slots[0]
	}
	if truthy(c.slots[1]) {
		tc.slots[1] = c.slots[1]
	}
}

// Python repr of an old style comment list
func commentRepr(c *tokenComment) string {
	parts := make([]string, len(c.slots))
	for i, s := range c.slots {
		parts[i] = commentItemRepr(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func commentItemRepr(s any) string {
	switch x := s.(type) {
	case nil:
		return "None"
	case *token:
		return x.reprComment()
	case string:
		return pyStrRepr(x)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			parts[i] = commentItemRepr(e)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	return "?"
}

// Repr of a CommentToken, the value, the line and the column
func (t *token) reprComment() string {
	return "CommentToken(" + pyStrRepr(t.value) + ", line: " + itoa(t.start.Line) + ", col: " + itoa(t.start.Column) + ")"
}

// Splits off the post comment slot for a block sequence start
func (t *token) splitOldComment() *tokenComment {
	c := t.comment
	if c == nil || c.slots[0] == nil {
		return nil
	}
	ret := &tokenComment{slots: []any{c.slots[0], nil}}
	if c.slots[1] == nil {
		t.comment = nil
	}
	return ret
}
