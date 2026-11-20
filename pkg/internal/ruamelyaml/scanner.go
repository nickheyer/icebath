package ruamelyaml

import (
	"math/big"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

type simpleKey struct {
	tokenNumber int
	required    bool
	index       int
	line        int
	column      int
	mark        *Mark
}

// Result of a comment scan in the round trip scanner, the comment text with its marks
type scannedComment struct {
	value      string
	start, end *Mark
}

// ruamel Scanner, the round trip variant collecting comment tokens when rt is set
type scanner struct {
	l              *loader
	r              *reader
	rt             bool
	done           bool
	flowContext    []rune
	tokens         []*token
	tokensTaken    int
	indent         int
	indents        []int
	allowSimpleKey bool
	keyLevels      []int
	keys           map[int]*simpleKey
	yamlVersion    version
}

func scannerError(context string, contextMark *Mark, problem string, problemMark *Mark) *MarkedError {
	e := &MarkedError{Class: "ScannerError", ContextMark: contextMark, ProblemMark: problemMark, Problem: strp(problem)}
	if context != "" {
		e.Context = strp(context)
	}
	return e
}

func newScanner(l *loader, r *reader, rt bool) *scanner {
	s := &scanner{l: l, r: r, rt: rt, keys: map[int]*simpleKey{}}
	mark := r.getMark()
	s.tokens = append(s.tokens, &token{kind: tkStreamStart, start: mark, end: mark, encoding: r.encoding})
	s.indent = -1
	s.allowSimpleKey = true
	return s
}

func (s *scanner) flowLevel() int { return len(s.flowContext) }

func (s *scanner) processingVersion() version { return s.l.processingVersion() }

func (s *scanner) isTok(i int, kinds ...tokenKind) bool {
	for _, k := range kinds {
		if s.tokens[i].kind == k {
			return true
		}
	}
	return false
}

// Whether the next token is one of kinds, any token when kinds is empty
func (s *scanner) checkToken(kinds ...tokenKind) bool {
	for s.needMoreTokens() {
		s.fetchMoreTokens()
	}
	if s.rt {
		s.gatherComments()
	}
	if len(s.tokens) > 0 {
		if len(kinds) == 0 {
			return true
		}
		return s.isTok(0, kinds...)
	}
	return false
}

func (s *scanner) peekToken() *token {
	for s.needMoreTokens() {
		s.fetchMoreTokens()
	}
	if s.rt {
		s.gatherComments()
	}
	if len(s.tokens) > 0 {
		return s.tokens[0]
	}
	return nil
}

func (s *scanner) popToken(i int) *token {
	t := s.tokens[i]
	s.tokens = append(s.tokens[:i:i], s.tokens[i+1:]...)
	return t
}

func (s *scanner) getToken() *token {
	for s.needMoreTokens() {
		s.fetchMoreTokens()
	}
	if !s.rt {
		if len(s.tokens) > 0 {
			s.tokensTaken++
			return s.popToken(0)
		}
		return nil
	}
	s.gatherComments()
	if len(s.tokens) == 0 {
		return nil
	}
	if len(s.tokens) > 1 && s.isTok(0, tkScalar, tkValue, tkFlowSequenceEnd, tkFlowMappingEnd) && s.tokens[1].kind == tkComment &&
		s.tokens[0].end.Line == s.tokens[1].start.Line {
		s.tokensTaken++
		c := s.popToken(1)
		s.fetchMoreTokens()
		for len(s.tokens) > 1 && s.tokens[1].kind == tkComment {
			s.tokensTaken++
			c1 := s.popToken(1)
			c.value = c.value + strings.Repeat(" ", c1.start.Column) + c1.value
			s.fetchMoreTokens()
		}
		s.tokens[0].addPostComment(c)
	} else if len(s.tokens) > 1 && s.tokens[0].kind == tkScalar && s.tokens[1].kind == tkComment && s.tokens[0].end.Line != s.tokens[1].start.Line {
		s.tokensTaken++
		c := s.popToken(1)
		c.value = strings.Repeat("\n", max(0, c.start.Line-s.tokens[0].end.Line)) + strings.Repeat(" ", c.start.Column) + c.value
		s.tokens[0].addPostComment(c)
		s.fetchMoreTokens()
		for len(s.tokens) > 1 && s.tokens[1].kind == tkComment {
			s.tokensTaken++
			c1 := s.popToken(1)
			c.value = c.value + strings.Repeat(" ", c1.start.Column) + c1.value
			s.fetchMoreTokens()
		}
	}
	s.tokensTaken++
	return s.popToken(0)
}

// Combines leading comment tokens into the pre comments of the next real token
func (s *scanner) gatherComments() {
	var comments []any
	if len(s.tokens) == 0 {
		return
	}
	if s.tokens[0].kind == tkComment {
		comments = append(comments, s.popToken(0))
		s.tokensTaken++
	}
	for s.needMoreTokens() {
		s.fetchMoreTokens()
		if len(s.tokens) == 0 {
			return
		}
		if s.tokens[0].kind == tkComment {
			s.tokensTaken++
			comments = append(comments, s.popToken(0))
		}
	}
	if len(comments) >= 1 {
		if len(s.tokens) == 0 {
			panic(Raise("IndexError", "list index out of range"))
		}
		s.tokens[0].addPreComments(comments)
	}
	if !s.done && len(s.tokens) < 2 {
		s.fetchMoreTokens()
	}
}

func (s *scanner) needMoreTokens() bool {
	if s.done {
		return false
	}
	if len(s.tokens) == 0 {
		return true
	}
	s.staleKeys()
	if n, ok := s.nextPossibleSimpleKey(); ok && n == s.tokensTaken {
		return true
	}
	return false
}

func (s *scanner) fetchMoreTokens() {
	if c := s.scanToNextToken(); c != nil {
		s.fetchComment(c)
		return
	}
	s.staleKeys()
	s.unwindIndent(s.r.column)
	ch := s.r.peek(0)
	switch {
	case ch == 0:
		s.fetchStreamEnd()
		return
	case ch == '%' && s.r.column == 0:
		s.fetchDirective()
		return
	case ch == '-' && s.checkDocumentStart():
		s.fetchDocumentIndicator(tkDocumentStart)
		return
	case ch == '.' && s.checkDocumentEnd():
		s.fetchDocumentIndicator(tkDocumentEnd)
		return
	case ch == '[':
		s.fetchFlowCollectionStart(tkFlowSequenceStart, '[')
		return
	case ch == '{':
		s.fetchFlowCollectionStart(tkFlowMappingStart, '{')
		return
	case ch == ']':
		s.fetchFlowCollectionEnd(tkFlowSequenceEnd)
		return
	case ch == '}':
		s.fetchFlowCollectionEnd(tkFlowMappingEnd)
		return
	case ch == ',':
		s.fetchFlowEntry()
		return
	case ch == '-' && in(s.r.peek(1), theEndSpaceTab):
		s.fetchBlockEntry()
		return
	case ch == '?' && s.checkKey():
		s.fetchKey()
		return
	case ch == ':' && s.checkValue():
		s.fetchValue()
		return
	case ch == '*':
		s.fetchAnchor(tkAlias)
		return
	case ch == '&':
		s.fetchAnchor(tkAnchor)
		return
	case ch == '!':
		s.saveSimpleKey()
		s.allowSimpleKey = false
		s.tokens = append(s.tokens, s.scanTag())
		return
	case ch == '|' && s.flowLevel() == 0:
		s.fetchBlockScalar('|')
		return
	case ch == '>' && s.flowLevel() == 0:
		s.fetchBlockScalar('>')
		return
	case ch == '\'':
		s.fetchFlowScalar('\'')
		return
	case ch == '"':
		s.fetchFlowScalar('"')
		return
	}
	if s.checkPlain() {
		s.saveSimpleKey()
		s.allowSimpleKey = false
		s.tokens = append(s.tokens, s.scanPlain())
		return
	}
	panic(scannerError("while scanning for the next token", nil, "found character "+charRepr(ch)+" that cannot start any token", s.r.getMark()))
}

func (s *scanner) fetchComment(c *scannedComment) {
	value := c.value
	for value != "" && value[len(value)-1] == ' ' {
		value = value[:len(value)-1]
	}
	s.tokens = append(s.tokens, &token{kind: tkComment, value: value, start: c.start, end: c.end})
}

func (s *scanner) nextPossibleSimpleKey() (int, bool) {
	found, min := false, 0
	for _, level := range s.keyLevels {
		k := s.keys[level]
		if !found || k.tokenNumber < min {
			min, found = k.tokenNumber, true
		}
	}
	return min, found
}

func (s *scanner) delKey(level int) {
	delete(s.keys, level)
	for i, l := range s.keyLevels {
		if l == level {
			s.keyLevels = append(s.keyLevels[:i:i], s.keyLevels[i+1:]...)
			return
		}
	}
}

// Drops saved simple keys that left their line or ran past 1024 characters, a required one being an error
func (s *scanner) staleKeys() {
	for _, level := range append([]int(nil), s.keyLevels...) {
		k := s.keys[level]
		if k.line != s.r.line || s.r.index-k.index > 1024 {
			if k.required {
				panic(scannerError("while scanning a simple key", k.mark, "could not find expected ':'", s.r.getMark()))
			}
			s.delKey(level)
		}
	}
}

func (s *scanner) saveSimpleKey() {
	required := s.flowLevel() == 0 && s.indent == s.r.column
	if s.allowSimpleKey {
		s.removeSimpleKey()
		k := &simpleKey{tokenNumber: s.tokensTaken + len(s.tokens), required: required, index: s.r.index, line: s.r.line, column: s.r.column, mark: s.r.getMark()}
		s.keys[s.flowLevel()] = k
		s.keyLevels = append(s.keyLevels, s.flowLevel())
	}
}

func (s *scanner) removeSimpleKey() {
	if k, ok := s.keys[s.flowLevel()]; ok {
		if k.required {
			panic(scannerError("while scanning a simple key", k.mark, "could not find expected ':'", s.r.getMark()))
		}
		s.delKey(s.flowLevel())
	}
}

func (s *scanner) unwindIndent(column int) {
	if s.flowLevel() > 0 {
		return
	}
	for s.indent > column {
		mark := s.r.getMark()
		s.indent = s.indents[len(s.indents)-1]
		s.indents = s.indents[:len(s.indents)-1]
		s.tokens = append(s.tokens, newToken(tkBlockEnd, mark, mark))
	}
}

func (s *scanner) addIndent(column int) bool {
	if s.indent < column {
		s.indents = append(s.indents, s.indent)
		s.indent = column
		return true
	}
	return false
}

func (s *scanner) fetchStreamEnd() {
	s.unwindIndent(-1)
	s.removeSimpleKey()
	s.allowSimpleKey = false
	s.keys, s.keyLevels = map[int]*simpleKey{}, nil
	mark := s.r.getMark()
	s.tokens = append(s.tokens, newToken(tkStreamEnd, mark, mark))
	s.done = true
}

func (s *scanner) fetchDirective() {
	s.unwindIndent(-1)
	s.removeSimpleKey()
	s.allowSimpleKey = false
	s.tokens = append(s.tokens, s.scanDirective())
}

func (s *scanner) fetchDocumentIndicator(kind tokenKind) {
	s.unwindIndent(-1)
	s.removeSimpleKey()
	s.allowSimpleKey = false
	start := s.r.getMark()
	s.r.forward(3)
	s.tokens = append(s.tokens, newToken(kind, start, s.r.getMark()))
}

func (s *scanner) fetchFlowCollectionStart(kind tokenKind, push rune) {
	s.saveSimpleKey()
	s.flowContext = append(s.flowContext, push)
	s.allowSimpleKey = true
	start := s.r.getMark()
	s.r.forward(1)
	s.tokens = append(s.tokens, newToken(kind, start, s.r.getMark()))
}

func (s *scanner) fetchFlowCollectionEnd(kind tokenKind) {
	s.removeSimpleKey()
	if len(s.flowContext) > 0 {
		s.flowContext = s.flowContext[:len(s.flowContext)-1]
	}
	s.allowSimpleKey = false
	start := s.r.getMark()
	s.r.forward(1)
	s.tokens = append(s.tokens, newToken(kind, start, s.r.getMark()))
}

func (s *scanner) fetchFlowEntry() {
	s.allowSimpleKey = true
	s.removeSimpleKey()
	start := s.r.getMark()
	s.r.forward(1)
	s.tokens = append(s.tokens, newToken(tkFlowEntry, start, s.r.getMark()))
}

func (s *scanner) fetchBlockEntry() {
	if s.flowLevel() == 0 {
		if !s.allowSimpleKey {
			panic(scannerError("", nil, "sequence entries are not allowed here", s.r.getMark()))
		}
		if s.addIndent(s.r.column) {
			mark := s.r.getMark()
			s.tokens = append(s.tokens, newToken(tkBlockSequenceStart, mark, mark))
		}
	}
	s.allowSimpleKey = true
	s.removeSimpleKey()
	start := s.r.getMark()
	s.r.forward(1)
	s.tokens = append(s.tokens, newToken(tkBlockEntry, start, s.r.getMark()))
}

func (s *scanner) fetchKey() {
	if s.flowLevel() == 0 {
		if !s.allowSimpleKey {
			panic(scannerError("", nil, "mapping keys are not allowed here", s.r.getMark()))
		}
		if s.addIndent(s.r.column) {
			mark := s.r.getMark()
			s.tokens = append(s.tokens, newToken(tkBlockMappingStart, mark, mark))
		}
	}
	s.allowSimpleKey = s.flowLevel() == 0
	s.removeSimpleKey()
	start := s.r.getMark()
	s.r.forward(1)
	s.tokens = append(s.tokens, newToken(tkKey, start, s.r.getMark()))
}

func (s *scanner) insertToken(pos int, t *token) {
	s.tokens = append(s.tokens, nil)
	copy(s.tokens[pos+1:], s.tokens[pos:])
	s.tokens[pos] = t
}

func (s *scanner) fetchValue() {
	if k, ok := s.keys[s.flowLevel()]; ok {
		s.delKey(s.flowLevel())
		s.insertToken(k.tokenNumber-s.tokensTaken, newToken(tkKey, k.mark, k.mark))
		if s.flowLevel() == 0 && s.addIndent(k.column) {
			s.insertToken(k.tokenNumber-s.tokensTaken, newToken(tkBlockMappingStart, k.mark, k.mark))
		}
		s.allowSimpleKey = false
	} else {
		if s.flowLevel() == 0 && !s.allowSimpleKey {
			panic(scannerError("", nil, "mapping values are not allowed here", s.r.getMark()))
		}
		if s.flowLevel() == 0 && s.addIndent(s.r.column) {
			mark := s.r.getMark()
			s.tokens = append(s.tokens, newToken(tkBlockMappingStart, mark, mark))
		}
		s.allowSimpleKey = s.flowLevel() == 0
		s.removeSimpleKey()
	}
	start := s.r.getMark()
	s.r.forward(1)
	s.tokens = append(s.tokens, newToken(tkValue, start, s.r.getMark()))
}

func (s *scanner) fetchAnchor(kind tokenKind) {
	s.saveSimpleKey()
	s.allowSimpleKey = false
	s.tokens = append(s.tokens, s.scanAnchor(kind))
}

func (s *scanner) fetchBlockScalar(style rune) {
	s.allowSimpleKey = true
	s.removeSimpleKey()
	s.tokens = append(s.tokens, s.scanBlockScalar(style))
}

func (s *scanner) fetchFlowScalar(style rune) {
	s.saveSimpleKey()
	s.allowSimpleKey = false
	s.tokens = append(s.tokens, s.scanFlowScalar(style))
}

func (s *scanner) checkDocumentStart() bool {
	return s.r.column == 0 && s.r.prefixString(3) == "---" && in(s.r.peek(3), theEndSpaceTab)
}

func (s *scanner) checkDocumentEnd() bool {
	return s.r.column == 0 && s.r.prefixString(3) == "..." && in(s.r.peek(3), theEndSpaceTab)
}

func (s *scanner) checkKey() bool {
	if s.flowLevel() > 0 {
		return true
	}
	return in(s.r.peek(1), theEndSpaceTab)
}

func (s *scanner) checkValue() bool {
	if s.processingVersion().eq(v11) {
		if s.flowLevel() > 0 {
			return true
		}
	} else if s.flowLevel() > 0 {
		if s.flowContext[len(s.flowContext)-1] == '[' {
			if !in(s.r.peek(1), theEndSpaceTab) {
				return false
			}
		} else if len(s.tokens) > 0 && s.tokens[len(s.tokens)-1].kind == tkValue {
			if !in(s.r.peek(1), theEndSpaceTab) {
				return false
			}
		}
		return true
	}
	return in(s.r.peek(1), theEndSpaceTab)
}

const plainExcluded = "\x00 \t\r\n\u0085  -?:,[]{}#&*!|>'\"%@`"

func (s *scanner) checkPlain() bool {
	ch := s.r.peek(0)
	if s.processingVersion().eq(v11) {
		if !in(ch, plainExcluded) {
			return true
		}
		return !in(s.r.peek(1), theEndSpaceTab) && (ch == '-' || (s.flowLevel() == 0 && in(ch, "?:")))
	}
	if !in(ch, plainExcluded) {
		return true
	}
	ch1 := s.r.peek(1)
	if ch == '-' && !in(ch1, theEndSpaceTab) {
		return true
	}
	if ch == ':' && s.flowLevel() > 0 && !in(ch1, " \t") {
		return true
	}
	return !in(s.r.peek(1), theEndSpaceTab) && (ch == '-' || (s.flowLevel() == 0 && in(ch, "?:")))
}

// Skips spaces, comments and line breaks, the round trip scanner returning comments and blank line runs
func (s *scanner) scanToNextToken() *scannedComment {
	if s.r.index == 0 && s.r.peek(0) == 0xFEFF {
		s.r.forward(1)
	}
	whiteSpace := " "
	if s.flowLevel() > 0 {
		whiteSpace = " \t"
	}
	if !s.rt {
		for {
			for in(s.r.peek(0), whiteSpace) {
				s.r.forward(1)
			}
			if s.r.peek(0) == '#' {
				for !in(s.r.peek(0), theEnd) {
					s.r.forward(1)
				}
			}
			if s.scanLineBreak(false) != "" {
				if s.flowLevel() == 0 {
					s.allowSimpleKey = true
				}
			} else {
				return nil
			}
		}
	}
	for {
		for in(s.r.peek(0), whiteSpace) {
			s.r.forward(1)
		}
		ch := s.r.peek(0)
		if ch == '#' {
			start := s.r.getMark()
			var comment []rune
			comment = append(comment, ch)
			s.r.forward(1)
			for !in(ch, theEnd) {
				ch = s.r.peek(0)
				if ch == 0 {
					comment = append(comment, '\n')
					break
				}
				comment = append(comment, ch)
				s.r.forward(1)
			}
			lb := s.scanLineBreak(false)
			for lb != "" {
				comment = append(comment, []rune(lb)...)
				lb = s.scanLineBreak(false)
			}
			end := s.r.getMark()
			if s.flowLevel() == 0 {
				s.allowSimpleKey = true
			}
			return &scannedComment{value: pystr.FromRunes(comment), start: start, end: end}
		}
		if s.scanLineBreak(false) != "" {
			start := s.r.getMark()
			if s.flowLevel() == 0 {
				s.allowSimpleKey = true
			}
			ch = s.r.peek(0)
			if ch == '\n' {
				start = s.r.getMark()
				comment := ""
				lb := "x"
				for lb != "" {
					lb = s.scanLineBreak(true)
					comment += lb
				}
				if s.r.peek(0) == '#' {
					if i := strings.LastIndex(comment, "\n"); i >= 0 {
						comment = comment[:i]
					}
					comment += "\n"
				}
				return &scannedComment{value: comment, start: start, end: s.r.getMark()}
			}
		} else {
			return nil
		}
	}
}

func (s *scanner) scanDirective() *token {
	start := s.r.getMark()
	s.r.forward(1)
	name := s.scanDirectiveName(start)
	t := &token{kind: tkDirective, name: name, start: start}
	switch name {
	case "YAML":
		t.ver = s.scanYAMLDirectiveValue(start)
		t.hasValue = true
		t.end = s.r.getMark()
	case "TAG":
		t.tagHandle, t.tagPrefix = s.scanTagDirectiveValue(start)
		t.hasValue = true
		t.end = s.r.getMark()
	default:
		t.end = s.r.getMark()
		for !in(s.r.peek(0), theEnd) {
			s.r.forward(1)
		}
	}
	s.scanDirectiveIgnoredLine(start)
	return t
}

func isAlnumDash(ch rune, extra string) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || in(ch, extra)
}

