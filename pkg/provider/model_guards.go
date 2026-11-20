package provider

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// SelectionWarning is a selection time warning a surface confirms before applying a model
type SelectionWarning struct {
	Kind     string
	Title    string
	Model    string
	Provider string
	Message  string
}

// SelectionContext carries the live session facts switch aware guards need
type SelectionContext struct {
	ContextTokens int
	CurrentModel  string
}

// SelectionContextFor builds the live facts from the last billed prompt size, nil without a positive measurement
func SelectionContextFor(lastPromptTokens int, currentModel string) *SelectionContext {
	if lastPromptTokens <= 0 {
		return nil
	}
	return &SelectionContext{ContextTokens: lastPromptTokens, CurrentModel: currentModel}
}

// SelectionRequest is one model selection checked by every registered guard, nil IncludeKinds keeps every kind
type SelectionRequest struct {
	Model        string
	Provider     string
	BaseURL      string
	APIKey       string
	ModelInfo    *ModelsDevEntry
	IncludeKinds []string
	Context      *SelectionContext
}

var (
	inputCostWarningThreshold  = big.NewRat(20, 1)
	outputCostWarningThreshold = big.NewRat(100, 1)
)

// Seams the guard tests replace where the Python tests patch get_model_info and get_pricing_entry
var (
	guardModelInfoFn    = ModelsDevModelInfo
	guardPricingEntryFn = GetPricingEntry
)

const (
	gpt55ProOpenRouterID = "openai/gpt-5.5-pro"
	gpt55Suggestion      = "did you mean to select openai/gpt-5.5?"
)

// ExpensiveModelWarning is the confirmation payload for a model priced above the cost guardrail
type ExpensiveModelWarning struct {
	Model                string
	Provider             string
	InputCostPerMillion  *big.Rat
	OutputCostPerMillion *big.Rat
	Source               string
	Message              string
}

// Exact decimal of a float as Python Decimal of its str builds it
func costDecimal(v float64) *big.Rat {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'g', -1, 64))
	if !ok {
		return nil
	}
	return r
}

func formatMoney(v *big.Rat) string {
	if v == nil {
		return "unknown"
	}
	return "$" + ratFixed(v, 2) + "/M"
}

// Input and output prices of a models.dev entry with cost data, sourced to models.dev
func pricingFromModelInfo(info *ModelsDevEntry) (*big.Rat, *big.Rat, string) {
	if info == nil || !info.HasCostData() {
		return nil, nil, ""
	}
	return costDecimal(info.CostInput), costDecimal(info.CostOutput), "models.dev"
}

// The models.dev id of a known provider, empty when the provider has none
func knownModelsDevProvider(provider string) string {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	if normalized == "" {
		return ""
	}
	id, ok := ModelsDevIDFor(normalized)
	if !ok {
		return ""
	}
	return id
}

// Whether a supplied models.dev entry prices this provider rather than another vendor
func canTrustModelInfoPricing(provider string, info *ModelsDevEntry) bool {
	expected := knownModelsDevProvider(provider)
	if expected == "" || info == nil {
		return false
	}
	actual := strings.ToLower(strings.TrimSpace(info.ProviderID))
	return actual == "" || actual == expected
}

// Whether the billing route of a model is known so the pricing table may answer
func canTrustPricingLookup(model, provider, baseURL string) bool {
	return ResolveBillingRoute(model, provider, baseURL).BillingMode != "unknown"
}

