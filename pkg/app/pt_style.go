package app

import (
	"fmt"
	"image/color"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
)

// ANSI color names of prompt_toolkit style strings in palette order
var ptANSIColorNames = []string{"ansiblack", "ansired", "ansigreen", "ansiyellow", "ansiblue", "ansimagenta", "ansicyan", "ansigray",
	"ansibrightblack", "ansibrightred", "ansibrightgreen", "ansibrightyellow", "ansibrightblue", "ansibrightmagenta", "ansibrightcyan", "ansiwhite"}

// Older spellings of the ANSI color names
var ptANSIColorAliases = map[string]string{"ansidarkgray": "ansibrightblack", "ansiteal": "ansicyan", "ansiturquoise": "ansibrightcyan",
	"ansibrown": "ansiyellow", "ansipurple": "ansimagenta", "ansifuchsia": "ansibrightmagenta", "ansilightgray": "ansigray",
	"ansidarkred": "ansired", "ansidarkgreen": "ansigreen", "ansidarkblue": "ansiblue"}

// ptAttrs is a resolved text style, nil fields inheriting
type ptAttrs struct {
	color, bgcolor                                               *string
	bold, underline, strike, italic, blink, reverse, hidden, dim *bool
}

// Index of an ANSI color name, minus one when it is not one
func ptANSIColorIndex(name string) int {
	for i, n := range ptANSIColorNames {
		if n == name {
			return i
		}
	}
	return -1
}

// Normalizes a color to an ANSI name, six hex digits, default or empty
func ptParseColor(text string) (string, error) {
	if text == "ansidefault" || ptANSIColorIndex(text) >= 0 {
		return text, nil
	}
	if alias, ok := ptANSIColorAliases[text]; ok {
		return alias, nil
	}
	if hex, ok := ptNamedColors[strings.ToLower(text)]; ok {
		return strings.TrimLeft(hex, "#"), nil
	}
	if strings.HasPrefix(text, "#") {
		col := text[1:]
		if col == "ansidefault" || ptANSIColorIndex(col) >= 0 {
			return col, nil
		}
		if alias, ok := ptANSIColorAliases[col]; ok {
			return alias, nil
		}
		if len(col) == 6 {
			return col, nil
		}
		if len(col) == 3 {
			return string([]byte{col[0], col[0], col[1], col[1], col[2], col[2]}), nil
		}
	} else if text == "" || text == "default" {
		return text, nil
	}
	return "", fmt.Errorf("Wrong color format %q", text)
}

// Parses one style string into attributes, class parts ignored
func ptParseStyleStr(styleStr string) (ptAttrs, error) {
	var a ptAttrs
	yes, no := true, false
	if strings.Contains(styleStr, "noinherit") {
		empty := ""
		a = ptAttrs{&empty, &empty, &no, &no, &no, &no, &no, &no, &no, &no}
	}
	flags := map[string]**bool{"bold": &a.bold, "italic": &a.italic, "underline": &a.underline, "strike": &a.strike,
		"blink": &a.blink, "reverse": &a.reverse, "hidden": &a.hidden, "dim": &a.dim}
	for _, part := range strings.Fields(styleStr) {
		switch {
		case part == "noinherit", part == "roman", part == "sans", part == "mono", strings.HasPrefix(part, "border:"),
			strings.HasPrefix(part, "[") && strings.HasSuffix(part, "]"), strings.HasPrefix(part, "class:"):
		case flags[part] != nil:
			*flags[part] = &yes
		case strings.HasPrefix(part, "no") && flags[part[2:]] != nil:
			*flags[part[2:]] = &no
		case strings.HasPrefix(part, "bg:"):
			c, err := ptParseColor(part[3:])
			if err != nil {
				return a, err
			}
			a.bgcolor = &c
		default:
			c, err := ptParseColor(strings.TrimPrefix(part, "fg:"))
			if err != nil {
				return a, err
			}
			a.color = &c
		}
	}
	return a, nil
}

// Layers attributes, later set fields winning
func ptMergeAttrs(list ...ptAttrs) ptAttrs {
	var out ptAttrs
	for _, a := range list {
		if a.color != nil {
			out.color = a.color
		}
		if a.bgcolor != nil {
			out.bgcolor = a.bgcolor
		}
		for _, f := range [][2]**bool{{&out.bold, &a.bold}, {&out.underline, &a.underline}, {&out.strike, &a.strike}, {&out.italic, &a.italic},
			{&out.blink, &a.blink}, {&out.reverse, &a.reverse}, {&out.hidden, &a.hidden}, {&out.dim, &a.dim}} {
			if *f[1] != nil {
				*f[0] = *f[1]
			}
		}
	}
	return out
}

// ptStyleRule applies attributes to elements carrying every one of its class names
type ptStyleRule struct {
	names map[string]bool
	attrs ptAttrs
}

// ptStyle is an ordered rule list, later rules winning
type ptStyle struct {
	rules []ptStyleRule
}

// Builds a style from class name and style string pairs in order
func newPTStyle(rules [][2]string) (*ptStyle, error) {
	s := &ptStyle{}
	for _, r := range rules {
		attrs, err := ptParseStyleStr(r[1])
		if err != nil {
			return nil, err
		}
		names := map[string]bool{}
		for _, n := range strings.Fields(strings.ToLower(r[0])) {
			names[n] = true
		}
		s.rules = append(s.rules, ptStyleRule{names, attrs})
	}
	return s, nil
}