func (s *scanner) scanDirectiveName(start *Mark) string {
	length := 0
	ch := s.r.peek(length)
	for isAlnumDash(ch, "-_:.") {
		length++
		ch = s.r.peek(length)
	}
	if length == 0 {
		panic(scannerError("while scanning a directive", start, "expected alphabetic or numeric character, but found "+charRepr(ch), s.r.getMark()))
	}
	value := s.r.prefixString(length)
	s.r.forward(length)
	ch = s.r.peek(0)
	if !in(ch, "\x00 \r\n\u0085  ") {
		panic(scannerError("while scanning a directive", start, "expected alphabetic or numeric character, but found "+charRepr(ch), s.r.getMark()))
	}
	return value
}

func (s *scanner) scanYAMLDirectiveValue(start *Mark) version {
	for s.r.peek(0) == ' ' {
		s.r.forward(1)
	}
	major := s.scanYAMLDirectiveNumber(start)
	if s.r.peek(0) != '.' {
		panic(scannerError("while scanning a directive", start, "expected a digit or '.', but found "+charRepr(s.r.peek(0)), s.r.getMark()))
	}
	s.r.forward(1)
	minor := s.scanYAMLDirectiveNumber(start)
	if !in(s.r.peek(0), "\x00 \r\n\u0085  ") {
		panic(scannerError("while scanning a directive", start, "expected a digit or '.', but found "+charRepr(s.r.peek(0)), s.r.getMark()))
	}
	s.yamlVersion = version{set: true, major: major, minor: minor}
	return s.yamlVersion
}

