package consoleutil

import (
	"strings"
	"sync"

	"github.com/dlclark/regexp2/v2"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
)

// Named group of a highlight pattern and the style its text draws in
type highlightGroup struct {
	number int
	style  Style
}

type highlightPattern struct {
	re     *regexp2.Regexp
	groups []highlightGroup
}

// Patterns of the literals printed text is colored by, compiled on first use: tags, key=value pairs, brackets, then addresses, identifiers, calls, Go literals, numbers, paths, quoted strings and URLs
var highlightPatterns = sync.OnceValue(func() []highlightPattern {
	bold := Style{on: attrBold}
	brightGreenBold := Style{fg: basicColor(10), on: attrBold}
	styles := map[string]Style{
		"tag_start":    bold,
		"tag_name":     {fg: basicColor(13), on: attrBold},
		"tag_contents": {fg: colorSpec{kind: colorDefault}},
		"tag_end":      bold,
		"attrib_name":  {fg: basicColor(3), off: attrItalic},
		"attrib_value": {fg: basicColor(5), off: attrItalic},
		"brace":        bold,
		"ipv4":         brightGreenBold,
		"ipv6":         brightGreenBold,
		"eui48":        brightGreenBold,
		"eui64":        brightGreenBold,
		"uuid":         {fg: basicColor(11), off: attrBold},
		"call":         {fg: basicColor(5), on: attrBold},
		"bool_true":    {fg: basicColor(10), on: attrItalic},
		"bool_false":   {fg: basicColor(9), on: attrItalic},
		"nil":          {fg: basicColor(5), on: attrItalic},
		"ellipsis":     {fg: basicColor(3)},
		"number":       {fg: basicColor(6), on: attrBold, off: attrItalic},
		"path":         {fg: basicColor(5)},
		"filename":     {fg: basicColor(13)},
		"str":          {fg: basicColor(2), off: attrBold | attrItalic},
		"url":          {fg: basicColor(12), on: attrUnderline, off: attrBold | attrItalic},
	}
	sources := []string{
		`(?P<tag_start><)(?P<tag_name>[-\w.:|]*)(?P<tag_contents>[\w\W]*)(?P<tag_end>>)`,
		`(?P<attrib_name>[\w_]{1,50})=(?P<attrib_value>"?[\w_]+"?)?`,
		`(?P<brace>[\]\[{}()])`,
		strings.Join([]string{
			`(?P<ipv4>[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3})`,
			`(?P<ipv6>([A-Fa-f0-9]{1,4}::?){1,7}[A-Fa-f0-9]{1,4})`,
			`(?P<eui64>(?:[0-9A-Fa-f]{1,2}-){7}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{1,2}:){7}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{4}\.){3}[0-9A-Fa-f]{4})`,
			`(?P<eui48>(?:[0-9A-Fa-f]{1,2}-){5}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{1,2}:){5}[0-9A-Fa-f]{1,2}|(?:[0-9A-Fa-f]{4}\.){2}[0-9A-Fa-f]{4})`,
			`(?P<uuid>[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12})`,
			`(?P<call>[\w.]*?)\(`,
			`\b(?P<bool_true>true)\b|\b(?P<bool_false>false)\b|\b(?P<nil>nil)\b`,
			`(?P<ellipsis>\.\.\.)`,
			`(?P<number>(?<!\w)\-?[0-9]+\.?[0-9]*(e[-+]?\d+?)?\b|0x[0-9a-fA-F]*)`,
			`(?P<path>\B(/[-\w._+]+)*\/)(?P<filename>[-\w._+]*)?`,
			`(?<![\\\w])(?P<str>'.*?(?<!\\)'|".*?(?<!\\)")`,
			"(?P<url>(file|https|http|ws|wss)://[-0-9a-zA-Z$_+!`(),.?/;:&=%#~@]*)",
		}, "|"),
	}
	out := make([]highlightPattern, 0, len(sources))
	for _, src := range sources {
		re := regexutil.MustCompile(src)
		hp := highlightPattern{re: re}
		for _, n := range re.GetGroupNumbers() {
			if st, ok := styles[re.GroupNameFromNumber(n)]; ok {
				hp.groups = append(hp.groups, highlightGroup{n, st})
			}
		}
		out = append(out, hp)
	}
	return out
})

// Spans coloring the literals of plain text, in pattern then group order
func highlight(plain string) []span {
	if plain == "" {
		return nil
	}
	var spans []span
	for _, hp := range highlightPatterns() {
		for m := range regexutil.All(hp.re, plain) {
			for _, g := range hp.groups {
				grp := m.GroupByNumber(g.number)
				if grp == nil || len(grp.Captures) == 0 || grp.RuneLength == 0 {
					continue
				}
				at, n := grp.ByteRange()
				spans = append(spans, span{at, at + n, g.style})
			}
		}
	}
	return spans
}
