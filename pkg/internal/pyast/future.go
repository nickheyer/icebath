package pyast

import "strings"

// Reads the leading from __future__ imports as future_parse does, failing on braces and unknown features
func futureFeatures(mod *Node, filename string) (annotations bool, err error) {
	if !mod.Is("Module", "Interactive") {
		return false, nil
	}
	body := mod.Children("body")
	i := 0
	if docstringOf(body) {
		i++
	}
	for ; i < len(body); i++ {
		s := body[i]
		if !s.Is("ImportFrom") {
			return annotations, nil
		}
		if level, _ := s.Int("level"); level != 0 {
			return annotations, nil
		}
		if name, ok := s.Str("module"); !ok || name != "__future__" {
			return annotations, nil
		}
		for _, a := range s.Children("names") {
			feature, _ := a.Str("name")
			switch feature {
			case "nested_scopes", "generators", "division", "absolute_import", "with_statement", "print_function", "unicode_literals", "barry_as_FLUFL", "generator_stop":
			case "annotations":
				annotations = true
			case "braces":
				return false, futureError("not a chance", a, filename)
			default:
				return false, futureError("future feature "+feature+" is not defined", a, filename)
			}
		}
	}
	return annotations, nil
}

// Builds the SyntaxError PyErr_RangedSyntaxLocationObject raises at an alias, quoting the line of the named file
func futureError(msg string, a *Node, filename string) error {
	se := &SyntaxError{Class: "SyntaxError", Msg: msg, Lineno: a.Lineno, Offset: a.ColOffset + 1, EndLineno: a.EndLineno, EndOffset: a.EndColOffset + 1, HasLineno: true, HasEnd: true, Filename: filename}
	if text, ok := programText(filename, a.Lineno, ""); ok {
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