func (s *scanner) scanYAMLDirectiveNumber(start *Mark) *big.Int {
	ch := s.r.peek(0)
	if !(ch >= '0' && ch <= '9') {
		panic(scannerError("while scanning a directive", start, "expected a digit, but found "+charRepr(ch), s.r.getMark()))
	}
	length := 0
	for c := s.r.peek(length); c >= '0' && c <= '9'; c = s.r.peek(length) {
		length++
	}
	if length > intMaxStrDigits {
		panic(intDigitsLimitError(length))
	}
	n, _ := new(big.Int).SetString(s.r.prefixString(length), 10)
	s.r.forward(length)
	return n
}

func (s *scanner) scanTagDirectiveValue(start *Mark) (string, string) {
	for s.r.peek(0) == ' ' {
		s.r.forward(1)
	}
	handle := s.scanTagHandle("directive", start)
	if ch := s.r.peek(0); ch != ' ' {
		panic(scannerError("while scanning a directive", start, "expected ' ', but found "+charRepr(ch), s.r.getMark()))
	}
	for s.r.peek(0) == ' ' {
		s.r.forward(1)
	}
	prefix := s.scanTagURI("directive", start)
	if ch := s.r.peek(0); !in(ch, "\x00 \r\n\u0085  ") {
		panic(scannerError("while scanning a directive", start, "expected ' ', but found "+charRepr(ch), s.r.getMark()))
	}
	return handle, prefix
}

