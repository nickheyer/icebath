package catalog

import (
	"context"
	"math/big"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	WarningCost         = "cost"
	WarningDataPolicy   = "data_policy"
	WarningContextCache = "context_cache"
	WarningMultiple     = "multiple"

	DefaultContextCacheSwitchThreshold = 100_000

	gpt55ProOpenRouterID = "openai/gpt-5.5-pro"
	gpt55Suggestion      = "did you mean to select openai/gpt-5.5?"
)

var (
	inputCostWarningThreshold  = big.NewRat(20, 1)
	outputCostWarningThreshold = big.NewRat(100, 1)
)

const metaContributorMessage = "!!! CONTRIBUTOR TIER — TRAINS ON YOUR DATA !!!\n" +
	"\n" +
	"This is Meta's contributor tier. Selecting it permits Meta to use your\n" +
	"prompts and completions to train future Meta models.\n" +
	"\n" +
	"See current pricing and rate limits for the Meta Model API here:\n" +
	"  https://dev.meta.ai/docs/pricing-rate-limits/\n" +
	"\n" +
	"It lowers the barrier to entry for prototyping, testing integrations, and\n" +
	"scaling experiments where training on your data is acceptable. Do NOT use it\n" +
	"for confidential, proprietary, personal, or otherwise sensitive data. For the\n" +
	"same model with no training on your data, select the standard variant\n" +
	"(without the -contributor suffix).\n" +
	"\n" +
	"Confirm only if training on your prompts and completions is acceptable."

type ExpensiveModelWarning struct {
	Model    string
	Provider string
	Input    *big.Rat
	Output   *big.Rat
	Source   string
	Message  string
}

type DataTrainingWarning struct {
	Model    string
	Provider string
	Message  string
}

type SelectionWarning struct {
	Kind     string
	Title    string
	Model    string
	Provider string
	Message  string
}

type SelectionContext struct {
	ContextTokens int
	CurrentModel  string
}

type SelectionQuery struct {
	Model    string
	Provider string
	BaseURL  string
	Key      auth.Credential
	Info     *ModelInfo
	Kinds    []string
	Context  *SelectionContext
}

func NewSelectionContext(lastPromptTokens int, currentModel string) (*SelectionContext, bool) {
	if lastPromptTokens <= 0 {
		return nil, false
	}
	return &SelectionContext{ContextTokens: lastPromptTokens, CurrentModel: currentModel}, true
}

func formatMoney(value *big.Rat) string {
	if value == nil {
		return "unknown"
	}
	return "$" + formatFixed(value, 2) + "/M"
}

func pricingFromModelInfo(info *ModelInfo) (input, output *big.Rat, source string) {
	if info == nil || !info.HasCostData() {
		return nil, nil, ""
	}
	return floatRat(info.CostInput), floatRat(info.CostOutput), "models.dev"
}

func (c *Catalog) knownModelsDevProvider(ctx context.Context, provider string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		return "", false
	}
	id, ok := c.modelsDevIDFor(ctx, normalized)
	return id, ok && id != ""
}

func (c *Catalog) canTrustModelInfoPricing(ctx context.Context, provider string, info *ModelInfo) bool {
	expected, ok := c.knownModelsDevProvider(ctx, provider)
	if !ok || info == nil {
		return false
	}
	actual := strings.ToLower(strings.TrimSpace(info.ProviderID))
	return actual == "" || actual == expected
}

func (c *Catalog) ExpensiveModelWarning(ctx context.Context, modelName, provider, baseURL string, key auth.Credential, info *ModelInfo) (ExpensiveModelWarning, bool) {
	ctx = c.scoped(ctx)
	model := strings.TrimSpace(modelName)
	if model == "" {
		return ExpensiveModelWarning{}, false
	}
	var input, output *big.Rat
	source := ""
	if c.canTrustModelInfoPricing(ctx, provider, info) {
		input, output, source = pricingFromModelInfo(info)
	}
	if input == nil && output == nil {
		if _, known := c.knownModelsDevProvider(ctx, provider); known {
			if found, ok := c.ModelInfo(ctx, provider, model, false, nil); ok {
				input, output, source = pricingFromModelInfo(&found)
			}
		}
	}
	if input == nil && output == nil {
		if route := ResolveBillingRoute(model, provider, baseURL); route.BillingMode != BillingUnknown {
			if entry := c.pricingEntry(ctx, route, key); entry != nil {
				input, output, source = copyRat(entry.Input), copyRat(entry.Output), entry.Source
			}
		}
	}
	confusion := strings.ToLower(model) == gpt55ProOpenRouterID
	overInput := input != nil && input.Cmp(inputCostWarningThreshold) > 0
	overOutput := output != nil && output.Cmp(outputCostWarningThreshold) > 0
	if !overInput && !overOutput && !confusion {
		return ExpensiveModelWarning{}, false
	}
	lines := []string{
		"!!! EXPENSIVE MODEL WARNING !!!",
		"",
		model + " has known pricing above icebath's safety threshold.",
		"Input tokens: " + formatMoney(input),
		"Output tokens: " + formatMoney(output),
		"Threshold: more than $20/M input tokens or more than $100/M output tokens.",
	}
	if source != "" {
		lines = append(lines, "Pricing source: "+source+".")
	}
	if confusion {
		lines = append(lines, gpt55Suggestion)
	}
	lines = append(lines, "Confirm only if you intend to use this model.")
	if source == "" {
		source = "unknown"
	}
	return ExpensiveModelWarning{
		Model: model, Provider: strings.TrimSpace(provider), Input: input, Output: output,
		Source: source, Message: strings.Join(lines, "\n"),
	}, true
}