// ExpensiveModelWarningFor warns when known pricing exceeds the input or output threshold, never on unknown pricing
func ExpensiveModelWarningFor(ctx context.Context, modelName, provider, baseURL, apiKey string, info *ModelsDevEntry) *ExpensiveModelWarning {
	model := strings.TrimSpace(modelName)
	if model == "" {
		return nil
	}
	var inputCost, outputCost *big.Rat
	source := ""
	if canTrustModelInfoPricing(provider, info) {
		inputCost, outputCost, source = pricingFromModelInfo(info)
	}
	unpriced := func() bool { return inputCost == nil && outputCost == nil }
	if unpriced() && knownModelsDevProvider(provider) != "" {
		if entry, ok := guardModelInfoFn(ctx, provider, model, false); ok {
			inputCost, outputCost, source = pricingFromModelInfo(&entry)
		} else {
			inputCost, outputCost, source = nil, nil, ""
		}
	}
	if unpriced() && canTrustPricingLookup(model, provider, baseURL) {
		if entry, ok := guardPricingEntryFn(ctx, model, provider, baseURL, apiKey); ok {
			inputCost, outputCost, source = entry.InputCostPerMillion, entry.OutputCostPerMillion, entry.Source
		}
	}
	gpt55ProConfusion := strings.ToLower(model) == gpt55ProOpenRouterID
	overInput := inputCost != nil && inputCost.Cmp(inputCostWarningThreshold) > 0
	overOutput := outputCost != nil && outputCost.Cmp(outputCostWarningThreshold) > 0
	if !overInput && !overOutput && !gpt55ProConfusion {
		return nil
	}
	lines := []string{
		"!!! EXPENSIVE MODEL WARNING !!!",
		"",
		model + " has known pricing above Icebath's safety threshold.",
		"Input tokens: " + formatMoney(inputCost),
		"Output tokens: " + formatMoney(outputCost),
		"Threshold: more than $20/M input tokens or more than $100/M output tokens.",
	}
	if source != "" {
		lines = append(lines, "Pricing source: "+source+".")
	}
	if gpt55ProConfusion {
		lines = append(lines, gpt55Suggestion)
	}
	lines = append(lines, "Confirm only if you intend to use this model.")
	if source == "" {
		source = "unknown"
	}
	return &ExpensiveModelWarning{
		Model: model, Provider: strings.TrimSpace(provider), InputCostPerMillion: inputCost,
		OutputCostPerMillion: outputCost, Source: source, Message: strings.Join(lines, "\n"),
	}
}

// DataTrainingWarning is the confirmation payload for a tier whose vendor trains on user data
type DataTrainingWarning struct {
	Model    string
	Provider string
	Message  string
}

// Meta contributor tier matched on the id alone so every route serving it fires
func isMetaContributor(modelLower, providerLower string) bool {
	if strings.HasSuffix(modelLower, "-contributor") {
		return true
	}
	return containsString(strings.Split(modelLower, "-"), "contributor")
}

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

type dataPolicyRule struct {
	match   func(modelLower, providerLower string) bool
	message string
}

// Data policy rules evaluated in order, the first match wins
var dataPolicyRules = []dataPolicyRule{{isMetaContributor, metaContributorMessage}}

// DataTrainingWarningFor warns when the model selects a tier that trains on user data
func DataTrainingWarningFor(modelName, provider string) *DataTrainingWarning {
	model := strings.TrimSpace(modelName)
	if model == "" {
		return nil
	}
	modelLower, providerLower := strings.ToLower(model), strings.ToLower(strings.TrimSpace(provider))
	for _, rule := range dataPolicyRules {
		if safeDataPolicyMatch(rule, modelLower, providerLower) {
			return &DataTrainingWarning{Model: model, Provider: strings.TrimSpace(provider), Message: rule.message}
		}
	}
	return nil
}

// A panicking predicate counts as no match so selection never breaks
func safeDataPolicyMatch(rule dataPolicyRule, modelLower, providerLower string) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	return rule.match(modelLower, providerLower)
}

type selectionGuard func(ctx context.Context, req SelectionRequest) *SelectionWarning

func costGuard(ctx context.Context, req SelectionRequest) *SelectionWarning {
	w := ExpensiveModelWarningFor(ctx, req.Model, req.Provider, req.BaseURL, req.APIKey, req.ModelInfo)
	if w == nil {
		return nil
	}
	return &SelectionWarning{Kind: "cost", Title: "Expensive Model Warning", Model: w.Model, Provider: w.Provider, Message: w.Message}
}

func dataPolicyGuard(ctx context.Context, req SelectionRequest) *SelectionWarning {
	w := DataTrainingWarningFor(req.Model, req.Provider)
	if w == nil {
		return nil
	}
	return &SelectionWarning{Kind: "data_policy", Title: "Data-Training Tier Warning", Model: w.Model, Provider: w.Provider, Message: w.Message}
}

// DefaultContextCacheSwitchThreshold is the context size above which a mid session switch asks first
const DefaultContextCacheSwitchThreshold = 100_000

// The model switch_context_confirm_tokens setting where zero disables, else the default
func contextCacheThreshold(ctx context.Context) int {
	modelCfg, ok := modelsConfig(ctx)["model"].(map[string]any)
	if !ok {
		return DefaultContextCacheSwitchThreshold
	}
	raw, present := modelCfg["switch_context_confirm_tokens"]
	if !present || raw == nil {
		return DefaultContextCacheSwitchThreshold
	}
	if s, isStr := raw.(string); isStr {
		n, ok := pyIntLiteral(s)
		if !ok {
			return DefaultContextCacheSwitchThreshold
		}
		return max(0, n)
	}
	n, ok := pythonInt(raw)
	if !ok {
		return DefaultContextCacheSwitchThreshold
	}
	return max(0, n)
}

