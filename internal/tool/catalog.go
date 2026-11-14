package tool

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	searchToolName    = "tool_search"
	describeToolName  = "tool_describe"
	callToolName      = "tool_call"
	charsPerToken     = 4.0
	catalogSummaryMax = 80
	minTermCoverage   = 0.5
	minCoverageTerms  = 4
)

func IsBridgeTool(name string) bool {
	return name == searchToolName || name == describeToolName || name == callToolName
}

type catalogEntry struct {
	name        string
	description string
	params      *jnode
	source      string
	sourceName  string
	tokens      []string
	tokenSet    map[string]bool
}

func tokenize(text string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(text); i++ {
		alnum := i < len(text) && (text[i] >= 'a' && text[i] <= 'z' || text[i] >= 'A' && text[i] <= 'Z' || text[i] >= '0' && text[i] <= '9')
		if alnum && start < 0 {
			start = i
		}
		if !alnum && start >= 0 {
			out = append(out, stemWord(strings.ToLower(text[start:i])))
			start = -1
		}
	}
	return out
}

func newCatalogEntry(name, description string, params *jnode, source, sourceName, text string) catalogEntry {
	tokens := tokenize(text)
	set := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		set[t] = true
	}
	return catalogEntry{name: name, description: description, params: params, source: source, sourceName: sourceName, tokens: tokens, tokenSet: set}
}

func listingLabel(sourceName string) string {
	label := sourceName
	if label == "" {
		label = "other"
	}
	return strings.TrimPrefix(label, "mcp-")
}

func searchText(name, description string, params *jnode, sourceLabel string) string {
	name = strings.TrimPrefix(name, "mcp__")
	nameWords := strings.Map(func(r rune) rune {
		switch r {
		case '_', '.', ':', '-':
			return ' '
		}
		return r
	}, name)
	extra := ""
	if sourceLabel != "" && !slices.Contains(wideFields(nameWords), sourceLabel) {
		extra = sourceLabel
	}
	var paramNames []string
	if props := params.get("properties"); props.isObj() {
		paramNames = props.keys
	}
	return nameWords + " " + extra + " " + description + " " + strings.Join(paramNames, " ")
}

type corpusStats struct {
	avg float64
	df  map[string]int
	n   int
}

func statsOf(catalog []catalogEntry) corpusStats {
	st := corpusStats{df: map[string]int{}, n: len(catalog)}
	total := 0
	for _, e := range catalog {
		total += len(e.tokens)
		for t := range e.tokenSet {
			st.df[t]++
		}
	}
	st.avg = float64(total) / math.Max(float64(len(catalog)), 1)
	return st
}

func (st corpusStats) idf(token string) float64 {
	df := st.df[token]
	return math.Log(1 + (float64(st.n)-float64(df)+0.5)/(float64(df)+0.5))
}

func (st corpusStats) bm25(query []string, doc []string) float64 {
	const k1, b = 1.5, 0.75
	score := 0.0
	dl := float64(len(doc))
	tf := map[string]int{}
	for _, t := range doc {
		tf[t]++
	}
	for _, q := range query {
		df, f := st.df[q], tf[q]
		if df == 0 || f == 0 {
			continue
		}
		idf := math.Log(1 + (float64(st.n)-float64(df)+0.5)/(float64(df)+0.5))
		score += idf * float64(f) * (k1 + 1) / (float64(f) + k1*(1-b+b*dl/math.Max(st.avg, 1.0)))
	}
	return score
}

func requiredCoverage(answerable int) int {
	if answerable < minCoverageTerms {
		return 1
	}
	return int(math.Ceil(float64(answerable) * minTermCoverage))
}

func searchCatalog(catalog []catalogEntry, query string, limit int) []catalogEntry {
	if len(catalog) == 0 || limit <= 0 {
		return nil
	}
	tokens := tokenize(query)
	if len(tokens) == 0 {
		return nil
	}
	st := statsOf(catalog)
	gate := tokens[0]
	best := st.idf(gate)
	for _, t := range tokens[1:] {
		if v := st.idf(t); v > best {
			gate, best = t, v
		}
	}
	answerable := map[string]bool{}
	for _, t := range tokens {
		if st.df[t] > 0 {
			answerable[t] = true
		}
	}
	required := requiredCoverage(len(answerable))
	exact := strings.ToLower(trimWide(query))
	type scored struct {
		score float64
		entry catalogEntry
	}
	var hits []scored
	for _, e := range catalog {
		isExact := strings.ToLower(e.name) == exact
		if !isExact {
			if !e.tokenSet[gate] {
				continue
			}
			matched := 0
			for t := range answerable {
				if e.tokenSet[t] {
					matched++
				}
			}
			if matched < required {
				continue
			}
		}
		s := math.Inf(1)
		if !isExact {
			s = st.bm25(tokens, e.tokens)
		}
		hits = append(hits, scored{s, e})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]catalogEntry, len(hits))
	for i, h := range hits {
		out[i] = h.entry
	}
	return out
}

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_'
}

