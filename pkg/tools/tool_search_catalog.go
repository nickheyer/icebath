package tools

import (
	"context"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
)

// Reserved bridge names no user, plugin, or MCP tool may take
const (
	ToolSearchName   = "tool_search"
	ToolDescribeName = "tool_describe"
	ToolCallName     = "tool_call"
)

var BridgeToolNames = map[string]bool{ToolSearchName: true, ToolDescribeName: true, ToolCallName: true}

// CharsPerToken slightly underestimates so activation errs toward fewer false triggers
const CharsPerToken = 4.0

// CatalogEntry is one deferrable tool in the form the bridge tools search and serve
type CatalogEntry struct {
	Name        string
	Description string
	Schema      map[string]any
	Source      string
	SourceName  string
	Tokens      []string
}

var catalogTokenRe = regexp.MustCompile(`[A-Za-z0-9]+`)

var catalogNameSplitRe = regexp.MustCompile(`[_.:-]`)

// Lowercase alphanumeric tokens stemmed so the index and query paths agree
func catalogTokenize(text string) []string {
	if text == "" {
		return nil
	}
	raw := catalogTokenRe.FindAllString(text, -1)
	out := make([]string, 0, len(raw))
	for _, tok := range raw {
		out = append(out, stemToken(strings.ToLower(tok)))
	}
	return out
}

// The function block of a tool def or an empty map when absent
func toolDefFn(td map[string]any) map[string]any {
	if fn, ok := pyjson.Dict(td["function"]); ok && fn != nil {
		return fn
	}
	return map[string]any{}
}

func toolDefName(td map[string]any) string {
	name, _ := toolDefFn(td)["name"].(string)
	return name
}

func toolDefDescription(td map[string]any) string {
	desc, _ := toolDefFn(td)["description"].(string)
	return desc
}

// Registry lookups scoped to one session's context and environment
type searchScope struct {
	ctx context.Context
	env *Env
	reg *Registry
}

func newSearchScope(ctx context.Context, env *Env) searchScope {
	if ctx == nil {
		ctx = context.Background()
	}
	return searchScope{ctx: ctx, env: env, reg: Default}
}

func (s searchScope) entry(name string) *Def {
	return s.reg.Entry(s.ctx, s.env, name)
}

// Toolset of a registered tool or empty when unregistered
func (s searchScope) toolset(name string) (string, bool) {
	d := s.entry(name)
	if d == nil {
		return "", false
	}
	return d.Toolset, true
}

func (s searchScope) scopeKey() string {
	if s.env == nil {
		return ""
	}
	return ScopeKey(s.env.Home)
}

// Search text blob of split name words, source label, description, and top level parameter names
func catalogEntrySearchText(td map[string]any, sourceLabel string) string {
	fn := toolDefFn(td)
	name, _ := fn["name"].(string)
	name = strings.TrimPrefix(name, "mcp__")
	nameWords := catalogNameSplitRe.ReplaceAllString(name, " ")
	extra := ""
	if sourceLabel != "" {
		present := false
		for _, w := range strings.Fields(nameWords) {
			if w == sourceLabel {
				present = true
				break
			}
		}
		if !present {
			extra = sourceLabel
		}
	}
	var paramNames []string
	if params, ok := fn["parameters"].(map[string]any); ok {
		if props, ok := params["properties"].(map[string]any); ok {
			paramNames = sortedKeys(props)
		}
	}
	desc, _ := fn["description"].(string)
	return fmt.Sprintf("%s %s %s %s", nameWords, extra, desc, strings.Join(paramNames, " "))
}

// Source kind and source name for a registered tool name
func (s searchScope) classifySource(name string) (string, string) {
	toolset, ok := s.toolset(name)
	if !ok {
		return "other", ""
	}
	if strings.HasPrefix(toolset, "mcp-") {
		return "mcp", toolset
	}
	return "plugin", toolset
}

