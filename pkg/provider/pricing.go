package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// ---- usage_pricing: cost computation ----

const (
	// IncludedNote marks a cost result that is covered by a subscription rather than a $0 rate card
	IncludedNote = "subscription-included; no provider invoice for usage"
)

// Pay per token first party APIs whose models.dev rate card is the vendor list price
var modelsDevDirectHosts = map[string]string{
	"openai": "openai.com", "xai": "x.ai", "anthropic": "anthropic.com", "google": "googleapis.com",
	"deepseek": "deepseek.com", "xiaomi": "xiaomimimo.com",
}

var (
	ratZero       = big.NewRat(0, 1)
	ratOneMillion = big.NewRat(1_000_000, 1)
	subcentLimit  = big.NewRat(1, 100)
)

// Clock stamping fetched_at on live pricing entries
var pricingNow = func() time.Time { return time.Now().UTC() }

// Metadata and sibling slice seams so pricing tests never probe a live endpoint or credential store
var (
	fetchModelMetadataFn         = FetchModelMetadata
	fetchEndpointModelMetadataFn = FetchEndpointModelMetadata
	deepinfraCatalogURLFn        = deepinfraCatalogURL
	deepinfraCatalogCachedFn     = deepinfraCatalogCached
	fetchDeepinfraModelsByTagFn  = fetchDeepinfraModelsByTag
)

// Exact decimal from a rate string, nil when unparsable
func decimalOf(value any) *big.Rat {
	if value == nil {
		return nil
	}
	var text string
	switch v := value.(type) {
	case *big.Rat:
		if v == nil {
			return nil
		}
		return new(big.Rat).Set(v)
	case string:
		text = strings.TrimSpace(v)
	case json.Number:
		text = v.String()
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil
		}
		text = strconv.FormatFloat(v, 'g', -1, 64)
	case float32:
		text = strconv.FormatFloat(float64(v), 'g', -1, 32)
	case int:
		text = strconv.Itoa(v)
	case int64:
		text = strconv.FormatInt(v, 10)
	case bool:
		if v {
			text = "1"
		} else {
			text = "0"
		}
	default:
		text = fmt.Sprint(v)
	}
	if text == "" {
		return nil
	}
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil
	}
	return r
}

// Fixed point rendering with banker's rounding at the given decimals
func ratFixed(r *big.Rat, dp int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(dp)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(scale))
	num := new(big.Int).Set(scaled.Num())
	den := scaled.Denom()
	neg := num.Sign() < 0
	num.Abs(num)
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	twice := new(big.Int).Mul(rem, big.NewInt(2))
	switch twice.Cmp(den) {
	case 1:
		quo.Add(quo, big.NewInt(1))
	case 0:
		if quo.Bit(0) == 1 {
			quo.Add(quo, big.NewInt(1))
		}
	}
	digits := quo.String()
	if dp > 0 {
		for len(digits) <= dp {
			digits = "0" + digits
		}
		digits = digits[:len(digits)-dp] + "." + digits[len(digits)-dp:]
	}
	if neg && quo.Sign() != 0 {
		digits = "-" + digits
	}
	return digits
}

// FormatCostLabel renders zero as $0.00, sub cent amounts at four places and everything else at two
func FormatCostLabel(amount *big.Rat) string {
	if amount == nil || amount.Cmp(ratZero) == 0 {
		return "$0.00"
	}
	if amount.Cmp(subcentLimit) < 0 {
		label := "~$" + ratFixed(amount, 4)
		if label == "~$0.0000" {
			return "~$<0.0001"
		}
		return label
	}
	return "~$" + ratFixed(amount, 2)
}

// CanonicalUsage is the provider neutral token bucket view of one or more responses
type CanonicalUsage struct {
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	RequestCount     int
	RawUsage         map[string]any
}

// NewCanonicalUsage returns the empty bucket counting one request
func NewCanonicalUsage() CanonicalUsage { return CanonicalUsage{RequestCount: 1} }