func sentenceEnd(text []rune) int {
	for i, r := range text {
		if r != '.' && r != '!' && r != '?' {
			continue
		}
		if i+1 < len(text) && !wideSpace(text[i+1]) {
			continue
		}
		if abbreviationBefore(text, i, "e.g") || abbreviationBefore(text, i, "i.e") || abbreviationBefore(text, i, "etc") {
			continue
		}
		return i + 1
	}
	return -1
}

func abbreviationBefore(text []rune, at int, word string) bool {
	w := []rune(word)
	start := at - len(w)
	if start < 0 || string(text[start:at]) != word {
		return false
	}
	before := start == 0 || !wordRune(text[start-1])
	return before == wordRune(w[0])
}

func shortDescription(description string, max int) string {
	text := []rune(strings.Join(wideFields(description), " "))
	if end := sentenceEnd(text); end >= 0 {
		text = text[:end]
	}
	if len(text) <= max {
		return string(text)
	}
	clipped := string(text[:max])
	if i := strings.LastIndex(clipped, " "); i >= 0 {
		clipped = clipped[:i]
	}
	return strings.TrimRight(clipped, ",;: ") + "…"
}

type hiddenSource struct {
	name      string
	toolCount int
	sentence  string
}

type listingLine struct {
	name, desc string
}

func renderGroup(label string, tools []listingLine, mode string) string {
	if mode == "summary" {
		return fmt.Sprintf("%s (%d tools — names not listed; discover via `%s`)", label, len(tools), searchToolName)
	}
	var lines []string
	if mode == "full" {
		for _, t := range tools {
			if t.desc != "" {
				lines = append(lines, "- "+t.name+": "+t.desc)
			} else {
				lines = append(lines, "- "+t.name)
			}
		}
	} else if len(tools) > 1 {
		names := make([]string, len(tools))
		for i, t := range tools {
			names[i] = t.name
		}
		lines = []string{strings.Join(names, ", ")}
	} else {
		lines = []string{"- " + tools[0].name}
	}
	if len(tools) == 1 {
		return strings.Join(lines, "\n")
	}
	return strings.Join(append([]string{fmt.Sprintf("%s tools (%d):", label, len(tools))}, lines...), "\n")
}

func buildListing(groups map[string][]listingLine, hidden []hiddenSource, maxTokens int) (string, string) {
	if len(groups) == 0 && len(hidden) == 0 {
		return "", "none"
	}
	labels := make([]string, 0, len(groups))
	for label, tools := range groups {
		labels = append(labels, label)
		slices.SortFunc(tools, func(a, b listingLine) int {
			if c := strings.Compare(a.name, b.name); c != 0 {
				return c
			}
			return strings.Compare(a.desc, b.desc)
		})
	}
	sort.Strings(labels)
	header := fmt.Sprintf("Deferred tool catalog (call schemas via `%s`, invoke via `%s`):", describeToolName, callToolName)
	fits := func(modes map[string]string) (string, bool) {
		var singles []string
		blocks := map[string]string{}
		for _, label := range labels {
			if len(groups[label]) == 1 && modes[label] != "summary" {
				singles = append(singles, renderGroup(label, groups[label], modes[label]))
				continue
			}
			blocks[label] = renderGroup(label, groups[label], modes[label])
		}
		for _, h := range hidden {
			if _, ok := blocks[h.name]; ok {
				continue
			}
			blocks[h.name] = fmt.Sprintf("%s (%d tools unavailable: %s)", h.name, h.toolCount, h.sentence)
		}
		keys := make([]string, 0, len(blocks))
		for k := range blocks {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := append([]string{header}, singles...)
		for _, k := range keys {
			parts = append(parts, blocks[k])
		}
		text := strings.Join(parts, "\n")
		if math.Ceil(float64(utf8.RuneCountInString(text))/charsPerToken) <= float64(maxTokens) {
			return text, true
		}
		return "", false
	}
	uniform := func(mode string) map[string]string {
		m := map[string]string{}
		for _, l := range labels {
			m[l] = mode
		}
		return m
	}
	for _, mode := range []string{"full", "names"} {
		if text, ok := fits(uniform(mode)); ok {
			return text, mode
		}
	}
	order := slices.Clone(labels)
	sizes := map[string]int{}
	for _, l := range order {
		sizes[l] = utf8.RuneCountInString(renderGroup(l, groups[l], "names"))
	}
	sort.SliceStable(order, func(i, j int) bool {
		if sizes[order[i]] != sizes[order[j]] {
			return sizes[order[i]] > sizes[order[j]]
		}
		return order[i] < order[j]
	})
	summarized := map[string]bool{}
	for _, l := range order {
		summarized[l] = true
		for _, rest := range []string{"full", "names"} {
			modes := map[string]string{}
			for _, g := range labels {
				if summarized[g] {
					modes[g] = "summary"
				} else {
					modes[g] = rest
				}
			}
			if text, ok := fits(modes); ok {
				if len(summarized) == len(labels) {
					return text, "groups"
				}
				return text, "mixed"
			}
		}
	}
	return "", "none"
}

func ListingDescription(form string) string {
	switch form {
	case "full":
		return "catalog listing embedded"
	case "names":
		return "names-only listing embedded"
	case "mixed":
		return "listing embedded (oversized servers summarized)"
	case "groups":
		return "server summary embedded (search-only discovery)"
	case "none":
		return "no listing (search-only)"
	}
	return form
}
