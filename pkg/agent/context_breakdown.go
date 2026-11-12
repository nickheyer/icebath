package agent

import (
	"context"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var skillsBlockRe = regexp.MustCompile(`(?s)<available_skills>.*?</available_skills>`)

const (
	breakdownFreeGlyph    = "·"
	breakdownGridColumns  = 20
	breakdownGridRows     = 5
	detailsTableLimit     = 15
	breakdownSubagentTool = "delegate_task"
)

// Category ids in display order with their dashboard color and context grid glyph
var breakdownCategories = []struct{ id, color, glyph string }{
	{"system_prompt", "var(--context-usage-system)", "■"},
	{"tool_definitions", "var(--context-usage-tools)", "▣"},
	{"rules", "var(--context-usage-rules)", "▩"},
	{"skills", "var(--context-usage-skills)", "▤"},
	{"mcp", "var(--context-usage-mcp)", "▥"},
	{"subagent_definitions", "var(--context-usage-subagents)", "▦"},
	{"memory", "var(--context-usage-memory)", "▧"},
	{"conversation", "var(--context-usage-conversation)", "▨"},
}

// Occupancy sources the source line localizes, any other value printed verbatim
var contextSources = map[string]bool{"local_estimate": true, "provider_usage": true, "provider_usage_plus_estimate": true}

// ContextCategory is one estimated slice of the context window
type ContextCategory struct {
	Color  string `json:"color"`
	ID     string `json:"id"`
	Label  string `json:"label"`
	Tokens int    `json:"tokens"`
}

// ContextBreakdown is the context command payload, the categories plus the window occupancy and its provenance
type ContextBreakdown struct {
	Categories       []ContextCategory `json:"categories"`
	EstimatedTotal   int               `json:"estimated_total"`
	ContextMax       int               `json:"context_max"`
	ContextPercent   int               `json:"context_percent"`
	ContextUsed      int               `json:"context_used"`
	ContextSource    string            `json:"context_source"`
	ContextEstimated bool              `json:"context_estimated"`
	Model            string            `json:"model"`
}

// ContextSkillCost is one skill's index line and SKILL.md cost, a nil SKILL.md cost meaning unknown
type ContextSkillCost struct {
	Name          string `json:"name"`
	IndexTokens   int    `json:"index_tokens"`
	SkillMDTokens *int   `json:"skill_md_tokens"`
}

// ContextToolsetCost is one toolset's schema cost
type ContextToolsetCost struct {
	Toolset      string `json:"toolset"`
	ToolCount    int    `json:"tool_count"`
	SchemaTokens int    `json:"schema_tokens"`
}

// ContextDetails is the expanded per skill and per toolset listing of the context all view
type ContextDetails struct {
	Skills   []ContextSkillCost   `json:"skills"`
	Toolsets []ContextToolsetCost `json:"toolsets"`
}

// ContextUsageFields is the current occupancy of a context engine
type ContextUsageFields struct {
	ContextUsed      int    `json:"context_used"`
	ContextMax       int    `json:"context_max"`
	ContextPercent   int    `json:"context_percent"`
	ContextSource    string `json:"context_source"`
	ContextEstimated bool   `json:"context_estimated"`
}

// System prompt tiers the breakdown measures, swapped by tests
var breakdownPromptParts = func(ctx context.Context, a *AIAgent) (SystemPromptParts, error) {
	return BuildSystemPromptParts(ctx, a, nil)
}

// Python str.format of one constant spec, a malformed spec raising as in Python
func breakdownFormat(spec string, kw map[string]any) string {
	out, err := i18n.PyFormat(spec, kw)
	if err != nil {
		panic(err)
	}
	return out
}

// Localized label of a category id
func contextCategoryLabel(ctx context.Context, id string) string {
	return i18n.T(ctx, "gateway.context.category."+id, "", nil)
}

// Rough tokens of the compact JSON a value dumps to, zero for an empty value
func breakdownJSONTokens(tools []map[string]any) int {
	if len(tools) == 0 {
		return 0
	}
	text, err := pyjson.Dumps(tools, pyjson.EnsureASCII(false))
	if err != nil {
		return 0
	}
	return provider.EstimateTokens(text)
}

// Tokens of a byte size at four characters a token rounded up, nil for an unknown size
func bytesToTokens(size *int64) *int {
	if size == nil {
		return nil
	}
	n := int((*size + 3) / provider.CharsPerToken)
	return &n
}

// Live available skills block inside the stable tier, empty when absent
func skillsBlock(stable string) string {
	return skillsBlockRe.FindString(stable)
}

// Tool schemas split into built in, MCP and subagent buckets
func splitBreakdownTools(tools []map[string]any) (builtin, mcp, subagent []map[string]any) {
	for _, tool := range tools {
		source := tool
		if fn, ok := pyjson.Dict(tool["function"]); ok {
			source = fn
		}
		name, _ := source["name"].(string)
		switch {
		case strings.HasPrefix(name, "mcp_"):
			mcp = append(mcp, tool)
		case name == breakdownSubagentTool:
			subagent = append(subagent, tool)
		default:
			builtin = append(builtin, tool)
		}
	}
	return builtin, mcp, subagent
}

// Memory and user profile blocks the agent's memory store renders into the volatile tier
func breakdownMemoryBlocks(a *AIAgent) (memoryBlock, userBlock string) {
	if a.memoryStore != nil && a.memoryEnabled {
		memoryBlock = a.memoryStore.FormatForSystemPrompt("memory")
	}
	if a.memoryStore != nil && a.userProfileEnabled {
		userBlock = a.memoryStore.FormatForSystemPrompt("user")
	}
	return memoryBlock, userBlock
}

// Text with every non empty block removed and the result stripped
func stripBlocks(text string, blocks ...string) string {
	for _, b := range blocks {
		if b != "" {
			text = strings.ReplaceAll(text, b, "")
		}
	}
	return pystr.Strip(text)
}

// Non empty parts joined by blank lines and stripped
func joinBlocks(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return pystr.Strip(strings.Join(kept, "\n\n"))
}

// Grid glyph of a category id, a small square for unknown ids
func categoryGlyph(id string) string {
	for _, c := range breakdownCategories {
		if c.id == id {
			return c.glyph
		}
	}
	return "▪"
}

// Python round of a ratio as an int, halves to even
func roundHalfEven(x float64) int { return int(math.RoundToEven(x)) }

// Current occupancy of an engine, nothing when either the used or the maximum figure is zero
func ContextUsageFieldsOf(engine ContextEngine) (ContextUsageFields, bool) {
	base := engine.Base()
	used := max(0, base.LastPromptTokens)
	maximum := engineContextLength(engine)
	if used == 0 || maximum == 0 {
		return ContextUsageFields{}, false
	}
	used = min(used, maximum)
	source := contextDisplaySource(engine)
	return ContextUsageFields{ContextUsed: used, ContextMax: maximum,
		ContextPercent: max(0, min(100, roundHalfEven(float64(used)/float64(maximum)*100))),
		ContextSource:  source, ContextEstimated: source != "provider_usage"}, true
}

// Categories in display order and their estimated total, zero rows dropped except the conversation
func breakdownCategoriesOf(ctx context.Context, systemPrompt, skills string, tools []map[string]any, messages []map[string]any, rules, memory string) ContextBreakdown {
	builtin, mcp, subagent := splitBreakdownTools(tools)
	tokens := map[string]int{
		"system_prompt":        provider.EstimateTokens(systemPrompt),
		"tool_definitions":     breakdownJSONTokens(builtin),
		"rules":                provider.EstimateTokens(rules),
		"skills":               provider.EstimateTokens(skills),
		"mcp":                  breakdownJSONTokens(mcp),
		"subagent_definitions": breakdownJSONTokens(subagent),
		"memory":               provider.EstimateTokens(memory),
		"conversation":         provider.EstimateRowsTokens(messages, provider.CurrentImageTokenCost(ctx)),
	}
	out := ContextBreakdown{Categories: []ContextCategory{}}
	for _, c := range breakdownCategories {
		out.EstimatedTotal += tokens[c.id]
		if tokens[c.id] > 0 || c.id == "conversation" {
			out.Categories = append(out.Categories, ContextCategory{Color: c.color, ID: c.id, Label: contextCategoryLabel(ctx, c.id), Tokens: tokens[c.id]})
		}
	}
	return out
}

// Categories and total from a frozen system prompt and tool schemas, rules and memory counted inside the prompt
func StoredContextBreakdown(ctx context.Context, systemPrompt string, tools []map[string]any, messages []map[string]any) ContextBreakdown {
	index := skillsBlock(systemPrompt)
	return breakdownCategoriesOf(ctx, stripBlocks(systemPrompt, index), index, tools, messages, "", "")
}

// Category breakdown of one live agent with the window occupancy from the usage anchors or the estimate
func ComputeSessionContextBreakdown(ctx context.Context, a *AIAgent, messages []map[string]any) (ContextBreakdown, error) {
	if messages == nil {
		messages = []map[string]any{}
	}
	parts, err := breakdownPromptParts(ctx, a)
	if err != nil {
		return ContextBreakdown{}, err
	}
	index := skillsBlock(parts.Stable)
	memoryBlock, userBlock := breakdownMemoryBlocks(a)
	systemPromptText := joinBlocks(stripBlocks(parts.Stable, index), stripBlocks(parts.Volatile, memoryBlock, userBlock))
	out := breakdownCategoriesOf(ctx, systemPromptText, index, a.ToolDefs(), messages, parts.Context, joinBlocks(memoryBlock, userBlock))
	contextMax := 0
	if a.contextCompressor != nil {
		contextMax = engineContextLength(a.contextCompressor)
	}
	rows := rowsToMessages(messages)
	anchor := a.turnBaseUsageAnchor
	contextUsed, anchored := provider.AnchoredContextTokens(ctx, rows, anchor, false)
	if !anchored {
		anchor = a.usageAnchor
		contextUsed, anchored = provider.AnchoredContextTokens(ctx, rows, anchor, true)
	}
	var source string
	if !anchored {
		measured := 0
		if a.contextCompressor != nil {
			measured = a.contextCompressor.Base().LastPromptTokens
		}
		if measured > 0 {
			contextUsed, source = measured, contextDisplaySource(a.contextCompressor)
		} else {
			contextUsed, source = out.EstimatedTotal, "local_estimate"
		}
	} else {
		delta := messages[min(anchor.BaseCount, len(messages)):]
		if len(delta) > 0 && delta[0]["role"] == "assistant" {
			delta = delta[1:]
		}
		source = "provider_usage"
		if len(delta) > 0 {
			source = "provider_usage_plus_estimate"
		}
	}
	if contextMax != 0 {
		contextUsed = min(contextUsed, contextMax)
		out.ContextPercent = max(0, min(100, roundHalfEven(float64(contextUsed)/float64(contextMax)*100)))
	}
	out.ContextMax, out.ContextUsed, out.ContextSource = contextMax, contextUsed, source
	out.ContextEstimated = source != "provider_usage"
	out.Model = a.model
	return out, nil
}

// Per skill index and SKILL.md costs and per toolset schema costs of the agent's live prompt
func ComputeContextDetails(ctx context.Context, a *AIAgent) (ContextDetails, error) {
	parts, err := breakdownPromptParts(ctx, a)
	if err != nil {
		return ContextDetails{}, err
	}
	out := ContextDetails{Skills: []ContextSkillCost{}, Toolsets: []ContextToolsetCost{}}
	if block := skillsBlock(parts.Stable); block != "" {
		entries, err := computeSkillsBreakdown(a.env, block)
		if err != nil {
			return ContextDetails{}, err
		}
		for _, e := range entries {
			size := int64(e.IndexLineBytes)
			index := 0
			if n := bytesToTokens(&size); n != nil && *n != 0 {
				index = *n
			}
			out.Skills = append(out.Skills, ContextSkillCost{Name: e.Name, IndexTokens: index, SkillMDTokens: bytesToTokens(e.SkillMDBytes)})
		}
	}
	if defs := a.ToolDefs(); len(defs) > 0 {
		list := make([]any, len(defs))
		for i, d := range defs {
			list[i] = d
		}
		groups, err := computeToolsetsBreakdown(ctx, list)
		if err != nil {
			return ContextDetails{}, err
		}
		for _, g := range groups {
			size := int64(g.JSONBytes)
			out.Toolsets = append(out.Toolsets, ContextToolsetCost{Toolset: g.Toolset, ToolCount: g.ToolCount, SchemaTokens: *bytesToTokens(&size)})
		}
	}
	return out, nil
}

// Glyph grid of one hundred cells, one per percent of the window, categories first and free space after
func RenderContextGrid(payload ContextBreakdown) []string {
	total := breakdownGridColumns * breakdownGridRows
	var cells []string
	if payload.ContextMax > 0 {
		for _, c := range payload.Categories {
			n := roundHalfEven(float64(c.Tokens) / float64(payload.ContextMax) * float64(total))
			if n == 0 && c.Tokens > 0 {
				n = 1
			}
			for range n {
				cells = append(cells, categoryGlyph(c.ID))
			}
		}
		if len(cells) > total {
			cells = cells[:total]
		}
	}
	for len(cells) < total {
		cells = append(cells, breakdownFreeGlyph)
	}
	rows := make([]string, breakdownGridRows)
	for r := range breakdownGridRows {
		rows[r] = strings.Join(cells[r*breakdownGridColumns:(r+1)*breakdownGridColumns], " ")
	}
	return rows
}

// Estimated usage by category table with a free space row when the window is known
func RenderContextCategoryLines(ctx context.Context, payload ContextBreakdown) []string {
	lines := []string{i18n.T(ctx, "gateway.context.category_header", "", nil)}
	if len(payload.Categories) == 0 {
		return append(lines, i18n.T(ctx, "gateway.context.no_data_yet", "", nil))
	}
	denom := payload.ContextMax
	if denom == 0 {
		denom = payload.EstimatedTotal
	}
	freeLabel := i18n.T(ctx, "gateway.context.free_space", "", nil)
	width := utf8.RuneCountInString(freeLabel)
	for _, c := range payload.Categories {
		width = max(width, utf8.RuneCountInString(c.Label))
	}
	row := func(glyph, label string, tokens int, pct float64) string {
		return i18n.T(ctx, "gateway.context.category_row", "", map[string]any{"glyph": glyph,
			"label":  breakdownFormat("{v:<{w}}", map[string]any{"v": label, "w": width}),
			"tokens": breakdownFormat("{v:>9,}", map[string]any{"v": tokens}),
			"pct":    breakdownFormat("{v:>5.1f}", map[string]any{"v": pct})})
	}
	for _, c := range payload.Categories {
		label := c.Label
		if label == "" {
			label = c.ID
		}
		pct := 0.0
		if denom != 0 {
			pct = float64(c.Tokens) / float64(denom) * 100
		}
		lines = append(lines, row(categoryGlyph(c.ID), label, c.Tokens, pct))
	}
	if payload.ContextMax > 0 {
		free := max(0, payload.ContextMax-payload.EstimatedTotal)
		lines = append(lines, row(breakdownFreeGlyph, freeLabel, free, float64(free)/float64(payload.ContextMax)*100))
	}
	return lines
}

// One toolset row of the expanded listing
func toolsetCostRow(ctx context.Context, g ContextToolsetCost) string {
	return i18n.T(ctx, "gateway.context.toolset_row", "", map[string]any{
		"toolset": breakdownFormat("{v:<24}", map[string]any{"v": g.Toolset}),
		"count":   breakdownFormat("{v:>3}", map[string]any{"v": g.ToolCount}),
		"tokens":  breakdownFormat("{v:>8,}", map[string]any{"v": g.SchemaTokens})})
}

// One skill row of the expanded listing, long names cut to 28 cells with an ellipsis
func skillCostRow(ctx context.Context, e ContextSkillCost) string {
	name := e.Name
	if r := []rune(name); len(r) > 28 {
		name = string(r[:27]) + "…"
	}
	md := breakdownFormat("{v:>8}", map[string]any{"v": i18n.T(ctx, "gateway.context.not_available", "", nil)})
	if e.SkillMDTokens != nil {
		md = "~" + breakdownFormat("{v:>8,}", map[string]any{"v": *e.SkillMDTokens})
	}
	return i18n.T(ctx, "gateway.context.skill_row", "", map[string]any{
		"name":         breakdownFormat("{v:<28}", map[string]any{"v": name}),
		"index_tokens": breakdownFormat("{v:>6,}", map[string]any{"v": e.IndexTokens}),
		"md_tokens":    md})
}

// Appends a titled table capped at the display limit, blank separated from a preceding one
func appendCostTable(ctx context.Context, lines []string, title string, rows []string) []string {
	if len(rows) == 0 {
		return lines
	}
	if len(lines) > 0 {
		lines = append(lines, "")
	}
	lines = append(lines, title)
	lines = append(lines, rows[:min(len(rows), detailsTableLimit)]...)
	if len(rows) > detailsTableLimit {
		lines = append(lines, i18n.T(ctx, "gateway.context.and_more", "", map[string]any{"count": len(rows) - detailsTableLimit}))
	}
	return lines
}

// Expanded per toolset and per skill tables
func RenderContextDetailsLines(ctx context.Context, details ContextDetails) []string {
	toolsets := make([]string, len(details.Toolsets))
	for i, g := range details.Toolsets {
		toolsets[i] = toolsetCostRow(ctx, g)
	}
	skills := make([]string, len(details.Skills))
	for i, e := range details.Skills {
		skills[i] = skillCostRow(ctx, e)
	}
	lines := appendCostTable(ctx, []string{}, i18n.T(ctx, "gateway.context.toolsets_title", "", nil), toolsets)
	return appendCostTable(ctx, lines, i18n.T(ctx, "gateway.context.skills_title", "", nil), skills)
}

// Full context view, the glyph grid first when asked and the expanded tables when details are given
func RenderContextBreakdownLines(ctx context.Context, payload ContextBreakdown, details *ContextDetails, grid bool) []string {
	lines := []string{}
	if grid {
		lines = append(append(lines, RenderContextGrid(payload)...), "")
	}
	lines = append(lines, RenderContextCategoryLines(ctx, payload)...)
	if payload.ContextMax > 0 {
		mark := ""
		if payload.ContextEstimated {
			mark = "~"
		}
		lines = append(lines, "", i18n.T(ctx, "gateway.context.window_line", "", map[string]any{"mark": mark,
			"used": breakdownFormat("{v:,}", map[string]any{"v": payload.ContextUsed}),
			"max":  breakdownFormat("{v:,}", map[string]any{"v": payload.ContextMax}), "pct": payload.ContextPercent}))
		if source := payload.ContextSource; source != "" {
			label := source
			if contextSources[source] {
				label = i18n.T(ctx, "gateway.context.source."+source, "", nil)
			}
			lines = append(lines, i18n.T(ctx, "gateway.context.source_line", "", map[string]any{"source": label}))
		}
	}
	if details == nil {
		return append(lines, "", i18n.T(ctx, "gateway.context.hint_all", "", nil))
	}
	if detail := RenderContextDetailsLines(ctx, *details); len(detail) > 0 {
		lines = append(append(lines, ""), detail...)
	}
	return lines
}