// Python int of a string, surrounding whitespace, a sign and single underscores between digits allowed
func pyIntLiteral(s string) (int, bool) {
	t := strings.TrimFunc(s, pyIsSpace)
	if t != "" && (t[0] == '+' || t[0] == '-') {
		t = t[1:]
	}
	if t == "" || t[0] == '_' || t[len(t)-1] == '_' || strings.Contains(t, "__") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.ReplaceAll(strings.TrimFunc(s, pyIsSpace), "_", ""))
	return n, err == nil
}

// Integer with comma thousands separators as Python formats it
func groupThousands(n int) string {
	digits := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign, digits = "-", digits[1:]
	}
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return sign + b.String()
}

// Confirms a mid session switch that abandons a cached context at or above the threshold
func contextCacheGuard(ctx context.Context, req SelectionRequest) *SelectionWarning {
	if req.Context == nil || req.Context.ContextTokens == 0 {
		return nil
	}
	target := strings.TrimSpace(req.Model)
	current := strings.TrimSpace(req.Context.CurrentModel)
	if target == "" || (current != "" && target == current) {
		return nil
	}
	threshold := contextCacheThreshold(ctx)
	tokens := req.Context.ContextTokens
	if threshold <= 0 || tokens < threshold {
		return nil
	}
	message := strings.Join([]string{
		"!!! LARGE CONTEXT MODEL SWITCH !!!",
		"",
		fmt.Sprintf("This session holds ~%s tokens of context.", groupThousands(tokens)),
		fmt.Sprintf("Switching to %s makes the next reply re-read all of it uncached (providers key ", target) +
			"prompt caches per model) — a one-time full-price input cost.",
		"",
		fmt.Sprintf("Threshold: model.switch_context_confirm_tokens (currently %s; 0 disables this check).", groupThousands(threshold)),
		"Confirm only if you intend to switch now.",
	}, "\n")
	return &SelectionWarning{Kind: "context_cache", Title: "Large Context Switch Warning", Model: target,
		Provider: strings.TrimSpace(req.Provider), Message: message}
}

// Guard registry evaluated in order, new guard kinds are added here and never at the surfaces
var selectionGuards = []selectionGuard{costGuard, dataPolicyGuard, contextCacheGuard}

// A panicking guard is skipped so model selection never breaks
func runSelectionGuard(ctx context.Context, guard selectionGuard, req SelectionRequest) (w *SelectionWarning) {
	defer func() {
		if recover() != nil {
			w = nil
		}
	}()
	return guard(ctx, req)
}

// SelectionWarnings returns every registered guard's warning restricted to the requested kinds
func SelectionWarnings(ctx context.Context, req SelectionRequest) []SelectionWarning {
	var wanted map[string]bool
	if req.IncludeKinds != nil {
		wanted = setOf(req.IncludeKinds...)
	}
	results := []SelectionWarning{}
	for _, guard := range selectionGuards {
		w := runSelectionGuard(ctx, guard, req)
		if w != nil && (wanted == nil || wanted[w.Kind]) {
			results = append(results, *w)
		}
	}
	return results
}

// CombinedSelectionMessage joins every warning message into one confirm prompt body
func CombinedSelectionMessage(warnings []SelectionWarning) string {
	messages := make([]string, len(warnings))
	for i, w := range warnings {
		messages[i] = w.Message
	}
	return strings.Join(messages, "\n\n")
}

// CombinedSelectionWarning returns nil, the single warning, or one multiple kind warning stacking every message
func CombinedSelectionWarning(ctx context.Context, req SelectionRequest) *SelectionWarning {
	req.IncludeKinds = nil
	warnings := SelectionWarnings(ctx, req)
	switch len(warnings) {
	case 0:
		return nil
	case 1:
		return &warnings[0]
	}
	return &SelectionWarning{Kind: "multiple", Title: "Model Selection Warning", Model: warnings[0].Model,
		Provider: warnings[0].Provider, Message: CombinedSelectionMessage(warnings)}
}

// Picker entry to the guard registry for a pool environment
func modelSelectionWarnings(env PoolEnv, modelID, provider, baseURL, apiKey string, includeKinds []string) ([]SelectionWarning, error) {
	return SelectionWarnings(env.ctx(), SelectionRequest{
		Model: modelID, Provider: provider, BaseURL: baseURL, APIKey: apiKey, IncludeKinds: includeKinds,
	}), nil
}

func combinedSelectionMessage(warnings []SelectionWarning) string {
	return CombinedSelectionMessage(warnings)
}