// PromptTokens is uncached input plus both cache buckets
func (u CanonicalUsage) PromptTokens() int {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// TotalTokens is prompt plus output
func (u CanonicalUsage) TotalTokens() int { return u.PromptTokens() + u.OutputTokens }

// Add sums two buckets, dropping raw usage and adding request counts
func (u CanonicalUsage) Add(o CanonicalUsage) CanonicalUsage {
	return CanonicalUsage{
		InputTokens: u.InputTokens + o.InputTokens, OutputTokens: u.OutputTokens + o.OutputTokens,
		CacheReadTokens: u.CacheReadTokens + o.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens + o.CacheWriteTokens,
		ReasoningTokens: u.ReasoningTokens + o.ReasoningTokens, RequestCount: u.RequestCount + o.RequestCount,
	}
}

// BillingRoute names the pricing source a model request is billed through
type BillingRoute struct {
	Provider    string
	Model       string
	BaseURL     string
	BillingMode string
}

// PricingEntry is one rate card row in USD per million tokens with optional whole request tiering
type PricingEntry struct {
	InputCostPerMillion           *big.Rat
	OutputCostPerMillion          *big.Rat
	CacheReadCostPerMillion       *big.Rat
	CacheWriteCostPerMillion      *big.Rat
	RequestCost                   *big.Rat
	Source                        string
	SourceURL                     string
	PricingVersion                string
	FetchedAt                     time.Time
	TierThresholdTokens           int
	HasTier                       bool
	InputCostPerMillionAbove      *big.Rat
	OutputCostPerMillionAbove     *big.Rat
	CacheReadCostPerMillionAbove  *big.Rat
	CacheWriteCostPerMillionAbove *big.Rat
}

// CostResult is the priced outcome of a usage bucket
type CostResult struct {
	AmountUSD      *big.Rat
	Status         string
	Source         string
	Label          string
	FetchedAt      time.Time
	PricingVersion string
	Notes          []string
}

var includedEntry = PricingEntry{
	InputCostPerMillion: ratZero, OutputCostPerMillion: ratZero, CacheReadCostPerMillion: ratZero,
	CacheWriteCostPerMillion: ratZero, Source: "none", PricingVersion: "included-route",
}

func mustRat(text string) *big.Rat {
	r, ok := new(big.Rat).SetString(text)
	if !ok {
		panic("bad rate literal " + text)
	}
	return r
}

type tierRates struct{ input, output, cacheRead, cacheWrite string }

// Official docs snapshot entry from per million rate strings
func snap(rates []string, version, url string, tier int, above tierRates) PricingEntry {
	e := PricingEntry{Source: "official_docs_snapshot", SourceURL: url, PricingVersion: version}
	e.InputCostPerMillion = mustRat(rates[0])
	e.OutputCostPerMillion = mustRat(rates[1])
	if len(rates) > 2 {
		e.CacheReadCostPerMillion = mustRat(rates[2])
	}
	if len(rates) > 3 {
		e.CacheWriteCostPerMillion = mustRat(rates[3])
	}
	if tier > 0 {
		e.HasTier = true
		e.TierThresholdTokens = tier
		if above.input != "" {
			e.InputCostPerMillionAbove = mustRat(above.input)
		}
		if above.output != "" {
			e.OutputCostPerMillionAbove = mustRat(above.output)
		}
		if above.cacheRead != "" {
			e.CacheReadCostPerMillionAbove = mustRat(above.cacheRead)
		}
		if above.cacheWrite != "" {
			e.CacheWriteCostPerMillionAbove = mustRat(above.cacheWrite)
		}
	}
	return e
}

type snapshotRow struct {
	models []string
	rates  []string
}

type snapshotBlock struct {
	provider string
	url      string
	version  string
	rows     []snapshotRow
}

const (
	bedrockPricingURL   = "https://aws.amazon.com/bedrock/pricing/"
	anthropicPricingURL = "https://platform.claude.com/docs/en/about-claude/pricing"
	googlePricingURL    = "https://ai.google.dev/pricing"
)

var (
	opusRates   = []string{"5.00", "25.00", "0.50", "6.25"}
	sonnetRates = []string{"3.00", "15.00", "0.30", "3.75"}
	opus55Rates = []string{"4.00", "20.00", "0.20", "5.00"}
)

// Snapshot of published pricing stable enough to encode exactly
var pricingSnapshots = []snapshotBlock{
	{"openai", "https://openai.com/index/previewing-gpt-5-6-sol/", "openai-gpt-5.6-2026-07", []snapshotRow{
		{[]string{"gpt-5.6-sol"}, []string{"5.00", "30.00", "0.50", "6.25"}},
		{[]string{"gpt-5.6-terra"}, []string{"2.50", "15.00", "0.25", "3.125"}},
		{[]string{"gpt-5.6-luna"}, []string{"1.00", "6.00", "0.10", "1.25"}},
	}},
	{"anthropic", anthropicPricingURL, "anthropic-pricing-2026-05", []snapshotRow{
		{[]string{"claude-opus-4-8", "claude-opus-4-7", "claude-opus-4-7-20250507", "claude-opus-4-6", "claude-opus-4-6-20250414", "claude-opus-4-5"}, opusRates},
		{[]string{"claude-sonnet-4-6", "claude-sonnet-4-6-20250414", "claude-sonnet-4-5", "claude-sonnet-4-20250514", "claude-3-5-sonnet-20241022"}, sonnetRates},
		{[]string{"claude-haiku-4-5"}, []string{"1.00", "5.00", "0.10", "1.25"}},
		{[]string{"claude-opus-4-20250514", "claude-3-opus-20240229"}, []string{"15.00", "75.00", "1.50", "18.75"}},
		{[]string{"claude-3-5-haiku-20241022"}, []string{"0.80", "4.00", "0.08", "1.00"}},
		{[]string{"claude-3-haiku-20240307"}, []string{"0.25", "1.25", "0.03", "0.30"}},
	}},
	{"anthropic", "https://openrouter.ai/anthropic/claude-opus-4.8-fast", "anthropic-pricing-2026-05", []snapshotRow{
		{[]string{"claude-opus-4-8-fast"}, []string{"10.00", "50.00", "1.00", "12.50"}},
	}},
	{"anthropic", anthropicPricingURL, "anthropic-pricing-2026-06-intro", []snapshotRow{
		{[]string{"claude-sonnet-5"}, []string{"2.00", "10.00", "0.20", "2.50"}},
	}},
	{"anthropic", anthropicPricingURL, "anthropic-pricing-2026-09", []snapshotRow{
		{[]string{"claude-opus-5"}, opusRates},
		{[]string{"claude-opus-5-5"}, opus55Rates},
	}},
	{"openai", "https://openai.com/api/pricing/", "openai-pricing-2026-03-16", []snapshotRow{
		{[]string{"gpt-4o"}, []string{"2.50", "10.00", "1.25"}}, {[]string{"gpt-4o-mini"}, []string{"0.15", "0.60", "0.075"}},
		{[]string{"gpt-4.1"}, []string{"2.00", "8.00", "0.50"}}, {[]string{"gpt-4.1-mini"}, []string{"0.40", "1.60", "0.10"}},
		{[]string{"gpt-4.1-nano"}, []string{"0.10", "0.40", "0.025"}}, {[]string{"o3"}, []string{"10.00", "40.00", "2.50"}},
		{[]string{"o3-mini"}, []string{"1.10", "4.40", "0.55"}},
	}},
	{"deepseek", "https://api-docs.deepseek.com/quick_start/pricing", "deepseek-pricing-2026-09-10", []snapshotRow{
		{[]string{"deepseek-flash", "deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"}, []string{"0.15", "0.60", "0.003"}},
		{[]string{"deepseek-v4-pro"}, []string{"0.66", "1.98", "0.022"}},
	}},
	{"google", "https://ai.google.dev/gemini-api/docs/pricing", "google-pricing-2026-10-13", []snapshotRow{
		{[]string{"gemini-4-argon"}, []string{"2.00", "10.00", "0.10"}},
	}},
	{"google", "https://ai.google.dev/gemini-api/docs/pricing", "google-pricing-2026-09-02", []snapshotRow{
		{[]string{"gemini-3.8-flash", "gemini-3.7-flash"}, []string{"0.75", "3.75", "0.075"}},
	}},
	{"google", "https://ai.google.dev/gemini-api/docs/pricing", "google-pricing-2026-07-28", []snapshotRow{
		{[]string{"gemini-3.6-flash"}, []string{"1.50", "7.50", "0.15"}}, {[]string{"gemini-3.5-flash-lite"}, []string{"0.30", "2.50", "0.03"}},
	}},
	{"google", googlePricingURL, "google-pricing-2026-07-07", []snapshotRow{
		{[]string{"gemini-3.5-flash"}, []string{"1.50", "9.00", "0.15"}}, {[]string{"gemini-3.1-flash-lite"}, []string{"0.25", "1.50", "0.025"}},
		{[]string{"gemini-3-pro-preview"}, []string{"2.00", "12.00", "0.20"}}, {[]string{"gemini-3-flash-preview"}, []string{"0.50", "3.00", "0.05"}},
		{[]string{"gemini-2.5-flash"}, []string{"0.15", "0.60", "0.015"}}, {[]string{"gemini-2.0-flash"}, []string{"0.10", "0.40", "0.01"}},
	}},
	{"bedrock", bedrockPricingURL, "anthropic-list-2026-07", []snapshotRow{
		{[]string{"anthropic.claude-opus-4-8", "anthropic.claude-opus-4-7", "anthropic.claude-opus-4-6"}, opusRates},
	}},
	{"bedrock", bedrockPricingURL, "anthropic-list-2026-09", []snapshotRow{
		{[]string{"anthropic.claude-opus-5"}, opusRates}, {[]string{"anthropic.claude-opus-5-5"}, opus55Rates},
	}},
	{"bedrock", bedrockPricingURL, "bedrock-pricing-2026-06", []snapshotRow{{[]string{"anthropic.claude-sonnet-5"}, sonnetRates}}},
	{"bedrock", bedrockPricingURL, "bedrock-pricing-2026-04", []snapshotRow{
		{[]string{"anthropic.claude-sonnet-4-6", "anthropic.claude-sonnet-4-5"}, sonnetRates},
		{[]string{"anthropic.claude-haiku-4-5"}, []string{"0.80", "4.00", "0.08", "1.00"}},
		{[]string{"amazon.nova-pro"}, []string{"0.80", "3.20"}}, {[]string{"amazon.nova-lite"}, []string{"0.06", "0.24"}}, {[]string{"amazon.nova-micro"}, []string{"0.035", "0.14"}},
	}},
	{"minimax", "", "minimax-pricing-2026-04", []snapshotRow{{[]string{"minimax-m2.7"}, []string{"0.30", "1.20"}}}},
	{"minimax-cn", "", "minimax-pricing-2026-04", []snapshotRow{{[]string{"minimax-m2.7"}, []string{"0.30", "1.20"}}}},
	{"fireworks", "https://docs.fireworks.ai/serverless/pricing", "fireworks-pricing-2026-07", []snapshotRow{
		{[]string{"kimi-k2p6"}, []string{"0.95", "4.00", "0.16"}}, {[]string{"kimi-k2p7-code"}, []string{"0.95", "4.00", "0.19"}},
		{[]string{"glm-5p2"}, []string{"1.40", "4.40", "0.14"}}, {[]string{"deepseek-v4-pro"}, []string{"1.74", "3.48", "0.145"}},
		{[]string{"deepseek-v4-flash"}, []string{"0.14", "0.28", "0.028"}}, {[]string{"qwen3p7-plus"}, []string{"0.40", "1.60", "0.08"}},
		{[]string{"minimax-m3"}, []string{"0.30", "1.20", "0.06"}}, {[]string{"gpt-oss-120b"}, []string{"0.15", "0.60", "0.015"}},
		{[]string{"gpt-oss-20b"}, []string{"0.07", "0.30", "0.035"}}, {[]string{"glm-5p1"}, []string{"1.40", "4.40", "0.26"}},
		{[]string{"minimax-m2p7"}, []string{"0.30", "1.20", "0.06"}},
		{[]string{"kimi-k2p6-fast", "kimi-k2p6-turbo"}, []string{"2.00", "8.00", "0.30"}},
		{[]string{"kimi-k2p7-code-fast"}, []string{"1.90", "8.00", "0.38"}}, {[]string{"glm-5p2-fast"}, []string{"2.10", "6.60", "0.21"}},
		{[]string{"glm-5p1-fast"}, []string{"2.80", "8.80", "0.52"}},
	}},
}

type pricingKey struct{ provider, model string }

// Builds the official docs table from the snapshots plus the tiered and aliased rows
func buildOfficialDocsPricing() map[pricingKey]PricingEntry {
	table := map[pricingKey]PricingEntry{}
	for _, block := range pricingSnapshots {
		for _, row := range block.rows {
			entry := snap(row.rates, block.version, block.url, 0, tierRates{})
			for _, model := range row.models {
				table[pricingKey{block.provider, model}] = entry
			}
		}
	}
	table[pricingKey{"openai", "gpt-6-astra"}] = snap([]string{"10.00", "50.00", "1.00", "12.50"}, "openai-gpt-6-astra-2026-09",
		"https://developers.openai.com/api/docs/models/gpt-6-astra", 272_000, tierRates{"20.00", "75.00", "2.00", "25.00"})
	for _, row := range []struct {
		slug                                      string
		inp, out, read, write                     string
		inpAbove, outAbove, readAbove, writeAbove string
	}{
		{"gpt-6-sol", "2.00", "10.00", "0.20", "2.50", "4.00", "15.00", "0.40", "5.00"},
		{"gpt-6.1-sol", "2.00", "10.00", "0.10", "2.50", "4.00", "15.00", "0.20", "5.00"},
		{"gpt-6-luna", "0.10", "0.50", "0.01", "0.125", "0.20", "0.75", "0.02", "0.25"},
	} {
		table[pricingKey{"openai", row.slug}] = snap([]string{row.inp, row.out, row.read, row.write}, "openai-gpt-6-tiers-2026-09",
			"https://developers.openai.com/api/docs/models/"+row.slug, 272_000, tierRates{row.inpAbove, row.outAbove, row.readAbove, row.writeAbove})
	}
	table[pricingKey{"google", "gemini-3.1-pro"}] = snap([]string{"2.00", "12.00", "0.20"}, "google-pricing-2026-07-07", googlePricingURL,
		200_000, tierRates{"4.00", "18.00", "0.40", ""})
	table[pricingKey{"google", "gemini-2.5-pro"}] = snap([]string{"1.25", "10.00", "0.125"}, "google-pricing-2026-07-07", googlePricingURL,
		200_000, tierRates{"2.50", "15.00", "", ""})
	for _, m := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna", "gpt-6.1-sol"} {
		for _, suffix := range []string{"pro", "900k"} {
			table[pricingKey{"openai", m + "-" + suffix}] = table[pricingKey{"openai", m}]
		}
	}
	table[pricingKey{"google", "gemini-3.1-pro-preview"}] = table[pricingKey{"google", "gemini-3.1-pro"}]
	table[pricingKey{"google", "gemini-3.1-flash-lite-preview"}] = table[pricingKey{"google", "gemini-3.1-flash-lite"}]
	return table
}