// BuildCatalog builds the deferred tool catalog from the deferrable subset of tool defs
func BuildCatalog(ctx context.Context, env *Env, toolDefs []map[string]any) []CatalogEntry {
	s := newSearchScope(ctx, env)
	catalog := make([]CatalogEntry, 0, len(toolDefs))
	for _, td := range toolDefs {
		name := toolDefName(td)
		if name == "" {
			continue
		}
		source, sourceName := s.classifySource(name)
		sourceLabel := ""
		if sourceName != "" {
			sourceLabel = catalogListingGroupLabel(sourceName)
		}
		catalog = append(catalog, CatalogEntry{
			Name: name, Description: toolDefDescription(td), Schema: td, Source: source,
			SourceName: sourceName, Tokens: catalogTokenize(catalogEntrySearchText(td, sourceLabel)),
		})
	}
	return catalog
}

// Standard BM25 for one query against one document
func catalogBM25Score(queryTokens, docTokens []string, avgDL float64, docFreq map[string]int, nDocs int) float64 {
	const k1, b = 1.5, 0.75
	score := 0.0
	dl := float64(len(docTokens))
	docTF := map[string]int{}
	for _, t := range docTokens {
		docTF[t]++
	}
	for _, q := range queryTokens {
		df, tf := docFreq[q], docTF[q]
		if df == 0 || tf == 0 {
			continue
		}
		idf := math.Log(1 + (float64(nDocs)-float64(df)+0.5)/(float64(df)+0.5))
		tff := float64(tf)
		score += idf * tff * (k1 + 1) / (tff + k1*(1-b+b*dl/math.Max(avgDL, 1.0)))
	}
	return score
}

// CorpusStats holds the BM25 statistics shared by every query over a catalog
type CorpusStats struct {
	DocLengths []int
	AvgDL      float64
	DocFreq    map[string]int
	NDocs      int
}

func catalogCorpusStats(catalog []CatalogEntry) CorpusStats {
	lengths := make([]int, len(catalog))
	total := 0
	docFreq := map[string]int{}
	for i, entry := range catalog {
		lengths[i] = len(entry.Tokens)
		total += lengths[i]
		seen := map[string]bool{}
		for _, tok := range entry.Tokens {
			if !seen[tok] {
				seen[tok] = true
				docFreq[tok]++
			}
		}
	}
	avg := float64(total) / math.Max(float64(len(lengths)), 1)
	return CorpusStats{DocLengths: lengths, AvgDL: avg, DocFreq: docFreq, NDocs: len(catalog)}
}

// The query token with the highest IDF, the word that names the intent
func catalogGateToken(queryTokens []string, docFreq map[string]int, nDocs int) string {
	idf := func(token string) float64 {
		df := float64(docFreq[token])
		return math.Log(1 + (float64(nDocs)-df+0.5)/(df+0.5))
	}
	best := queryTokens[0]
	bestIDF := idf(best)
	for _, t := range queryTokens[1:] {
		if v := idf(t); v > bestIDF {
			best, bestIDF = t, v
		}
	}
	return best
}

// Relevance floor so long hunts whose words exist only scattered across tools return nothing
const (
	MinQueryTermCoverage          = 0.5
	MinAnswerableTermsForCoverage = 4
)

// How many answerable unique terms a document must match to be offered
func catalogRequiredTermCoverage(answerableTermCount int) int {
	if answerableTermCount < MinAnswerableTermsForCoverage {
		return 1
	}
	return int(math.Ceil(float64(answerableTermCount) * MinQueryTermCoverage))
}

// SearchCatalog returns the top limit entries by BM25 with exact name matches ranked first
func SearchCatalog(catalog []CatalogEntry, query string, limit int, stats *CorpusStats) []CatalogEntry {
	var queryTokens []string
	if len(catalog) > 0 && limit > 0 {
		queryTokens = catalogTokenize(query)
	}
	if len(queryTokens) == 0 {
		return nil
	}
	var cs CorpusStats
	if stats != nil {
		cs = *stats
	} else {
		cs = catalogCorpusStats(catalog)
	}
	gate := catalogGateToken(queryTokens, cs.DocFreq, cs.NDocs)
	answerable := map[string]bool{}
	for _, t := range queryTokens {
		if cs.DocFreq[t] > 0 {
			answerable[t] = true
		}
	}
	requiredTerms := catalogRequiredTermCoverage(len(answerable))
	exactName := strings.ToLower(strings.TrimSpace(query))
	admitted := func(entry CatalogEntry) bool {
		if strings.ToLower(entry.Name) == exactName {
			return true
		}
		tokens := map[string]bool{}
		for _, t := range entry.Tokens {
			tokens[t] = true
		}
		if !tokens[gate] {
			return false
		}
		covered := 0
		for t := range answerable {
			if tokens[t] {
				covered++
			}
		}
		return covered >= requiredTerms
	}
	type scored struct {
		score float64
		index int
	}
	var hits []scored
	for i, entry := range catalog {
		if !admitted(entry) {
			continue
		}
		score := math.Inf(1)
		if strings.ToLower(entry.Name) != exactName {
			score = catalogBM25Score(queryTokens, entry.Tokens, cs.AvgDL, cs.DocFreq, cs.NDocs)
		}
		hits = append(hits, scored{score, i})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]CatalogEntry, 0, len(hits))
	for _, h := range hits {
		out = append(out, catalog[h.index])
	}
	return out
}