func (s *scanner) scanDirectiveIgnoredLine(start *Mark) {
	for s.r.peek(0) == ' ' {
		s.r.forward(1)
	}
	if s.r.peek(0) == '#' {
		for !in(s.r.peek(0), theEnd) {
			s.r.forward(1)
		}
	}
	if ch := s.r.peek(0); !in(ch, theEnd) {
		panic(scannerError("while scanning a directive", start, "expected a comment or a line break, but found "+charRepr(ch), s.r.getMark()))
	}
	s.scanLineBreak(false)
}

// Anchor and alias name characters, the YAML namespace set without flow indicators and the BOM
func checkAnchorNameChar(ch rune) bool {
	if in(ch, ",[]{}") {
		return false
	}
	switch {
	case ch >= 0x21 && ch <= 0x7E, ch >= 0xA0 && ch <= 0xD7FF:
		return true
	case ch >= 0xE000 && ch <= 0xFFFD && ch != 0xFEFF:
		return true
	case ch >= 0x10000 && ch <= 0x10FFFF:
		return true
	}
	return false
}

func (s *scanner) scanAnchor(kind tokenKind) *token {
	start := s.r.getMark()
	name := "anchor"
	if s.r.peek(0) == '*' {
		name = "alias"
	}
	s.r.forward(1)
	length := 0
	ch := s.r.peek(length)
	for checkAnchorNameChar(ch) {
		length++
		ch = s.r.peek(length)
	}
	if length == 0 {
		panic(scannerError("while scanning an "+name, start, "expected alphabetic or numeric character, but found "+charRepr(ch), s.r.getMark()))
	}
	value := pystr.FromRunes(s.r.prefix(length))
	s.r.forward(length)
	if !in(ch, "\x00 \t\r\n\u0085  ?:,[]{}%@`") {
		panic(scannerError("while scanning an "+name, start, "expected alphabetic or numeric character, but found "+charRepr(ch), s.r.getMark()))
	}
	return &token{kind: kind, value: value, start: start, end: s.r.getMark()}
}