var officialDocsPricing = buildOfficialDocsPricing()

// OpenAI Ultrafast tier, six times standard on every bucket with the same 272K whole request tier
var openAIUltrafastPricing = map[string]PricingEntry{
	"gpt-6-astra": snap([]string{"60.00", "300.00", "6.00", "75.00"}, "openai-ultrafast-2026-09",
		"https://developers.openai.com/api/docs/pricing?latest-pricing=ultrafast", 272_000, tierRates{"120.00", "450.00", "12.00", "150.00"}),
	"gpt-6.1-sol": snap([]string{"12.00", "60.00", "0.60", "15.00"}, "openai-ultrafast-2026-10-08",
		"https://developers.openai.com/api/docs/pricing?latest-pricing=ultrafast", 272_000, tierRates{"24.00", "90.00", "1.20", "30.00"}),
}

// Anthropic fast mode premium rows selected per response by usage.speed
var anthropicFastModePricing = func() map[string]PricingEntry {
	out := map[string]PricingEntry{}
	for _, m := range []string{"claude-opus-4-8", "claude-opus-5"} {
		out[m] = snap([]string{"10.00", "50.00", "1.00", "12.50"}, "anthropic-fast-mode-2026-09", anthropicPricingURL+"#fast-mode-pricing", 0, tierRates{})
	}
	out["claude-opus-5-5"] = snap([]string{"8.00", "40.00", "0.40", "10.00"}, "anthropic-fast-mode-2026-09", anthropicPricingURL+"#fast-mode-pricing", 0, tierRates{})
	return out
}()

// Non negative int at a nested path of a usage map, zero when any hop is falsy or non numeric
func usageField(obj any, path ...string) int {
	for _, hop := range path {
		if !truthy(obj) {
			return 0
		}
		m, ok := asMap(obj)
		if !ok {
			return 0
		}
		obj = m[hop]
	}
	if obj == nil {
		return 0
	}
	n, ok := pythonInt(obj)
	if !ok || n < 0 {
		return 0
	}
	return n
}

// First non zero usage field across candidate paths
func firstNonzero(obj any, paths ...[]string) int {
	for _, path := range paths {
		if v := usageField(obj, path...); v != 0 {
			return v
		}
	}
	return 0
}

var snapshotProviderAliases = map[string]string{
	"anthropic": "anthropic", "openai": "openai", "openai-api": "openai", "minimax": "minimax", "minimax-cn": "minimax-cn",
}

var googleProviderNames = map[string]bool{"google": true, "gemini": true, "vertex": true, "google-gemini": true, "google-ai-studio": true, "google-vertex": true, "vertex-ai": true}