// Sentence end punctuation followed by whitespace or end, never inside an abbreviation
var catalogSentenceEndRe = regexp2.MustCompile(`(?<!\be\.g)(?<!\bi\.e)(?<!\betc)[.!?](?=\s|$)`, regexp2.None)

// CatalogSummaryMaxChars caps one catalog line at its tool's first sentence
const CatalogSummaryMaxChars = 80

// First sentence of a description clipped on a word boundary
func catalogShortDesc(description string, maxChars int) string {
	text := strings.Join(strings.Fields(description), " ")
	if m, _ := catalogSentenceEndRe.FindStringMatch(text); m != nil {
		runes := []rune(text)
		text = string(runes[:m.RuneIndex+m.RuneLength])
	}
	if utf8.RuneCountInString(text) <= maxChars {
		return text
	}
	runes := []rune(text)
	clipped := string(runes[:maxChars])
	if idx := strings.LastIndex(clipped, " "); idx >= 0 {
		clipped = clipped[:idx]
	}
	return strings.TrimRight(clipped, ",;: ") + "…"
}

// Human facing group heading for a toolset such as mcp-github becoming github
func catalogListingGroupLabel(sourceName string) string {
	label := sourceName
	if label == "" {
		label = "other"
	}
	return strings.TrimPrefix(label, "mcp-")
}

// MCPUnavailableDetails reports the unavailability sentence of a declared MCP server, set by pkg/mcp
var MCPUnavailableDetails func(server string) (sentence string, declared bool, ok bool)

// HiddenDeclaredSource summarizes one declared MCP server hidden by its availability check
type HiddenDeclaredSource struct {
	Name        string
	ToolCount   int
	Unavailable string
}

// HiddenDeclaredSources returns deterministic summaries for declared MCP servers hidden by their check
func HiddenDeclaredSources(ctx context.Context, env *Env) []HiddenDeclaredSource {
	s := newSearchScope(ctx, env)
	grouped := map[string][]*Def{}
	for _, d := range s.reg.Snapshot(s.ctx, s.scopeKey()) {
		if strings.HasPrefix(d.Toolset, "mcp-") {
			server := d.Toolset[4:]
			grouped[server] = append(grouped[server], d)
		}
	}
	if len(grouped) == 0 {
		return nil
	}
	if MCPUnavailableDetails == nil {
		panic("tools: pkg/mcp must install MCPUnavailableDetails before MCP toolsets register")
	}
	servers := make([]string, 0, len(grouped))
	for server := range grouped {
		servers = append(servers, server)
	}
	sort.Strings(servers)
	var rows []HiddenDeclaredSource
	for _, server := range servers {
		entries := grouped[server]
		anyAvailable := false
		for _, d := range entries {
			if d.Check == nil || d.Check(s.env) {
				anyAvailable = true
				break
			}
		}
		if anyAvailable {
			continue
		}
		sentence, declared, ok := MCPUnavailableDetails(server)
		if !declared || !ok {
			continue
		}
		rows = append(rows, HiddenDeclaredSource{Name: server, ToolCount: len(entries), Unavailable: sentence})
	}
	return rows
}

type catalogListingTool struct {
	name string
	desc string
}