func isMetaContributor(modelLower string) bool {
	return strings.HasSuffix(modelLower, "-contributor") || slices.Contains(strings.Split(modelLower, "-"), "contributor")
}

func DataTrainingWarningFor(modelName, provider string) (DataTrainingWarning, bool) {
	model := strings.TrimSpace(modelName)
	if model == "" || !isMetaContributor(strings.ToLower(model)) {
		return DataTrainingWarning{}, false
	}
	return DataTrainingWarning{Model: model, Provider: strings.TrimSpace(provider), Message: metaContributorMessage}, true
}

func (c *Catalog) contextCacheThreshold(ctx context.Context) int {
	if raw := c.modelConfig(ctx).SwitchContextConfirm; raw != nil {
		return max(0, *raw)
	}
	return DefaultContextCacheSwitchThreshold
}

func (c *Catalog) contextCacheGuard(ctx context.Context, q SelectionQuery) (SelectionWarning, bool) {
	if q.Context == nil || q.Context.ContextTokens == 0 {
		return SelectionWarning{}, false
	}
	target := strings.TrimSpace(q.Model)
	current := strings.TrimSpace(q.Context.CurrentModel)
	if target == "" || (current != "" && target == current) {
		return SelectionWarning{}, false
	}
	threshold := c.contextCacheThreshold(ctx)
	tokens := q.Context.ContextTokens
	if threshold <= 0 || tokens < threshold {
		return SelectionWarning{}, false
	}
	message := strings.Join([]string{
		"!!! LARGE CONTEXT MODEL SWITCH !!!",
		"",
		"This session holds ~" + groupedInt(tokens) + " tokens of context.",
		"Switching to " + target + " makes the next reply re-read all of it uncached (providers key " +
			"prompt caches per model) — a one-time full-price input cost.",
		"",
		"Threshold: model.switch_context_confirm_tokens (currently " + groupedInt(threshold) + "; 0 disables this check).",
		"Confirm only if you intend to switch now.",
	}, "\n")
	return SelectionWarning{
		Kind: WarningContextCache, Title: "Large Context Switch Warning", Model: target,
		Provider: strings.TrimSpace(q.Provider), Message: message,
	}, true
}

func (c *Catalog) SelectionWarnings(ctx context.Context, q SelectionQuery) []SelectionWarning {
	ctx = c.scoped(ctx)
	results := []SelectionWarning{}
	keep := func(w SelectionWarning) {
		if q.Kinds == nil || slices.Contains(q.Kinds, w.Kind) {
			results = append(results, w)
		}
	}
	if w, ok := c.ExpensiveModelWarning(ctx, q.Model, q.Provider, q.BaseURL, q.Key, q.Info); ok {
		keep(SelectionWarning{Kind: WarningCost, Title: "Expensive Model Warning", Model: w.Model, Provider: w.Provider, Message: w.Message})
	}
	if w, ok := DataTrainingWarningFor(q.Model, q.Provider); ok {
		keep(SelectionWarning{Kind: WarningDataPolicy, Title: "Data-Training Tier Warning", Model: w.Model, Provider: w.Provider, Message: w.Message})
	}
	if w, ok := c.contextCacheGuard(ctx, q); ok {
		keep(w)
	}
	return results
}

func CombinedMessage(warnings []SelectionWarning) string {
	messages := make([]string, 0, len(warnings))
	for _, w := range warnings {
		messages = append(messages, w.Message)
	}
	return strings.Join(messages, "\n\n")
}

func (c *Catalog) CombinedSelectionWarning(ctx context.Context, q SelectionQuery) (SelectionWarning, bool) {
	q.Kinds = nil
	warnings := c.SelectionWarnings(ctx, q)
	switch len(warnings) {
	case 0:
		return SelectionWarning{}, false
	case 1:
		return warnings[0], true
	}
	return SelectionWarning{
		Kind: WarningMultiple, Title: "Model Selection Warning", Model: warnings[0].Model,
		Provider: warnings[0].Provider, Message: CombinedMessage(warnings),
	}, true
}
