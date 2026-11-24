package pythonutil

import (
	"errors"
	"unicode/utf8"
)

// A source span: 1-based lines and 0-based UTF-8 byte columns
type srcLoc struct{ lineno, endLineno, col, endCol int }

var noLocation = srcLoc{-1, -1, -1, -1}

func locOf(n *Node) srcLoc {
	return srcLoc{n.Line, n.EndLine, n.Col, n.EndCol}
}

// Features the leading from __future__ imports turn on and where the last of them sits
type futureInfo struct {
	annotations bool
	loc         srcLoc
}

// Reads the from __future__ imports that open a module, failing on braces and on features Python does not define
func futureParse(mod *Node, lx *lexer, filename string) (futureInfo, error) {
	ff := futureInfo{loc: noLocation}
	body := mod.Children("body")
	i := 0
	if docstringOf(body) {
		i++
	}
	for ; i < len(body); i++ {
		s := body[i]
		if !s.Is("ImportFrom") {
			return ff, nil
		}
		if level, _ := s.Int("level"); level != 0 {
			return ff, nil
		}
		if name, ok := s.Str("module"); !ok || name != "__future__" {
			return ff, nil
		}
		for _, a := range s.Children("names") {
			feature, _ := a.Str("name")
			switch feature {
			case "nested_scopes", "generators", "division", "absolute_import", "with_statement", "print_function", "unicode_literals", "barry_as_FLUFL", "generator_stop":
			case "annotations":
				ff.annotations = true
			case "braces":
				return ff, rangedSyntaxError("not a chance", locOf(a), lx, filename)
			default:
				return ff, rangedSyntaxError("future feature "+firstBytes(feature, 100)+" is not defined", locOf(a), lx, filename)
			}
		}
		ff.loc = locOf(s)
	}
	return ff, nil
}

// Truncates text to at most n bytes without splitting a UTF-8 sequence
func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// SyntaxError a compile stage raises over a source span, quoting the source line with character columns
func rangedSyntaxError(msg string, loc srcLoc, lx *lexer, filename string) *SyntaxError {
	text := lx.textOfLine(loc.lineno)
	se := &SyntaxError{Msg: msg, Line: loc.lineno, Column: byteToCharRaw(text, loc.col+1), EndLine: loc.endLineno, EndColumn: byteToCharRaw(lx.textOfLine(loc.endLineno), loc.endCol+1), Text: text}
	return se.finish(filename)
}

// Error for broken bookkeeping inside the compile checks, which no source should be able to cause
func internalError(msg string) error { return errors.New("python compile check: " + msg) }

// Whether the first statement is an expression statement holding a string constant, a docstring
func docstringOf(body []*Node) bool {
	if len(body) == 0 || !body[0].Is("Expr") {
		return false
	}
	v := body[0].Child("value")
	if !v.Is("Constant") {
		return false
	}
	_, ok := v.Get("value").(string)
	return ok
}