func (s *scanner) scanTag() *token {
	start := s.r.getMark()
	ch := s.r.peek(1)
	shortHandle := "!"
	if ch == '!' {
		shortHandle = "!!"
		s.r.forward(1)
		ch = s.r.peek(1)
	}
	var handle *string
	var suffix string
	switch {
	case ch == '<':
		s.r.forward(2)
		suffix = s.scanTagURI("tag", start)
		if s.r.peek(0) != '>' {
			panic(scannerError("while parsing a tag", start, "expected '>' but found "+charRepr(s.r.peek(0)), s.r.getMark()))
		}
		s.r.forward(1)
	case in(ch, theEndSpaceTab):
		suffix = shortHandle
		s.r.forward(1)
	default:
		length := 1
		useHandle := false
		for !in(ch, "\x00 \r\n\u0085  ") {
			if ch == '!' {
				useHandle = true
				break
			}
			length++
			ch = s.r.peek(length)
		}
		if useHandle {
			h := s.scanTagHandle("tag", start)
			handle = &h
		} else {
			h := shortHandle
			handle = &h
			s.r.forward(1)
		}
		suffix = s.scanTagURI("tag", start)
	}
	if ch := s.r.peek(0); !in(ch, "\x00 \r\n\u0085  ") {
		panic(scannerError("while scanning a tag", start, "expected ' ', but found "+charRepr(ch), s.r.getMark()))
	}
	return &token{kind: tkTag, handle: handle, suffix: suffix, start: start, end: s.r.getMark()}
}