// ResolveBillingRoute maps a model, provider and base URL onto the pricing source that bills it
func ResolveBillingRoute(modelName, provider, baseURL string) BillingRoute {
	providerName := strings.ToLower(strings.TrimSpace(provider))
	base := strings.ToLower(strings.TrimSpace(baseURL))
	model := strings.TrimSpace(modelName)
	if providerName == "" && strings.Contains(model, "/") {
		parts := strings.SplitN(model, "/", 2)
		if parts[0] == "anthropic" || parts[0] == "openai" || parts[0] == "google" {
			providerName, model = parts[0], parts[1]
		}
	}
	url := baseURL
	segments := strings.Split(model, "/")
	bare := segments[len(segments)-1]
	host := func(name string) bool { return HostMatches(url, name) }
	if providerName == "openai-codex" {
		return BillingRoute{"openai-codex", model, url, "subscription_included"}
	}
	if providerName == "openrouter" || host("openrouter.ai") {
		return BillingRoute{"openrouter", model, url, "official_models_api"}
	}
	snapshotProvider := snapshotProviderAliases[providerName]
	if snapshotProvider == "" {
		if googleProviderNames[providerName] || host("aiplatform.googleapis.com") || host("generativelanguage.googleapis.com") {
			snapshotProvider = "google"
		} else if providerName == "fireworks" || host("api.fireworks.ai") {
			snapshotProvider = "fireworks"
		}
	}
	if snapshotProvider != "" {
		if snapshotProvider == "openai" && (url == "" || host("api.openai.com")) {
			bare = StripCodexContextVariantSuffix(bare)
		}
		return BillingRoute{snapshotProvider, bare, url, "official_docs_snapshot"}
	}
	if providerName == "custom" || providerName == "local" || (base != "" && (Hostname(base) == "localhost" || Hostname(base) == "127.0.0.1")) {
		p := providerName
		if p == "" {
			p = "custom"
		}
		return BillingRoute{p, model, url, "unknown"}
	}
	p := providerName
	if p == "" {
		p = "unknown"
	}
	if model == "" {
		bare = ""
	}
	return BillingRoute{p, bare, url, "unknown"}
}

var bedrockTrailers = []*regexp.Regexp{regexp.MustCompile(`:\d+$`), regexp.MustCompile(`-v\d+$`), regexp.MustCompile(`-\d{8}$`)}

var dottedVersionPricingRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// Drops the first matching prefix, at most one
func stripFirstPrefix(name string, prefixes []string) string {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return name[len(p):]
		}
	}
	return name
}

// Bare Bedrock foundation model id without region scope, dotted versions or trailing revisions
func normalizeBedrockModelName(model string) string {
	name := dottedVersionPricingRe.ReplaceAllString(stripFirstPrefix(strings.TrimSpace(strings.ToLower(model)), bedrockRegionPrefixes), "$1-$2")
	for _, re := range bedrockTrailers {
		name = re.ReplaceAllString(name, "")
	}
	return name
}

// Anthropic id without the vendor prefix and with dotted versions hyphenated
func normalizeAnthropicModelName(model string) string {
	return dottedVersionPricingRe.ReplaceAllString(stripFirstPrefix(strings.TrimSpace(strings.ToLower(model)), []string{"anthropic/"}), "$1-$2")
}

var pricingModelNormalizers = map[string]func(string) string{"anthropic": normalizeAnthropicModelName, "bedrock": normalizeBedrockModelName}

// Snapshot entry for a route, retrying with the provider's normalized model name
func lookupOfficialDocsPricing(route BillingRoute) (PricingEntry, bool) {
	model := strings.ToLower(route.Model)
	if entry, ok := officialDocsPricing[pricingKey{route.Provider, model}]; ok {
		return entry, true
	}
	normalize := pricingModelNormalizers[route.Provider]
	if normalize == nil {
		return PricingEntry{}, false
	}
	normalized := normalize(model)
	if normalized == model {
		return PricingEntry{}, false
	}
	entry, ok := officialDocsPricing[pricingKey{route.Provider, normalized}]
	return entry, ok
}

// WithServedServiceTier folds the response level OpenAI service tier into the raw usage
func WithServedServiceTier(usage CanonicalUsage, serviceTier string) CanonicalUsage {
	tier := strings.TrimSpace(serviceTier)
	if tier == "" {
		return usage
	}
	raw := make(map[string]any, len(usage.RawUsage)+1)
	for k, v := range usage.RawUsage {
		raw[k] = v
	}
	raw["service_tier"] = strings.ToLower(tier)
	usage.RawUsage = raw
	return usage
}

func servedOpenAITier(usage CanonicalUsage) string {
	if usage.RawUsage == nil {
		return ""
	}
	tier, _ := usage.RawUsage["service_tier"].(string)
	return tier
}

// Anthropic names the speed that served a fast mode request in usage.speed
func servedFast(usage CanonicalUsage) bool {
	return usage.RawUsage != nil && usage.RawUsage["speed"] == "fast"
}

func anthropicFastModeEntry(model string) (PricingEntry, bool) {
	name := strings.ToLower(model)
	if e, ok := anthropicFastModePricing[name]; ok {
		return e, true
	}
	e, ok := anthropicFastModePricing[normalizeAnthropicModelName(name)]
	return e, ok
}

// Pricing entry from an OpenRouter shaped metadata row with per token rates scaled to per million
func pricingEntryFromMetadata(metadata map[string]map[string]any, modelID, sourceURL, pricingVersion string) (PricingEntry, bool) {
	row, ok := metadata[modelID]
	if !ok {
		return PricingEntry{}, false
	}
	pricing := dictOrEmpty(row["pricing"])
	perMillion := func(key string, aliases ...string) *big.Rat {
		raw := pricing[key]
		for _, alias := range aliases {
			if !truthy(raw) {
				raw = pricing[alias]
			}
		}
		value := decimalOf(raw)
		if value == nil {
			return nil
		}
		return value.Mul(value, ratOneMillion)
	}
	prompt := perMillion("prompt")
	completion := perMillion("completion")
	request := decimalOf(pricing["request"])
	if prompt == nil && completion == nil && request == nil {
		return PricingEntry{}, false
	}
	return PricingEntry{
		InputCostPerMillion: prompt, OutputCostPerMillion: completion,
		CacheReadCostPerMillion:  perMillion("cache_read", "cached_prompt", "input_cache_read"),
		CacheWriteCostPerMillion: perMillion("cache_write", "cache_creation", "input_cache_write"),
		RequestCost:              request, Source: "provider_models_api", SourceURL: sourceURL,
		PricingVersion: pricingVersion, FetchedAt: pricingNow(),
	}, true
}

func openRouterPricingEntry(ctx context.Context, route BillingRoute) (PricingEntry, bool) {
	return pricingEntryFromMetadata(fetchModelMetadataFn(ctx, false), route.Model,
		"https://openrouter.ai/docs/api/api-reference/models/get-models", "openrouter-models-api")
}

func ratFromFloat(f float64) *big.Rat { return decimalOf(f) }

func ratFromFloatPtr(f *float64) *big.Rat {
	if f == nil {
		return nil
	}
	return decimalOf(*f)
}

// models.dev list price for a direct first party route on https 443 to the vendor domain
func modelsDevPricingEntry(ctx context.Context, route BillingRoute) (PricingEntry, bool) {
	domain := modelsDevDirectHosts[route.Provider]
	if domain == "" || route.Model == "" {
		return PricingEntry{}, false
	}
	if route.BaseURL != "" {
		scheme, host, port := baseURLOrigin(route.BaseURL)
		if scheme != "https" || port != "443" || !(host == domain || strings.HasSuffix(host, "."+domain)) {
			return PricingEntry{}, false
		}
	}
	info, ok := ModelsDevProfile(ctx, route.Provider, route.Model)
	if !ok || !info.HasCostData() {
		return PricingEntry{}, false
	}
	return PricingEntry{
		InputCostPerMillion: ratFromFloat(info.CostInput), OutputCostPerMillion: ratFromFloat(info.CostOutput),
		CacheReadCostPerMillion: ratFromFloatPtr(info.CostCacheRead), CacheWriteCostPerMillion: ratFromFloatPtr(info.CostCacheWrite),
		Source: "provider_models_api", SourceURL: "https://models.dev", PricingVersion: "models.dev", FetchedAt: pricingNow(),
	}, true
}