// styleDict is an insertion ordered class name to style string mapping
type styleDict struct {
	keys   []string
	values map[string]string
}

// Sets a key, a new key appended at the end
func (d *styleDict) set(key, value string) {
	if d.values == nil {
		d.values = map[string]string{}
	}
	if _, ok := d.values[key]; !ok {
		d.keys = append(d.keys, key)
	}
	d.values[key] = value
}

// Sets every pair of a map, new keys appended in sorted order
func (d *styleDict) update(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d.set(k, m[k])
	}
}

// Copy sharing nothing with the original
func (d *styleDict) clone() *styleDict {
	out := &styleDict{}
	if d != nil {
		for _, k := range d.keys {
			out.set(k, d.values[k])
		}
	}
	return out
}

// Builds a style from a dict in key order
func ptStyleFromDict(d *styleDict) (*ptStyle, error) {
	rules := make([][2]string, 0, len(d.keys))
	for _, k := range d.keys {
		rules = append(rules, [2]string{k, d.values[k]})
	}
	return newPTStyle(rules)
}

// Dotted prefixes of a class name, shortest first
func ptExpandClassName(name string) []string {
	parts := strings.Split(name, ".")
	out := make([]string, 0, len(parts))
	for i := 1; i <= len(parts); i++ {
		out = append(out, strings.Join(parts[:i], "."))
	}
	return out
}

// Attributes this style gives an element style string
func (s *ptStyle) attrsFor(styleStr string) ptAttrs {
	var list []ptAttrs
	have := map[string]bool{}
	for _, r := range s.rules {
		if len(r.names) == 0 {
			list = append(list, r.attrs)
		}
	}
	for _, part := range strings.Fields(styleStr) {
		if !strings.HasPrefix(part, "class:") {
			if inline, err := ptParseStyleStr(part); err == nil {
				list = append(list, inline)
			}
			continue
		}
		var added []string
		for _, p := range strings.Split(strings.ToLower(part[6:]), ",") {
			added = append(added, ptExpandClassName(p)...)
		}
		for _, name := range added {
			for _, r := range s.rules {
				if !r.names[name] {
					continue
				}
				match := true
				for n := range r.names {
					if n != name && !have[n] {
						match = false
						break
					}
				}
				if match {
					list = append(list, r.attrs)
				}
			}
			have[name] = true
		}
	}
	return ptMergeAttrs(list...)
}

// ptMergedStyle resolves a style string through several styles, later styles winning
type ptMergedStyle struct {
	styles []*ptStyle
	cache  map[string]lipgloss.Style
}

// Attributes of a style string across every merged style
func (m *ptMergedStyle) attrsFor(styleStr string) ptAttrs {
	list := make([]ptAttrs, 0, len(m.styles))
	for _, s := range m.styles {
		list = append(list, s.attrsFor(styleStr))
	}
	return ptMergeAttrs(list...)
}

// Terminal color of a normalized color value, nil for the terminal default
func ptColor(value *string) color.Color {
	if value == nil || *value == "" || *value == "default" || *value == "ansidefault" {
		return nil
	}
	if idx := ptANSIColorIndex(*value); idx >= 0 {
		return lipgloss.Color(fmt.Sprint(idx))
	}
	return lipgloss.Color("#" + *value)
}

// Renderable style of a style string, memoized per string
func (m *ptMergedStyle) render(styleStr string) lipgloss.Style {
	if st, ok := m.cache[styleStr]; ok {
		return st
	}
	a := m.attrsFor(styleStr)
	st := lipgloss.NewStyle()
	if c := ptColor(a.color); c != nil {
		st = st.Foreground(c)
	}
	if c := ptColor(a.bgcolor); c != nil {
		st = st.Background(c)
	}
	on := func(b *bool) bool { return b != nil && *b }
	st = st.Bold(on(a.bold)).Italic(on(a.italic)).Underline(on(a.underline)).Strikethrough(on(a.strike)).Blink(on(a.blink)).Reverse(on(a.reverse)).Faint(on(a.dim))
	if m.cache == nil {
		m.cache = map[string]lipgloss.Style{}
	}
	m.cache[styleStr] = st
	return st
}

// Reports whether a style string resolves to any color or attribute
func (m *ptMergedStyle) hasStyle(styleStr string) bool {
	a := m.attrsFor(styleStr)
	on := func(b *bool) bool { return b != nil && *b }
	return ptColor(a.color) != nil || ptColor(a.bgcolor) != nil || on(a.bold) || on(a.underline) || on(a.strike) ||
		on(a.italic) || on(a.blink) || on(a.reverse) || on(a.hidden) || on(a.dim)
}

// Merges the prompt_toolkit default interface style under a user style
func newPTMergedStyle(user *ptStyle) *ptMergedStyle {
	base, err := newPTStyle(ptDefaultUIStyleRules)
	if err != nil {
		panic(err)
	}
	return &ptMergedStyle{styles: []*ptStyle{base, user}}
}