// BuildCatalogListingWithForm renders the deferred catalog manifest and reports its degradation form
func BuildCatalogListingWithForm(ctx context.Context, env *Env, deferrable []map[string]any, maxTokens int) (string, string) {
	s := newSearchScope(ctx, env)
	groups := map[string][]catalogListingTool{}
	for _, td := range deferrable {
		name := toolDefName(td)
		if name == "" {
			continue
		}
		_, sourceName := s.classifySource(name)
		label := catalogListingGroupLabel(sourceName)
		groups[label] = append(groups[label], catalogListingTool{name, catalogShortDesc(toolDefDescription(td), CatalogSummaryMaxChars)})
	}
	unavailable := HiddenDeclaredSources(ctx, env)
	if len(groups) == 0 && len(unavailable) == 0 {
		return "", "none"
	}
	labels := make([]string, 0, len(groups))
	for label := range groups {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	for _, label := range labels {
		tools := groups[label]
		sort.Slice(tools, func(i, j int) bool {
			if tools[i].name != tools[j].name {
				return tools[i].name < tools[j].name
			}
			return tools[i].desc < tools[j].desc
		})
	}
	renderGroup := func(label, mode string) string {
		tools := groups[label]
		if mode == "summary" {
			return fmt.Sprintf("%s (%d tools — names not listed; discover via `%s`)", label, len(tools), ToolSearchName)
		}
		var lines []string
		if mode == "full" {
			for _, t := range tools {
				if t.desc != "" {
					lines = append(lines, fmt.Sprintf("- %s: %s", t.name, t.desc))
				} else {
					lines = append(lines, "- "+t.name)
				}
			}
		} else if len(tools) > 1 {
			names := make([]string, 0, len(tools))
			for _, t := range tools {
				names = append(names, t.name)
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
	header := fmt.Sprintf("Deferred tool catalog (call schemas via `%s`, invoke via `%s`):", ToolDescribeName, ToolCallName)
	assembleIfFits := func(modes map[string]string) (string, bool) {
		var singles []string
		availableBlocks := map[string]string{}
		for _, label := range labels {
			if len(groups[label]) == 1 && modes[label] != "summary" {
				singles = append(singles, renderGroup(label, modes[label]))
			} else {
				availableBlocks[label] = renderGroup(label, modes[label])
			}
		}
		unavailableBlocks := map[string]string{}
		for _, row := range unavailable {
			unavailableBlocks[row.Name] = fmt.Sprintf("%s (%d tools unavailable: %s)", row.Name, row.ToolCount, row.Unavailable)
		}
		blockLabels := map[string]bool{}
		for l := range availableBlocks {
			blockLabels[l] = true
		}
		for l := range unavailableBlocks {
			blockLabels[l] = true
		}
		ordered := make([]string, 0, len(blockLabels))
		for l := range blockLabels {
			ordered = append(ordered, l)
		}
		sort.Strings(ordered)
		blocks := make([]string, 0, len(ordered))
		for _, l := range ordered {
			if b, ok := availableBlocks[l]; ok {
				blocks = append(blocks, b)
			} else {
				blocks = append(blocks, unavailableBlocks[l])
			}
		}
		parts := append([]string{header}, singles...)
		parts = append(parts, blocks...)
		text := strings.Join(parts, "\n")
		if int(math.Ceil(float64(utf8.RuneCountInString(text))/CharsPerToken)) <= maxTokens {
			return text, true
		}
		return "", false
	}
	for _, mode := range []string{"full", "names"} {
		modes := map[string]string{}
		for _, l := range labels {
			modes[l] = mode
		}
		if text, ok := assembleIfFits(modes); ok {
			return text, mode
		}
	}
	bySize := append([]string(nil), labels...)
	sort.SliceStable(bySize, func(i, j int) bool {
		li, lj := utf8.RuneCountInString(renderGroup(bySize[i], "names")), utf8.RuneCountInString(renderGroup(bySize[j], "names"))
		if li != lj {
			return li > lj
		}
		return bySize[i] < bySize[j]
	})
	summarized := map[string]bool{}
	for _, lbl := range bySize {
		summarized[lbl] = true
		for _, rest := range []string{"full", "names"} {
			modes := map[string]string{}
			for _, g := range labels {
				if summarized[g] {
					modes[g] = "summary"
				} else {
					modes[g] = rest
				}
			}
			if text, ok := assembleIfFits(modes); ok {
				if len(summarized) == len(groups) {
					return text, "groups"
				}
				return text, "mixed"
			}
		}
	}
	return "", "none"
}