// GetPricingEntry resolves the rate card for a model route from snapshot, endpoint metadata or models.dev
func GetPricingEntry(ctx context.Context, modelName, provider, baseURL, apiKey string) (PricingEntry, bool) {
	route := ResolveBillingRoute(modelName, provider, baseURL)
	if route.BillingMode == "subscription_included" {
		return includedEntry, true
	}
	if route.Provider == "openrouter" {
		return openRouterPricingEntry(ctx, route)
	}
	if bundled, ok := lookupOfficialDocsPricing(route); ok {
		return bundled, true
	}
	if route.BaseURL != "" {
		entry, ok := pricingEntryFromMetadata(fetchEndpointModelMetadataFn(ctx, route.BaseURL, apiKey, false), route.Model,
			strings.TrimRight(route.BaseURL, "/")+"/models", "openai-compatible-models-api")
		if ok {
			return entry, true
		}
	}
	return modelsDevPricingEntry(ctx, route)
}

type usageShape [][][]string

var anthropicUsageShape = usageShape{
	{{"input_tokens"}}, {{"output_tokens"}}, {{"cache_read_input_tokens"}}, {{"cache_creation_input_tokens"}},
}

var codexUsageShape = usageShape{
	{{"input_tokens"}}, {{"output_tokens"}}, {{"input_tokens_details", "cached_tokens"}},
	{{"input_tokens_details", "cache_write_tokens"}, {"input_tokens_details", "cache_creation_tokens"}},
}

var chatUsageShape = usageShape{
	{{"prompt_tokens"}, {"input_tokens"}},
	{{"completion_tokens"}, {"output_tokens"}},
	{{"prompt_tokens_details", "cached_tokens"}, {"cache_read_input_tokens"}, {"prompt_cache_hit_tokens"}, {"cached_tokens"}},
	{{"prompt_tokens_details", "cache_write_tokens"}, {"prompt_tokens_details", "cache_creation_input_tokens"}, {"cache_creation_input_tokens"}, {"cache_write_tokens"}},
}

// Usage payload as a map, structs and JSON marshalable objects converted through JSON
func usageMapOf(responseUsage any) map[string]any {
	if m, ok := asMap(responseUsage); ok {
		return m
	}
	if responseUsage == nil {
		return nil
	}
	raw, err := json.Marshal(responseUsage)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// NormalizeUsage turns a raw API usage payload into canonical token buckets by wire shape
func NormalizeUsage(responseUsage any, provider, apiMode string) CanonicalUsage {
	u := usageMapOf(responseUsage)
	if len(u) == 0 {
		return NewCanonicalUsage()
	}
	providerName := strings.ToLower(strings.TrimSpace(provider))
	mode := strings.ToLower(strings.TrimSpace(apiMode))
	shape := chatUsageShape
	anthropic := false
	if mode == "anthropic_messages" || providerName == "anthropic" {
		shape, anthropic = anthropicUsageShape, true
	} else if mode == "codex_responses" {
		shape = codexUsageShape
	}
	promptTotal := firstNonzero(u, shape[0]...)
	outputTokens := firstNonzero(u, shape[1]...)
	cacheRead := firstNonzero(u, shape[2]...)
	cacheWrite := firstNonzero(u, shape[3]...)
	inputTokens := promptTotal
	if !anthropic {
		inputTokens = promptTotal - cacheRead - cacheWrite
		if inputTokens < 0 {
			inputTokens = 0
		}
	}
	reasoning := firstNonzero(u, []string{"output_tokens_details", "reasoning_tokens"}, []string{"completion_tokens_details", "reasoning_tokens"})
	if (providerName == "minimax" || providerName == "minimax-cn") && mode == "anthropic_messages" {
		pkgLog().Debug(fmt.Sprintf("cache_observability provider=%s mode=%s input_tokens=%d output_tokens=%d cache_read_tokens=%d cache_write_tokens=%d (note: on MiniMax-M3 cache_read carries a +128 constant floor and is not a reliable hit signal - track input_tokens drops across calls instead)",
			providerName, mode, inputTokens, outputTokens, cacheRead, cacheWrite))
	}
	raw := make(map[string]any, len(u))
	for k, v := range u {
		raw[k] = v
	}
	return CanonicalUsage{InputTokens: inputTokens, OutputTokens: outputTokens, CacheReadTokens: cacheRead, CacheWriteTokens: cacheWrite,
		ReasoningTokens: reasoning, RequestCount: 1, RawUsage: raw}
}

func unknownCost(source string, notes ...string) CostResult {
	return CostResult{Status: "unknown", Source: source, Label: "n/a", Notes: notes}
}

// UsageCostHook lets a provider profile report the cost its own API returned for a response
type UsageCostHook func(model string, usage CanonicalUsage) (CostResult, bool)

func reportedUsageCost(provider, model string, usage CanonicalUsage) (CostResult, bool) {
	if strings.TrimSpace(provider) == "" {
		return CostResult{}, false
	}
	p := getProviderProfile(context.Background(), NormalizeProvider(provider))
	if p == nil || p.GetUsageCostHook == nil {
		return CostResult{}, false
	}
	return p.GetUsageCostHook(model, usage)
}

// EstimateUsageCost prices a usage bucket for a model route with tier, fast mode and ultrafast rules
func EstimateUsageCost(ctx context.Context, modelName string, usage CanonicalUsage, provider, baseURL, apiKey string) CostResult {
	if reported, ok := reportedUsageCost(provider, modelName, usage); ok {
		return reported
	}
	route := ResolveBillingRoute(modelName, provider, baseURL)
	if route.BillingMode == "subscription_included" {
		return CostResult{AmountUSD: new(big.Rat), Status: "included", Source: "none", Label: "included", PricingVersion: "included-route", Notes: []string{IncludedNote}}
	}
	entry, ok := GetPricingEntry(ctx, modelName, provider, baseURL, apiKey)
	if route.Provider == "anthropic" && servedFast(usage) {
		entry, ok = anthropicFastModeEntry(route.Model)
		if !ok {
			return unknownCost("official_docs_snapshot", "fast-mode pricing unavailable for model")
		}
	}
	if route.Provider == "openai" && servedOpenAITier(usage) == "ultrafast" {
		entry, ok = openAIUltrafastPricing[route.Model]
		if !ok {
			return unknownCost("official_docs_snapshot", "ultrafast pricing unavailable for model")
		}
	}
	if !ok {
		return unknownCost("none")
	}
	above := entry.HasTier && usage.PromptTokens() > entry.TierThresholdTokens
	amount := new(big.Rat)
	for _, bucket := range []struct {
		tokens    int
		rate      *big.Rat
		rateAbove *big.Rat
		note      []string
	}{
		{usage.InputTokens, entry.InputCostPerMillion, entry.InputCostPerMillionAbove, nil},
		{usage.OutputTokens, entry.OutputCostPerMillion, entry.OutputCostPerMillionAbove, nil},
		{usage.CacheReadTokens, entry.CacheReadCostPerMillion, entry.CacheReadCostPerMillionAbove, []string{"cache-read pricing unavailable for route"}},
		{usage.CacheWriteTokens, entry.CacheWriteCostPerMillion, entry.CacheWriteCostPerMillionAbove, []string{"cache-write pricing unavailable for route"}},
	} {
		rate := bucket.rate
		if above && bucket.rateAbove != nil {
			rate = bucket.rateAbove
		}
		if rate == nil {
			if bucket.tokens != 0 {
				return unknownCost(entry.Source, bucket.note...)
			}
			continue
		}
		part := new(big.Rat).Mul(big.NewRat(int64(bucket.tokens), 1), rate)
		amount.Add(amount, part.Quo(part, ratOneMillion))
	}
	if entry.RequestCost != nil && usage.RequestCount != 0 {
		amount.Add(amount, new(big.Rat).Mul(big.NewRat(int64(usage.RequestCount), 1), entry.RequestCost))
	}
	var notes []string
	status := "estimated"
	label := FormatCostLabel(amount)
	if entry.Source == "none" && amount.Cmp(ratZero) == 0 {
		status = "included"
		label = "included"
		notes = append(notes, IncludedNote)
	}
	if route.Provider == "openrouter" {
		notes = append(notes, "OpenRouter cost is estimated from the models API until reconciled.")
	}
	return CostResult{AmountUSD: amount, Status: status, Source: entry.Source, Label: label, FetchedAt: entry.FetchedAt, PricingVersion: entry.PricingVersion, Notes: notes}
}

// HasKnownPricing reports whether any rate card covers the model route
func HasKnownPricing(ctx context.Context, modelName, provider, baseURL, apiKey string) bool {
	_, ok := GetPricingEntry(ctx, modelName, provider, baseURL, apiKey)
	return ok
}

// FormatDurationCompact renders seconds as s, m, h m or fractional days
func FormatDurationCompact(seconds float64) string {
	if seconds < 60 {
		return fmt.Sprintf("%.0fs", seconds)
	}
	minutes := seconds / 60
	if minutes < 60 {
		return fmt.Sprintf("%.0fm", minutes)
	}
	hours := minutes / 60
	if hours < 24 {
		remaining := int(math.Mod(minutes, 60))
		if remaining != 0 {
			return fmt.Sprintf("%dh %dm", int(hours), remaining)
		}
		return fmt.Sprintf("%dh", int(hours))
	}
	return fmt.Sprintf("%.1fd", hours/24)
}

// ---- models_pricing: live catalog pricing with per endpoint and per credential caches ----

// PricingTable maps model id to its picker shaped pricing row
type PricingTable = map[string]map[string]any

const (
	failedCatalogTTL      = 120 * time.Second
	pricingAuthKeyPrefix  = "\x00auth:"
	openRouterPricingBase = "https://openrouter.ai/api"
	fireworksPricingKey   = "models.dev/fireworks"
	// AIGatewayBaseURL is the Vercel AI Gateway OpenAI compatible root
	AIGatewayBaseURL = home.AIGatewayBaseURL
)

var (
	pricingCacheMu          sync.Mutex
	pricingCache            = map[string]PricingTable{}
	pricingCacheOrder       []string
	pricingCacheRetryAfter  = map[string]time.Time{}
	pricingProviderCacheKey = map[[2]string]string{}
)

// Clock behind the pricing cache deadlines
var pricingClock = time.Now

// JSON GET seam for catalog endpoints, nil on any failure
var pricingGetJSON = pricingGetJSONHTTP

func pricingCacheRemember(key string) {
	for _, k := range pricingCacheOrder {
		if k == key {
			return
		}
	}
	pricingCacheOrder = append(pricingCacheOrder, key)
}

func pricingCacheForget(key string) {
	delete(pricingCache, key)
	delete(pricingCacheRetryAfter, key)
	for i, k := range pricingCacheOrder {
		if k == key {
			pricingCacheOrder = append(pricingCacheOrder[:i], pricingCacheOrder[i+1:]...)
			return
		}
	}
}

// Cached catalog for a key, nil to go fetch it, dropping an entry past its deadline
func cachedCatalog(key string) PricingTable {
	pricingCacheMu.Lock()
	defer pricingCacheMu.Unlock()
	cached, ok := pricingCache[key]
	if !ok {
		return nil
	}
	if deadline, has := pricingCacheRetryAfter[key]; has && !pricingClock().Before(deadline) {
		pricingCacheForget(key)
		return nil
	}
	return cached
}

// Caches a catalog with an expiry for empty results or a caller chosen ttl, sweeping expired rows
func cacheCatalog(key string, result PricingTable, ttl time.Duration) PricingTable {
	pricingCacheMu.Lock()
	defer pricingCacheMu.Unlock()
	now := pricingClock()
	for k, deadline := range pricingCacheRetryAfter {
		if !now.Before(deadline) {
			pricingCacheForget(k)
		}
	}
	if result == nil {
		result = PricingTable{}
	}
	pricingCache[key] = result
	pricingCacheRemember(key)
	switch {
	case len(result) == 0:
		pricingCacheRetryAfter[key] = now.Add(failedCatalogTTL)
	case ttl > 0:
		pricingCacheRetryAfter[key] = now.Add(ttl)
	default:
		delete(pricingCacheRetryAfter, key)
	}
	return result
}

// Cache key suffix identifying the credential a catalog was read with
func pricingAuthFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return pricingAuthKeyPrefix + hex.EncodeToString(sum[:8])
}

