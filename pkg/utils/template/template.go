package templateutil

import (
	"slices"
	"strings"
)

// Text with {name} and {name:spec} placeholders, parsed once and safe to render from many goroutines
type Template struct {
	parts  []part
	fields []string
	size   int
}

// Literal text, or a placeholder whose spec is parsed already or holds nested placeholders filled in when rendering
type part struct {
	text   string
	field  bool
	spec   spec
	nested string
}

// Template parsed from text where {name} and {name:spec} are placeholders and {{ and }} stand for literal braces
func Parse(text string) (*Template, error) {
	t := &Template{}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, part{text: lit.String()})
			t.size += lit.Len()
			lit.Reset()
		}
	}
	for i := 0; i < len(text); {
		switch text[i] {
		case '{':
			if i+1 < len(text) && text[i+1] == '{' {
				lit.WriteByte('{')
				i += 2
				continue
			}
			end, err := closing(text, i)
			if err != nil {
				return nil, err
			}
			p, names, err := parseField(text[i+1:end], i)
			if err != nil {
				return nil, err
			}
			flush()
			t.parts = append(t.parts, p)
			for _, name := range names {
				if !slices.Contains(t.fields, name) {
					t.fields = append(t.fields, name)
				}
			}
			i = end + 1
		case '}':
			if i+1 < len(text) && text[i+1] == '}' {
				lit.WriteByte('}')
				i += 2
				continue
			}
			return nil, &SyntaxError{Offset: i, Msg: "single } outside a placeholder"}
		default:
			j := i
			for j < len(text) && text[j] != '{' && text[j] != '}' {
				j++
			}
			lit.WriteString(text[i:j])
			i = j
		}
	}
	flush()
	return t, nil
}

// Names of the placeholders in order of first appearance, nested placeholders included; shared slice, callers must not modify it
func (t *Template) Fields() []string { return t.fields }

// Text with each placeholder replaced by the value lookup gives for its name, formatted under its spec
func (t *Template) Render(lookup func(name string) (any, bool)) (string, error) {
	var b strings.Builder
	b.Grow(t.size + 16*len(t.fields))
	for _, p := range t.parts {
		if !p.field {
			b.WriteString(p.text)
			continue
		}
		v, ok := lookup(p.text)
		if !ok {
			return "", &MissingError{Name: p.text}
		}
		sp, specText := p.spec, ""
		if p.nested != "" {
			var err error
			if specText, err = fillNested(p.nested, lookup); err != nil {
				return "", err
			}
			if sp, err = parseSpec(specText); err != nil {
				return "", &SpecError{Name: p.text, Spec: specText, Msg: err.Error()}
			}
		}
		text, err := sp.apply(v)
		if err != nil {
			if specText == "" {
				specText = sp.source
			}
			return "", &SpecError{Name: p.text, Spec: specText, Msg: err.Error()}
		}
		b.WriteString(text)
	}
	return b.String(), nil
}

// Text of a template with each placeholder replaced by its value in values
func Format(text string, values map[string]any) (string, error) {
	t, err := Parse(text)
	if err != nil {
		return "", err
	}
	return t.Render(func(name string) (any, bool) {
		v, ok := values[name]
		return v, ok
	})
}

// Placeholder names of a template in order of first appearance, nested placeholders included
func Fields(text string) ([]string, error) {
	t, err := Parse(text)
	if err != nil {
		return nil, err
	}
	return t.fields, nil
}

// Offset of the brace closing the placeholder opened at start, allowing placeholders one level deep inside its spec
func closing(text string, start int) (int, error) {
	depth := 1
	for i := start + 1; i < len(text); i++ {
		switch text[i] {
		case '{':
			if depth++; depth > 2 {
				return 0, &SyntaxError{Offset: i, Msg: "placeholders nest only one level deep"}
			}
		case '}':
			if depth--; depth == 0 {
				return i, nil
			}
		}
	}
	return 0, &SyntaxError{Offset: start, Msg: "unclosed {"}
}

// Placeholder of a field body, its name, spec and the names it reads; offset places errors in the whole text
func parseField(body string, offset int) (part, []string, error) {
	name, specText, _ := strings.Cut(body, ":")
	if name == "" || strings.ContainsAny(name, "{}") {
		return part{}, nil, &SyntaxError{Offset: offset, Msg: "placeholder without a name"}
	}
	p := part{text: name, field: true}
	names := []string{name}
	if !strings.Contains(specText, "{") {
		sp, err := parseSpec(specText)
		if err != nil {
			return part{}, nil, &SyntaxError{Offset: offset, Msg: "placeholder {" + name + "}: " + err.Error()}
		}
		p.spec = sp
		return p, names, nil
	}
	rest := specText
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			break
		}
		end := strings.IndexByte(rest[open:], '}') + open
		inner := rest[open+1 : end]
		if inner == "" || strings.ContainsAny(inner, ":{") {
			return part{}, nil, &SyntaxError{Offset: offset, Msg: "placeholder {" + name + "}: a nested placeholder is a bare {name}"}
		}
		names = append(names, inner)
		rest = rest[end+1:]
	}
	p.nested = specText
	return p, names, nil
}

// Spec text with each nested {name} replaced by the text of its value
func fillNested(specText string, lookup func(name string) (any, bool)) (string, error) {
	var b strings.Builder
	for {
		open := strings.IndexByte(specText, '{')
		if open < 0 {
			b.WriteString(specText)
			return b.String(), nil
		}
		end := strings.IndexByte(specText[open:], '}') + open
		name := specText[open+1 : end]
		v, ok := lookup(name)
		if !ok {
			return "", &MissingError{Name: name}
		}
		b.WriteString(specText[:open])
		b.WriteString(valueText(v))
		specText = specText[end+1:]
	}
}