func (s *scanner) scanBlockScalar(style rune) *token {
	folded := style == '>'
	var chunks []rune
	start := s.r.getMark()
	s.r.forward(1)
	chomping, increment := s.scanBlockScalarIndicators(start)
	blockScalarComment, hasComment := s.scanBlockScalarIgnoredLine(start)
	minIndent := s.indent + 1
	var breaks []string
	var end *Mark
	var indent int
	if increment == 0 {
		var maxIndent int
		breaks, maxIndent, end = s.scanBlockScalarIndentation()
		indent = max(minIndent, maxIndent)
	} else {
		if minIndent < 1 {
			minIndent = 1
		}
		indent = minIndent + increment - 1
		breaks, end = s.scanBlockScalarBreaks(indent)
	}
	lineBreak := ""
	for s.r.column == indent && s.r.peek(0) != 0 {
		for _, b := range breaks {
			chunks = append(chunks, []rune(b)...)
		}
		leadingNonSpace := !in(s.r.peek(0), " \t")
		length := 0
		for !in(s.r.peek(length), theEnd) {
			length++
		}
		chunks = append(chunks, s.r.prefix(length)...)
		s.r.forward(length)
		lineBreak = s.scanLineBreak(false)
		breaks, end = s.scanBlockScalarBreaks(indent)
		if minIndent == 0 && (s.checkDocumentStart() || s.checkDocumentEnd()) {
			break
		}
		if s.r.column == indent && s.r.peek(0) != 0 {
			if s.rt && folded && lineBreak == "\n" {
				chunks = append(chunks, '\a')
			}
			if folded && lineBreak == "\n" && leadingNonSpace && !in(s.r.peek(0), " \t") {
				if len(breaks) == 0 {
					chunks = append(chunks, ' ')
				}
			} else {
				chunks = append(chunks, []rune(lineBreak)...)
			}
		} else {
			break
		}
	}
	var trailing []string
	if chomping != '-' {
		chunks = append(chunks, []rune(lineBreak)...)
	}
	if chomping == '+' {
		for _, b := range breaks {
			chunks = append(chunks, []rune(b)...)
		}
	} else {
		trailing = append(trailing, breaks...)
	}
	t := &token{kind: tkScalar, value: pystr.FromRunes(chunks), plain: false, start: start, end: end, style: style}
	if hasComment {
		t.addPreComments([]any{blockScalarComment})
	}
	if len(trailing) > 0 {
		c := s.scanToNextToken()
		for c != nil {
			trailing = append(trailing, strings.Repeat(" ", c.start.Column)+c.value)
			c = s.scanToNextToken()
		}
		commentEnd := s.r.getMark()
		t.addPostComment(&token{kind: tkComment, value: strings.Join(trailing, ""), start: end, end: commentEnd})
	}
	return t
}

// Chomping indicator, '+', '-' or zero for none, and the indentation increment, zero for none
func (s *scanner) scanBlockScalarIndicators(start *Mark) (rune, int) {
	var chomping rune
	increment := 0
	ch := s.r.peek(0)
	zeroErr := func() {
		panic(scannerError("while scanning a block scalar", start, "expected indentation indicator in the range 1-9, but found 0", s.r.getMark()))
	}
	if in(ch, "+-") {
		chomping = ch
		s.r.forward(1)
		ch = s.r.peek(0)
		if in(ch, "0123456789") {
			increment = int(ch - '0')
			if increment == 0 {
				zeroErr()
			}
			s.r.forward(1)
		}
	} else if in(ch, "0123456789") {
		increment = int(ch - '0')
		if increment == 0 {
			zeroErr()
		}
		s.r.forward(1)
		ch = s.r.peek(0)
		if in(ch, "+-") {
			chomping = ch
			s.r.forward(1)
		}
	}
	if ch := s.r.peek(0); !in(ch, "\x00 \r\n\u0085  ") {
		panic(scannerError("while scanning a block scalar", start, "expected chomping or indentation indicators, but found "+charRepr(ch), s.r.getMark()))
	}
	return chomping, increment
}

func (s *scanner) scanBlockScalarIgnoredLine(start *Mark) (string, bool) {
	prefix := ""
	comment, has := "", false
	for s.r.peek(0) == ' ' {
		prefix += " "
		s.r.forward(1)
	}
	if s.r.peek(0) == '#' {
		has = true
		cs := []rune(prefix)
		for !in(s.r.peek(0), theEnd) {
			cs = append(cs, s.r.peek(0))
			s.r.forward(1)
		}
		comment = pystr.FromRunes(cs)
	}
	if ch := s.r.peek(0); !in(ch, theEnd) {
		panic(scannerError("while scanning a block scalar", start, "expected a comment or a line break, but found "+charRepr(ch), s.r.getMark()))
	}
	s.scanLineBreak(false)
	return comment, has
}

func (s *scanner) scanBlockScalarIndentation() ([]string, int, *Mark) {
	var chunks []string
	firstIndent, maxIndent := -1, 0
	end := s.r.getMark()
	for in(s.r.peek(0), " \r\n\u0085  ") {
		if s.r.peek(0) != ' ' {
			if firstIndent < 0 {
				firstIndent = s.r.column
			}
			chunks = append(chunks, s.scanLineBreak(false))
			end = s.r.getMark()
		} else {
			s.r.forward(1)
			if s.r.column > maxIndent {
				maxIndent = s.r.column
			}
		}
	}
	if firstIndent > 0 && maxIndent > firstIndent {
		start := s.r.getMark()
		panic(&MarkedError{Class: "ScannerError", Context: strp("more indented follow up line than first in a block scalar"), ContextMark: start})
	}
	return chunks, maxIndent, end
}

func (s *scanner) scanBlockScalarBreaks(indent int) ([]string, *Mark) {
	var chunks []string
	end := s.r.getMark()
	for s.r.column < indent && s.r.peek(0) == ' ' {
		s.r.forward(1)
	}
	for in(s.r.peek(0), "\r\n\u0085  ") {
		chunks = append(chunks, s.scanLineBreak(false))
		end = s.r.getMark()
		for s.r.column < indent && s.r.peek(0) == ' ' {
			s.r.forward(1)
		}
	}
	return chunks, end
}