// FormatPricePerMtok renders a per token price string as dollars per million tokens
func FormatPricePerMtok(perToken string) string {
	val, err := strconv.ParseFloat(strings.TrimSpace(perToken), 64)
	if err != nil {
		return "?"
	}
	if val == 0 {
		return "free"
	}
	perM := val * 1_000_000
	text := fmt.Sprintf("%.2f", perM)
	if perM < 0.01 {
		prec := 3
		for prec < 12 && roundTo(perM, prec) == 0 {
			prec++
		}
		shown := prec + 1
		if shown > 12 {
			shown = 12
		}
		text = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.*f", shown, perM), "0"), ".")
	}
	return "$" + text
}

func roundTo(v float64, places int) float64 {
	scale := math.Pow(10, float64(places))
	return math.RoundToEven(v*scale) / scale
}

// Finite non negative price as a float, false when unset or invalid
func priceFloat(raw any) (float64, bool) {
	if raw == nil {
		return 0, false
	}
	if s, ok := raw.(string); ok {
		if s == "" {
			return 0, false
		}
		n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil {
			return 0, false
		}
		raw = n
	}
	n, ok := floatOf(raw)
	if !ok || math.IsNaN(n) {
		return 0, false
	}
	if n < 0 {
		return 0, false
	}
	return n, true
}

func rawPriceString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return fmt.Sprint(v)
}

// GET a catalog URL as JSON through open_credentialed_url or plain urlopen, headers in recorded order, nil on any failure
func pricingGetJSONHTTP(url string, headers map[string]string, timeout time.Duration, credentialed bool) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	header := http.Header{}
	for k, v := range headers {
		header.Set(k, v)
	}
	resp, err := urllibOpen(ctx, urllibCall{url: url, headers: header, credentialed: credentialed, timeout: timeout})
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var data any
	if json.Unmarshal(raw, &data) != nil {
		return nil
	}
	m, ok := asMap(data)
	if !ok {
		return nil
	}
	return m
}

// Picker shaped row from a catalog pricing block already using the cache field names
func pricingRow(pricing map[string]any, promptKey, completionKey string) map[string]any {
	entry := map[string]any{"prompt": rawPriceString(pricing[promptKey]), "completion": rawPriceString(pricing[completionKey])}
	for _, key := range []string{"input_cache_read", "input_cache_write"} {
		if truthy(pricing[key]) {
			entry[key] = rawPriceString(pricing[key])
		}
	}
	return entry
}

// Dollars per million to the per token price string the picker expects
func perToken(perMtok any) string {
	f, _ := floatOf(perMtok)
	return strconv.FormatFloat(f/1_000_000, 'g', -1, 64)
}

func pricingCatalogItems(payload map[string]any) []map[string]any {
	list, _ := asList(payload["data"])
	var out []map[string]any
	for _, item := range list {
		if m, ok := asMap(item); ok {
			out = append(out, m)
		}
	}
	return out
}

