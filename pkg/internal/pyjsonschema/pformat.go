package pyjsonschema

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const pformatWidth = 72

// pformat from a caller st sits in, pformat, PrettyPrinter.pformat and the recursive pprint frames under it
func pformatAt(v any, st *state) string {
	defer st.call()()
	st.touch(1)
	defer st.call()()
	var b strings.Builder
	pformatValue(st, &b, v, 0, 0, 0)
	return b.String()
}

// Frames _safe_repr reaches for a value, one per call and one more for format at every container level
func safeReprDepth(v any) int {
	deepest := 0
	switch x := v.(type) {
	case *pyjson.Object:
		for _, k := range x.Keys {
			deepest = max(deepest, 1+max(safeReprDepth(k), safeReprDepth(x.Vals[k])))
		}
	case []any:
		for _, item := range x {
			deepest = max(deepest, 1+safeReprDepth(item))
		}
	}
	return 1 + deepest
}

// _repr of a value, _repr and format above the _safe_repr recursion
func reprFrames(st *state, v any) string {
	st.touch(2 + safeReprDepth(v))
	return repr(v)
}

func pformatValue(st *state, b *strings.Builder, v any, indent, allowance, level int) {
	defer st.call()()
	rep := reprFrames(st, v)
	if pystr.Len(rep) > pformatWidth-indent-allowance {
		switch x := v.(type) {
		case *pyjson.Object:
			pformatDict(st, b, x, indent, allowance, level+1)
			return
		case []any:
			pformatList(st, b, x, indent, allowance, level+1)
			return
		case string:
			pformatStr(st, b, x, rep, indent, allowance, level+1)
			return
		}
	}
	b.WriteString(rep)
}

func pformatDict(st *state, b *strings.Builder, d *pyjson.Object, indent, allowance, level int) {
	defer st.call()()
	b.WriteByte('{')
	if len(d.Keys) > 0 {
		defer st.call()()
		allowance++
		indent++
		delim := ",\n" + strings.Repeat(" ", indent)
		for i, k := range d.Keys {
			last := i == len(d.Keys)-1
			rep := reprFrames(st, k)
			b.WriteString(rep + ": ")
			a := 1
			if last {
				a = allowance
			}
			pformatValue(st, b, d.Vals[k], indent+pystr.Len(rep)+2, a, level)
			if !last {
				b.WriteString(delim)
			}
		}
	}
	b.WriteByte('}')
}

func pformatList(st *state, b *strings.Builder, items []any, indent, allowance, level int) {
	defer st.call()()
	defer st.call()()
	b.WriteByte('[')
	allowance++
	indent++
	delim := ",\n" + strings.Repeat(" ", indent)
	for i, item := range items {
		if i > 0 {
			b.WriteString(delim)
		}
		a := 1
		if i == len(items)-1 {
			a = allowance
		}
		pformatValue(st, b, item, indent, a, level)
	}
	b.WriteByte(']')
}

// Long string split into word chunks, parenthesized at the top level the way pprint wraps it
func pformatStr(st *state, b *strings.Builder, s, rep string, indent, allowance, level int) {
	defer st.call()()
	if s == "" {
		b.WriteString(rep)
		return
	}
	lines := pystr.Splitlines(s, true)
	if level == 1 {
		indent++
		allowance++
	}
	maxWidth := pformatWidth - indent
	maxWidth1 := maxWidth
	var chunks []string
	for i, line := range lines {
		rep = repr(line)
		if i == len(lines)-1 {
			maxWidth1 -= allowance
		}
		if pystr.Len(rep) <= maxWidth1 {
			chunks = append(chunks, rep)
			continue
		}
		st.touch(2)
		parts := wordParts(line)
		maxWidth2 := maxWidth
		current := ""
		for j, part := range parts {
			candidate := current + part
			if j == len(parts)-1 && i == len(lines)-1 {
				maxWidth2 -= allowance
			}
			if pystr.Len(repr(candidate)) > maxWidth2 {
				if current != "" {
					chunks = append(chunks, repr(current))
				}
				current = part
			} else {
				current = candidate
			}
		}
		if current != "" {
			chunks = append(chunks, repr(current))
		}
	}
	if len(chunks) == 1 {
		b.WriteString(rep)
		return
	}
	if level == 1 {
		b.WriteByte('(')
	}
	for i, c := range chunks {
		if i > 0 {
			b.WriteString("\n" + strings.Repeat(" ", indent))
		}
		b.WriteString(c)
	}
	if level == 1 {
		b.WriteByte(')')
	}
}

// Alternating non space then space runs re.findall of \S*\s* yields without its empty tail
func wordParts(line string) []string {
	var parts []string
	rs := pystr.Runes(line)
	for i := 0; i < len(rs); {
		j := i
		for j < len(rs) && !pystr.IsSpace(rs[j]) {
			j++
		}
		for j < len(rs) && pystr.IsSpace(rs[j]) {
			j++
		}
		parts = append(parts, pystr.FromRunes(rs[i:j]))
		i = j
	}
	return parts
}