func (s *scanner) scanFlowScalar(style rune) *token {
	double := style == '"'
	var chunks []rune
	start := s.r.getMark()
	quote := s.r.peek(0)
	s.r.forward(1)
	chunks = append(chunks, s.scanFlowScalarNonSpaces(double, start)...)
	for s.r.peek(0) != quote {
		chunks = append(chunks, s.scanFlowScalarSpaces(double, start)...)
		chunks = append(chunks, s.scanFlowScalarNonSpaces(double, start)...)
	}
	s.r.forward(1)
	return &token{kind: tkScalar, value: pystr.FromRunes(chunks), plain: false, start: start, end: s.r.getMark(), style: style}
}

var escapeReplacements = map[rune]rune{
	'0': 0, 'a': 0x07, 'b': 0x08, 't': 0x09, '\t': 0x09, 'n': 0x0A, 'v': 0x0B, 'f': 0x0C, 'r': 0x0D, 'e': 0x1B,
	' ': 0x20, '"': '"', '/': '/', '\\': '\\', 'N': 0x85, '_': 0xA0, 'L': 0x2028, 'P': 0x2029,
}

var escapeCodes = map[rune]int{'x': 2, 'u': 4, 'U': 8}

func isHexDigit(ch rune) bool { return in(ch, "0123456789ABCDEFabcdef") }

func (s *scanner) scanFlowScalarNonSpaces(double bool, start *Mark) []rune {
	var chunks []rune
	for {
		length := 0
		for !in(s.r.peek(length), " \n'\"\\\x00\t\r\u0085  ") {
			length++
		}
		if length != 0 {
			chunks = append(chunks, s.r.prefix(length)...)
			s.r.forward(length)
		}
		ch := s.r.peek(0)
		switch {
		case !double && ch == '\'' && s.r.peek(1) == '\'':
			chunks = append(chunks, '\'')
			s.r.forward(2)
		case (double && ch == '\'') || (!double && in(ch, "\"\\")):
			chunks = append(chunks, ch)
			s.r.forward(1)
		case double && ch == '\\':
			s.r.forward(1)
			ch = s.r.peek(0)
			if rep, ok := escapeReplacements[ch]; ok {
				chunks = append(chunks, rep)
				s.r.forward(1)
			} else if n, ok := escapeCodes[ch]; ok {
				s.r.forward(1)
				for k := 0; k < n; k++ {
					if !isHexDigit(s.r.peek(k)) {
						panic(scannerError("while scanning a double-quoted scalar", start,
							"expected escape sequence of "+itoa(n)+" hexdecimal numbers, but found "+charRepr(s.r.peek(k)), s.r.getMark()))
					}
				}
				code, _ := new(big.Int).SetString(s.r.prefixString(n), 16)
				if code.Cmp(big.NewInt(0x10FFFF)) > 0 {
					panic(Raise("ValueError", "chr() arg not in range(0x110000)"))
				}
				chunks = append(chunks, rune(code.Int64()))
				s.r.forward(n)
			} else if in(ch, "\n\r\u0085  ") {
				s.scanLineBreak(false)
				for _, b := range s.scanFlowScalarBreaks(double, start) {
					chunks = append(chunks, []rune(b)...)
				}
			} else {
				panic(scannerError("while scanning a double-quoted scalar", start, "found unknown escape character "+charRepr(ch), s.r.getMark()))
			}
		default:
			return chunks
		}
	}
}

func (s *scanner) scanFlowScalarSpaces(double bool, start *Mark) []rune {
	var chunks []rune
	length := 0
	for in(s.r.peek(length), " \t") {
		length++
	}
	whitespaces := s.r.prefix(length)
	s.r.forward(length)
	ch := s.r.peek(0)
	switch {
	case ch == 0:
		panic(scannerError("while scanning a quoted scalar", start, "found unexpected end of stream", s.r.getMark()))
	case in(ch, "\r\n\u0085  "):
		lineBreak := s.scanLineBreak(false)
		breaks := s.scanFlowScalarBreaks(double, start)
		if lineBreak != "\n" {
			chunks = append(chunks, []rune(lineBreak)...)
		} else if len(breaks) == 0 {
			chunks = append(chunks, ' ')
		}
		for _, b := range breaks {
			chunks = append(chunks, []rune(b)...)
		}
	default:
		chunks = append(chunks, whitespaces...)
	}
	return chunks
}

func (s *scanner) scanFlowScalarBreaks(double bool, start *Mark) []string {
	var chunks []string
	for {
		prefix := s.r.prefixString(3)
		if (prefix == "---" || prefix == "...") && in(s.r.peek(3), theEndSpaceTab) {
			panic(scannerError("while scanning a quoted scalar", start, "found unexpected document separator", s.r.getMark()))
		}
		for in(s.r.peek(0), " \t") {
			s.r.forward(1)
		}
		if in(s.r.peek(0), "\r\n\u0085  ") {
			chunks = append(chunks, s.scanLineBreak(false))
		} else {
			return chunks
		}
	}
}