// Mirrors a fetched /v1/models document into the reasoning capability cache for its URL
func seedReasoningCaps(ctx context.Context, url string, items any) bool {
	list, ok := asList(items)
	if !ok {
		return false
	}
	c := catalogFor(url)
	caps := c.parseItems(list)
	if len(caps) == 0 {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = caps
	c.saveDisk(ctx, caps)
	return true
}

// PricingFetchOptions control one OpenRouter compatible pricing fetch
type PricingFetchOptions struct {
	APIKey       string
	BaseURL      string
	Timeout      time.Duration
	ForceRefresh bool
	CacheTTL     time.Duration
}

// FetchModelsWithPricing reads /v1/models pricing cached per base URL and per credential
func FetchModelsWithPricing(ctx context.Context, opts PricingFetchOptions) PricingTable {
	if opts.BaseURL == "" {
		opts.BaseURL = openRouterPricingBase
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 8 * time.Second
	}
	root := strings.TrimRight(opts.BaseURL, "/")
	cacheKey := root + pricingAuthFingerprint(opts.APIKey)
	if !opts.ForceRefresh {
		if cached := cachedCatalog(cacheKey); cached != nil {
			return cached
		}
	}
	url := root + "/v1/models"
	lines := []headerField{{Name: "Accept", Value: "application/json"}, {Name: "User-Agent", Value: userAgent("icebath-cli")}}
	if opts.APIKey != "" {
		lines = append(lines, headerField{Name: "Authorization", Value: "Bearer " + opts.APIKey})
	}
	payload := pricingGetJSON(url, fieldMap(lines...), opts.Timeout, true)
	if payload == nil {
		return cacheCatalog(cacheKey, PricingTable{}, 0)
	}
	seedReasoningCaps(ctx, url, payload["data"])
	result := PricingTable{}
	for _, item := range pricingCatalogItems(payload) {
		mid := stringOf(item["id"])
		pricing, ok := asMap(item["pricing"])
		if mid == "" || !ok {
			continue
		}
		result[mid] = pricingRow(pricing, "prompt", "completion")
	}
	return cacheCatalog(cacheKey, result, opts.CacheTTL)
}

// FetchAIGatewayPricing reads Vercel AI Gateway pricing translating input and output to prompt and completion
func FetchAIGatewayPricing(timeout time.Duration, forceRefresh bool) PricingTable {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cacheKey := strings.TrimRight(AIGatewayBaseURL, "/")
	if !forceRefresh {
		if cached := cachedCatalog(cacheKey); cached != nil {
			return cached
		}
	}
	payload := pricingGetJSON(cacheKey+"/models", map[string]string{"Accept": "application/json"}, timeout, false)
	if payload == nil {
		return cacheCatalog(cacheKey, PricingTable{}, 0)
	}
	result := PricingTable{}
	for _, item := range pricingCatalogItems(payload) {
		mid := stringOf(item["id"])
		pricing, ok := asMap(item["pricing"])
		if mid == "" || !ok {
			continue
		}
		result[mid] = pricingRow(pricing, "input", "output")
	}
	return cacheCatalog(cacheKey, result, 0)
}

func resolveOpenRouterPricingAPIKey() string {
	return strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
}

func pricingProfileKey(ctx context.Context) string { return home.Key(home.Dir(ctx)) }

func rememberProviderCacheKey(ctx context.Context, provider, cacheKey string) {
	pricingCacheMu.Lock()
	defer pricingCacheMu.Unlock()
	pricingProviderCacheKey[[2]string{pricingProfileKey(ctx), provider}] = cacheKey
}

func providerCacheKeyFor(ctx context.Context, provider string) (string, bool) {
	pricingCacheMu.Lock()
	defer pricingCacheMu.Unlock()
	key, ok := pricingProviderCacheKey[[2]string{pricingProfileKey(ctx), provider}]
	return key, ok
}

func fetchOpenRouterPricing(ctx context.Context, forceRefresh bool) PricingTable {
	rememberProviderCacheKey(ctx, "openrouter", openRouterPricingBase)
	return FetchModelsWithPricing(ctx, PricingFetchOptions{APIKey: resolveOpenRouterPricingAPIKey(), BaseURL: openRouterPricingBase, ForceRefresh: forceRefresh})
}

func fetchAIGatewayPricingForProvider(ctx context.Context, forceRefresh bool) PricingTable {
	rememberProviderCacheKey(ctx, "ai-gateway", aiGatewayPricingScope())
	return FetchAIGatewayPricing(0, forceRefresh)
}

func fetchNovitaPricingForProvider(ctx context.Context, forceRefresh bool) PricingTable {
	rememberProviderCacheKey(ctx, "novita", novitaPricingScope())
	return fetchNovitaPricing(0, forceRefresh)
}

// Kilo gateway base with KILOCODE_BASE_URL overriding the profile
func kiloPricingScope() string {
	base := strings.TrimSpace(os.Getenv("KILOCODE_BASE_URL"))
	if base == "" {
		if p, ok := ProfileFor("kilocode"); ok {
			base = p.BaseURL
		}
	}
	return strings.TrimSuffix(strings.TrimRight(base, "/"), "/v1")
}

// Kilo gateway pricing with the isFree flag authoritative and the varies sentinel rows skipped
func fetchKilocodePricing(timeout time.Duration, forceRefresh bool) PricingTable {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cacheKey := kiloPricingScope()
	if cacheKey == "" {
		return PricingTable{}
	}
	if !forceRefresh {
		if cached := cachedCatalog(cacheKey); cached != nil {
			return cached
		}
	}
	lines := []headerField{{Name: "Accept", Value: "application/json"}, {Name: "User-Agent", Value: userAgent("icebath-cli")}}
	if apiKey := strings.TrimSpace(os.Getenv("KILOCODE_API_KEY")); apiKey != "" {
		lines = append(lines, headerField{Name: "Authorization", Value: "Bearer " + apiKey})
	}
	payload := pricingGetJSON(cacheKey+"/models", fieldMap(lines...), timeout, true)
	if payload == nil {
		return cacheCatalog(cacheKey, PricingTable{}, 0)
	}
	result := PricingTable{}
	for _, item := range pricingCatalogItems(payload) {
		mid := stringOf(item["id"])
		pricing, ok := asMap(item["pricing"])
		if mid == "" || !ok {
			continue
		}
		entry := pricingRow(pricing, "prompt", "completion")
		if truthy(item["isFree"]) {
			entry["prompt"] = "0"
			entry["completion"] = "0"
			delete(entry, "input_cache_read")
		}
		if _, ok := priceFloat(entry["prompt"]); !ok {
			continue
		}
		if _, ok := priceFloat(entry["completion"]); !ok {
			continue
		}
		result[mid] = entry
	}
	return cacheCatalog(cacheKey, result, 0)
}

func fetchFireworksPricingForProvider(ctx context.Context, forceRefresh bool) PricingTable {
	rememberProviderCacheKey(ctx, "fireworks", fireworksPricingKey)
	return fireworksPricingFromModelsDev(ctx, forceRefresh)
}

func fetchKilocodePricingForProvider(ctx context.Context, forceRefresh bool) PricingTable {
	rememberProviderCacheKey(ctx, "kilocode", kiloPricingScope())
	return fetchKilocodePricing(0, forceRefresh)
}

func aiGatewayPricingScope() string { return strings.TrimRight(AIGatewayBaseURL, "/") }

func novitaPricingScope() string {
	base := strings.TrimSpace(os.Getenv("NOVITA_BASE_URL"))
	if base == "" {
		base = "https://api.novita.ai/openai/v1"
	}
	return strings.TrimRight(base, "/")
}

// Static endpoint identity per provider, dynamic ones resolved in PricingCacheScope
var staticPricingScopes = map[string]func() string{
	"openrouter": func() string { return openRouterPricingBase },
	"ai-gateway": aiGatewayPricingScope,
	"novita":     novitaPricingScope,
	"kilocode":   kiloPricingScope,
	"fireworks":  func() string { return fireworksPricingKey },
}

// ResolvePricingProvider returns the canonical pricing source for a provider, empty when a custom entry is unproven
func ResolvePricingProvider(provider, baseURL string) string {
	normalized := catalogNormalizeProvider(provider)
	if !strings.HasPrefix(normalized, "custom:") {
		return normalized
	}
	if baseURL == "" {
		return ""
	}
	inferred := inferProviderFromBaseURL(baseURL)
	if inferred == "" {
		return ""
	}
	host := Hostname(baseURL)
	claimed := false
	if profile, ok := ProfileFor(inferred); ok {
		for _, h := range profile.Hostnames {
			if h == host {
				claimed = true
				break
			}
		}
	}
	if !claimed {
		for _, e := range metadataURLToProvider {
			if e.provider == inferred && !strings.HasPrefix(e.part, ".") && HostMatches(baseURL, e.part) {
				claimed = true
				break
			}
		}
	}
	if !claimed {
		return ""
	}
	return catalogNormalizeProvider(inferred)
}

// PricingCacheScope is the current endpoint identity a provider's pricing cache is keyed on, resolved locally
func PricingCacheScope(ctx context.Context, provider, baseURL string) string {
	normalized := ResolvePricingProvider(provider, baseURL)
	if static := staticPricingScopes[normalized]; static != nil {
		return static()
	}
	if normalized == "deepinfra" {
		key, _ := deepinfraCatalogURLFn()
		return key
	}
	return ""
}

// Process resident pricing for a normalized provider without any provider I/O
func cachedOnlyPricing(ctx context.Context, normalized string) PricingTable {
	if normalized == "deepinfra" {
		key, _ := deepinfraCatalogURLFn()
		if deepinfraCatalogCachedFn(key) {
			return fetchDeepinfraPricing(0, false)
		}
		return PricingTable{}
	}
	cacheKey, ok := providerCacheKeyFor(ctx, normalized)
	if !ok && (normalized == "openrouter" || normalized == "ai-gateway" || normalized == "fireworks") {
		cacheKey = staticPricingScopes[normalized]()
	}
	if cacheKey == "" {
		return PricingTable{}
	}
	if cached := cachedCatalog(cacheKey); cached != nil {
		return cached
	}
	return PricingTable{}
}

var pricingFetchers = map[string]func(context.Context, bool) PricingTable{
	"openrouter": fetchOpenRouterPricing,
	"ai-gateway": fetchAIGatewayPricingForProvider,
	"novita":     fetchNovitaPricingForProvider,
	"deepinfra":  func(ctx context.Context, force bool) PricingTable { return fetchDeepinfraPricing(0, force) },
	"fireworks":  fetchFireworksPricingForProvider,
	"kilocode":   fetchKilocodePricingForProvider,
}

// GetPricingForProvider returns live pricing for the providers that publish it, empty for everything else
func GetPricingForProvider(ctx context.Context, provider, baseURL string, forceRefresh, cachedOnly bool) PricingTable {
	normalized := ResolvePricingProvider(provider, baseURL)
	if cachedOnly {
		return cachedOnlyPricing(ctx, normalized)
	}
	fetcher := pricingFetchers[normalized]
	if fetcher == nil {
		return PricingTable{}
	}
	return fetcher(ctx, forceRefresh)
}

// Fireworks picker pricing as a pure transform of the models.dev registry
func fireworksPricingFromModelsDev(ctx context.Context, forceRefresh bool) PricingTable {
	if !forceRefresh {
		if cached := cachedCatalog(fireworksPricingKey); cached != nil {
			return cached
		}
	}
	result := PricingTable{}
	h := home.Dir(ctx)
	for mid, entry := range modelsDevProviderModels(h, "fireworks", false, modelCatalogConfig(h)) {
		em, ok := asMap(entry)
		if !ok {
			continue
		}
		cost, ok := asMap(em["cost"])
		if !ok {
			continue
		}
		inp, out := cost["input"], cost["output"]
		if inp == nil && out == nil {
			continue
		}
		row := map[string]any{"prompt": perToken(inp), "completion": perToken(out)}
		if truthy(cost["cache_read"]) {
			row["input_cache_read"] = perToken(cost["cache_read"])
		}
		result[mid] = row
	}
	return cacheCatalog(fireworksPricingKey, result, 0)
}

// NovitaAI pricing converting per million prices in units of 0.0001 USD into per token strings
func fetchNovitaPricing(timeout time.Duration, forceRefresh bool) PricingTable {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	apiKey := strings.TrimSpace(os.Getenv("NOVITA_API_KEY"))
	if apiKey == "" {
		return PricingTable{}
	}
	cacheKey := novitaPricingScope()
	if !forceRefresh {
		if cached := cachedCatalog(cacheKey); cached != nil {
			return cached
		}
	}
	headers := fieldMap(headerField{Name: "Authorization", Value: "Bearer " + apiKey}, headerField{Name: "Accept", Value: "application/json"},
		headerField{Name: "User-Agent", Value: userAgent("icebath-cli")})
	payload := pricingGetJSON(cacheKey+"/models", headers, timeout, true)
	if payload == nil {
		return cacheCatalog(cacheKey, PricingTable{}, 0)
	}
	result := PricingTable{}
	for _, item := range pricingCatalogItems(payload) {
		mid := stringOf(item["id"])
		inp, out := item["input_token_price_per_m"], item["output_token_price_per_m"]
		if mid == "" || (inp == nil && out == nil) {
			continue
		}
		inF, _ := floatOf(inp)
		outF, _ := floatOf(out)
		result[mid] = map[string]any{
			"prompt":     strconv.FormatFloat(inF/10_000/1_000_000, 'g', -1, 64),
			"completion": strconv.FormatFloat(outF/10_000/1_000_000, 'g', -1, 64),
		}
	}
	return cacheCatalog(cacheKey, result, 0)
}

// DeepInfra chat model pricing from dollars per million into per token picker rows
func fetchDeepinfraPricing(timeout time.Duration, forceRefresh bool) PricingTable {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	result := PricingTable{}
	for _, item := range fetchDeepinfraModelsByTagFn("chat", timeout, forceRefresh) {
		metadata := dictOrEmpty(item["metadata"])
		pricing, ok := asMap(metadata["pricing"])
		if !ok {
			continue
		}
		entry := map[string]any{}
		for _, pair := range [][2]string{{"input_tokens", "prompt"}, {"output_tokens", "completion"}, {"cache_read_tokens", "input_cache_read"}} {
			if v, present := pricing[pair[0]]; present && v != nil {
				entry[pair[1]] = perToken(v)
			}
		}
		if len(entry) > 0 {
			result[stringOf(item["id"])] = entry
		}
	}
	return result
}

// ---- plan limits and credits tracker reads ----

// Both pricing fields parse to zero with missing fields read as the default
func zeroPriced(pricing any, keys [2]string, def string) bool {
	m, ok := asMap(pricing)
	if !ok {
		return false
	}
	for _, k := range keys {
		raw := def
		if v, present := m[k]; present {
			raw = rawPriceString(v)
			if v == nil {
				return false
			}
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil || f != 0 {
			return false
		}
	}
	return true
}
