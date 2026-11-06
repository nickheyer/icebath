package pyrich

import (
	"sort"
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
)

type textSpan struct {
	start, end int
	style      string
}

// Patterns of rich.highlighter.ReprHighlighter, each group naming the repr theme style of its text
var reprHighlights = []string{
	`(?P<tag_start><)(?P<tag_name>[-\w.:|]*)(?P<tag_contents>[\w\W]*)(?P<tag_end>>)`,
	`(?P<attrib_name>[\w_]{1,50})=(?P<attrib_value>"?[\w_]+"?)?`,
	`(?P<brace>[][{}()])`,
	strings.Join([]string{
		`(?P<ipv4>[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3})`,
		`(?P<ipv6>([A-Fa-f0-9]{1,4}::?){1,7}[A-Fa-f0-9]{1,4})`,
		`(?P<eui64>(?:[0-9A-Fa-f]{1,2}-){7}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{1,2}:){7}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{4}\.){3}[0-9A-Fa-f]{4})`,
		`(?P<eui48>(?:[0-9A-Fa-f]{1,2}-){5}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{1,2}:){5}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{4}\.){2}[0-9A-Fa-f]{4})`,
		`(?P<uuid>[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12})`,
		`(?P<call>[\w.]*?)\(`,
		`\b(?P<bool_true>True)\b|\b(?P<bool_false>False)\b|\b(?P<none>None)\b`,
		`(?P<ellipsis>\.\.\.)`,
		`(?P<number_complex>(?<!\w)(?:\-?[0-9]+\.?[0-9]*(?:e[-+]?\d+?)?)(?:[-+](?:[0-9]+\.?[0-9]*(?:e[-+]?\d+)?))?j)`,
		`(?P<number>(?<!\w)\-?[0-9]+\.?[0-9]*(e[-+]?\d+?)?\b|0x[0-9a-fA-F]*)`,
		`(?P<path>\B(/[-\w._+]+)*\/)(?P<filename>[-\w._+]*)?`,
		`(?<![\\\w])(?P<str>b?'''.*?(?<!\\)'''|b?'.*?(?<!\\)'|b?\"\"\".*?(?<!\\)\"\"\"|b?\".*?(?<!\\)\")`,
		"(?P<url>(file|https|http|ws|wss)://[-0-9a-zA-Z$_+!`(),.?/;:&=%#~@]*)",
	}, "|"),
}

type namedPattern struct {
	re    *regexp2.Regexp
	names []string
	nums  []int
}

var compiledRepr = sync.OnceValue(func() []namedPattern {
	out := make([]namedPattern, 0, len(reprHighlights))
	for _, p := range reprHighlights {
		re, err := pyre.Compile(p)
		if err != nil {
			panic(err)
		}
		index, err := pyre.GroupIndex(p)
		if err != nil {
			panic(err)
		}
		np := namedPattern{re: re}
		for name := range index {
			np.names = append(np.names, name)
		}
		sort.Slice(np.names, func(i, j int) bool { return index[np.names[i]] < index[np.names[j]] })
		for _, name := range np.names {
			np.nums = append(np.nums, index[name])
		}
		out = append(out, np)
	}
	return out
})

// Spans ReprHighlighter adds to plain text in pattern then group order, rune offsets with repr prefixed style names
func reprSpans(plain string) []textSpan {
	var spans []textSpan
	for _, np := range compiledRepr() {
		m, err := np.re.FindStringMatch(plain)
		for err == nil && m != nil {
			for i, num := range np.nums {
				g := m.GroupByNumber(num)
				if g != nil && len(g.Captures) > 0 && g.RuneLength > 0 {
					spans = append(spans, textSpan{g.RuneIndex, g.RuneIndex + g.RuneLength, "repr." + np.names[i]})
				}
			}
			m, err = np.re.FindNextMatch(m)
		}
		if err != nil {
			panic(err)
		}
	}
	return spans
}

// Style a console resolves a style name to, theme first, then a parsed definition, the null style when neither works
func getStyle(name string) Style {
	if st, ok := defaultStyles[name]; ok {
		return st
	}
	st, err := ParseStyle(name)
	if err != nil {
		return Style{}
	}
	return st
}

// Plain text cut at every span edge, each piece rendered with its spans combined in list order, as Text.render
func renderSpans(plain string, spans []textSpan, system string) string {
	if len(spans) == 0 {
		return plain
	}
	runes := []rune(plain)
	type edge struct {
		offset  int
		leaving bool
		id      int
	}
	edges := []edge{{0, false, 0}}
	styles := make([]Style, len(spans)+1)
	for i, sp := range spans {
		styles[i+1] = getStyle(sp.style)
		edges = append(edges, edge{sp.start, false, i + 1})
	}
	for i, sp := range spans {
		edges = append(edges, edge{sp.end, true, i + 1})
	}
	edges = append(edges, edge{len(runes), true, 0})
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].offset != edges[j].offset {
			return edges[i].offset < edges[j].offset
		}
		return !edges[i].leaving && edges[j].leaving
	})
	var b strings.Builder
	var stack []int
	for k := 0; k+1 < len(edges); k++ {
		e := edges[k]
		if e.leaving {
			for i := len(stack) - 1; i >= 0; i-- {
				if stack[i] == e.id {
					stack = append(stack[:i], stack[i+1:]...)
					break
				}
			}
		} else {
			stack = append(stack, e.id)
		}
		next := edges[k+1].offset
		if next <= e.offset {
			continue
		}
		ids := append([]int(nil), stack...)
		sort.Ints(ids)
		var st Style
		for _, id := range ids {
			st = st.Add(styles[id])
		}
		b.WriteString(st.Render(string(runes[e.offset:next]), system))
	}
	return b.String()
}
