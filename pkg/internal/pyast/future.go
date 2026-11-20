package pyast

import (
	"strings"
	"unicode/utf8"
)

// srcLoc is a _Py_SourceLocation, a line and byte column range of the source
type srcLoc struct{ lineno, endLineno, col, endCol int }

var noLocation = srcLoc{-1, -1, -1, -1}

// Location of a node that carries position attributes as the LOCATION macro reads it
func locOf(n *Node) srcLoc {
	return srcLoc{n.Lineno, n.EndLineno, n.ColOffset, n.EndColOffset}
}

// futureInfo is _PyFutureFeatures, the flags of the leading future statements and the location of the last one
type futureInfo struct {
	annotations bool
	loc         srcLoc
}

// Reads the leading from __future__ imports as future_parse does, failing on braces and unknown features
func futureParse(mod *Node, filename string) (futureInfo, error) {
	ff := futureInfo{loc: noLocation}
	if !mod.Is("Module", "Interactive") {
		return ff, nil
	}
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
				return ff, futureError("not a chance", a, filename)
			default:
				return ff, futureError("future feature "+firstBytes(feature, 100)+" is not defined", a, filename)
			}
		}
		ff.loc = locOf(s)
	}
	return ff, nil
}

// Truncates text to n bytes short of a split UTF-8 sequence as the %.100s conversion of PyErr_Format does
func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// Reports whether the leading future statements enable annotations, with the error of an unknown feature
func futureFeatures(mod *Node, filename string) (annotations bool, err error) {
	ff, err := futureParse(mod, filename)
	if err != nil {
		return false, err
	}
	return ff.annotations, nil
}

// Builds the SyntaxError PyErr_RangedSyntaxLocationObject raises at an alias, quoting the line of the named file
func futureError(msg string, a *Node, filename string) error {
	return rangedSyntaxError(msg, locOf(a), filename)
}

// Builds a SyntaxError located by PyErr_RangedSyntaxLocationObject with one based columns and the line of the named file
func rangedSyntaxError(msg string, loc srcLoc, filename string) *SyntaxError {
	se := &SyntaxError{Class: "SyntaxError", Msg: msg, Lineno: loc.lineno, Offset: loc.col + 1, EndLineno: loc.endLineno, EndOffset: loc.endCol + 1, HasLineno: true, HasEnd: true, Filename: filename}
	if text, ok := programText(filename, loc.lineno, ""); ok {
		se.Text, se.HasText = strings.TrimSuffix(text, "\n"), true
	}
	return se
}

// Reports whether the first statement is an expression statement holding a str constant like _PyAST_GetDocString
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
