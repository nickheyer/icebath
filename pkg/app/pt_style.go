package app

import (
	"fmt"
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

// ptMergedStyle resolves a style string through several styles, later styles winning
type ptMergedStyle struct {
	styles []*ptStyle
	cache  map[string]lipgloss.Style
}

// Merges the prompt_toolkit default interface style under a user style
func newPTMergedStyle(user *ptStyle) *ptMergedStyle {
	base, err := newPTStyle(ptDefaultUIStyleRules)
	if err != nil {
		panic(err)
	}
	return &ptMergedStyle{styles: []*ptStyle{base, user}}
}