func (s *scanner) scanPlain() *token {
	var chunks []rune
	start := s.r.getMark()
	end := start
	indent := s.indent + 1
	var spaces []string
	spacesSet := true
	for {
		length := 0
		if s.r.peek(0) == '#' {
			break
		}
		var ch rune
		for {
			ch = s.r.peek(length)
			if ch == ':' && !in(s.r.peek(length+1), theEndSpaceTab) {
			} else if ch == '?' && !s.processingVersion().eq(v11) {
			} else if in(ch, theEndSpaceTab) || (s.flowLevel() == 0 && ch == ':' && in(s.r.peek(length+1), theEndSpaceTab)) ||
				(s.flowLevel() > 0 && in(ch, ",:?[]{}")) {
				break
			}
			length++
		}
		if s.flowLevel() > 0 && ch == ':' && !in(s.r.peek(length+1), "\x00 \t\r\n\u0085  ,[]{}") {
			s.r.forward(length)
			e := scannerError("while scanning a plain scalar", start, "found unexpected ':'", s.r.getMark())
			e.Note = "Please check http://pyyaml.org/wiki/YAMLColonInFlowContext for details."
			panic(e)
		}
		if length == 0 {
			break
		}
		s.allowSimpleKey = false
		for _, sp := range spaces {
			chunks = append(chunks, []rune(sp)...)
		}
		chunks = append(chunks, s.r.prefix(length)...)
		s.r.forward(length)
		end = s.r.getMark()
		spaces, spacesSet = s.scanPlainSpaces(indent, start)
		if !spacesSet || len(spaces) == 0 || s.r.peek(0) == '#' || (s.flowLevel() == 0 && s.r.column < indent) {
			break
		}
	}
	t := &token{kind: tkScalar, value: pystr.FromRunes(chunks), plain: true, start: start, end: end}
	if spacesSet && len(spaces) > 0 && spaces[0] == "\n" {
		t.addPostComment(&token{kind: tkComment, value: strings.Join(spaces, "") + "\n", start: start, end: end})
	}
	return t
}

// Folded spaces and breaks after a plain scalar line, false when a document separator ends the scalar
func (s *scanner) scanPlainSpaces(indent int, start *Mark) ([]string, bool) {
	var chunks []string
	length := 0
	for s.r.peek(length) == ' ' {
		length++
	}
	whitespaces := s.r.prefix(length)
	s.r.forward(length)
	ch := s.r.peek(0)
	if in(ch, "\r\n\u0085  ") {
		lineBreak := s.scanLineBreak(false)
		s.allowSimpleKey = true
		prefix := s.r.prefixString(3)
		if (prefix == "---" || prefix == "...") && in(s.r.peek(3), theEndSpaceTab) {
			return nil, false
		}
		var breaks []string
		for in(s.r.peek(0), " \r\n\u0085  ") {
			if s.r.peek(0) == ' ' {
				s.r.forward(1)
			} else {
				breaks = append(breaks, s.scanLineBreak(false))
				prefix = s.r.prefixString(3)
				if (prefix == "---" || prefix == "...") && in(s.r.peek(3), theEndSpaceTab) {
					return nil, false
				}
			}
		}
		if lineBreak != "\n" {
			chunks = append(chunks, lineBreak)
		} else if len(breaks) == 0 {
			chunks = append(chunks, " ")
		}
		chunks = append(chunks, breaks...)
	} else if len(whitespaces) > 0 {
		chunks = append(chunks, string(whitespaces))
	}
	return chunks, true
}

func (s *scanner) scanTagHandle(name string, start *Mark) string {
	ch := s.r.peek(0)
	if ch != '!' {
		panic(scannerError("while scanning an "+name, start, "expected '!', but found "+charRepr(ch), s.r.getMark()))
	}
	length := 1
	ch = s.r.peek(length)
	if ch != ' ' {
		for isAlnumDash(ch, "-_") {
			length++
			ch = s.r.peek(length)
		}
		if ch != '!' {
			s.r.forward(length)
			panic(scannerError("while scanning an "+name, start, "expected '!' but found "+charRepr(ch), s.r.getMark()))
		}
		length++
	}
	value := s.r.prefixString(length)
	s.r.forward(length)
	return value
}

func (s *scanner) scanTagURI(name string, start *Mark) string {
	var chunks []string
	length := 0
	ch := s.r.peek(length)
	for isAlnumDash(ch, "-;/?:@&=+$,_.!~*'()[]%") || (s.processingVersion().greater(v11) && ch == '#') {
		if ch == '%' {
			chunks = append(chunks, pystr.FromRunes(s.r.prefix(length)))
			s.r.forward(length)
			length = 0
			chunks = append(chunks, s.scanURIEscapes(name, start))
		} else {
			length++
		}
		ch = s.r.peek(length)
	}
	if length != 0 {
		chunks = append(chunks, pystr.FromRunes(s.r.prefix(length)))
		s.r.forward(length)
	}
	if len(chunks) == 0 {
		panic(scannerError("while parsing an "+name, start, "expected URI, but found "+charRepr(ch), s.r.getMark()))
	}
	return strings.Join(chunks, "")
}

func (s *scanner) scanURIEscapes(name string, start *Mark) string {
	var codeBytes []byte
	var raw []rune
	mark := s.r.getMark()
	for s.r.peek(0) == '%' {
		raw = append(raw, '%')
		s.r.forward(1)
		for k := 0; k < 2; k++ {
			if !isHexDigit(s.r.peek(k)) {
				panic(scannerError("while scanning an "+name, start, "expected URI escape sequence of 2 hexdecimal numbers, but found "+charRepr(s.r.peek(k)), s.r.getMark()))
			}
		}
		hex := s.r.prefix(2)
		raw = append(raw, hex...)
		v, _ := new(big.Int).SetString(string(hex), 16)
		codeBytes = append(codeBytes, byte(v.Int64()))
		s.r.forward(2)
	}
	text, err := pystr.DecodeUTF8(codeBytes)
	if err != nil {
		panic(scannerError("while scanning an "+name, start, err.Error(), mark))
	}
	if s.rt {
		return string(raw)
	}
	return text
}

func (s *scanner) scanLineBreak(emptyLine bool) string {
	ch := s.r.peek(0)
	switch {
	case in(ch, "\r\n\u0085"):
		if s.r.prefixString(2) == "\r\n" {
			s.r.forward(2)
		} else {
			s.r.forward(1)
		}
		return "\n"
	case in(ch, "  "):
		s.r.forward(1)
		return string(ch)
	case s.rt && emptyLine && in(ch, "\t "):
		s.r.forward(1)
		return string(ch)
	}
	return ""
}
