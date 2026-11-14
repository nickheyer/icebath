package catalog

import (
	"context"
	"errors"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/providers"
	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	CostEstimated = "estimated"
	CostIncluded  = "included"
	CostUnknown   = "unknown"

	CostSourceModelsAPI     = "provider_models_api"
	CostSourceDocsSnapshot  = "official_docs_snapshot"
	CostSourceNone          = "none"
	BillingSubscription     = "subscription_included"
	BillingModelsAPI        = "official_models_api"
	BillingDocsSnapshot     = "official_docs_snapshot"
	BillingUnknown          = "unknown"
	portalDefaultBaseURL    = "https://inference-api.icebath.app/v1"
	includedNote            = "subscription-included; no provider invoice for usage"
	includedPricingVersion  = "included-route"
	openRouterModelsDocsURL = "https://openrouter.ai/docs/api/api-reference/models/get-models"
)

type BillingRoute struct {
	Provider    string
	Model       string
	BaseURL     string
	BillingMode string
}

type PricingEntry struct {
	Input          *big.Rat
	Output         *big.Rat
	CacheRead      *big.Rat
	CacheWrite     *big.Rat
	Request        *big.Rat
	Source         string
	SourceURL      string
	PricingVersion string
	FetchedAt      time.Time
	TierThreshold  int

	InputAbove      *big.Rat
	OutputAbove     *big.Rat
	CacheReadAbove  *big.Rat
	CacheWriteAbove *big.Rat

	nonFinite bool
}

func (e *PricingEntry) NonFinite() bool { return e != nil && e.nonFinite }

type CostResult struct {
	Amount         *big.Rat
	Status         string
	Source         string
	Label          string
	FetchedAt      time.Time
	PricingVersion string
	Notes          []string
}

type snapshotRow struct {
	models []string
	rates  []string
}

type snapshotSet struct {
	provider string
	url      string
	version  string
	rows     []snapshotRow
}

type snapshotKey struct {
	provider string
	model    string
}

type tieredRow struct {
	slug      string
	base      [4]string
	above     [4]string
	url       string
	version   string
	threshold int
}

const (
	bedrockPricingURL   = "https://aws.amazon.com/bedrock/pricing/"
	anthropicPricingURL = "https://platform.claude.com/docs/en/about-claude/pricing"
	googlePricingURL    = "https://ai.google.dev/pricing"
	geminiPricingURL    = "https://ai.google.dev/gemini-api/docs/pricing"
	ultrafastPricingURL = "https://developers.openai.com/api/docs/pricing?latest-pricing=ultrafast"
)

var (
	opusRates    = []string{"5.00", "25.00", "0.50", "6.25"}
	sonnetRates  = []string{"3.00", "15.00", "0.30", "3.75"}
	opus55Rates  = []string{"4.00", "20.00", "0.20", "5.00"}
	oneMillion   = big.NewRat(1000000, 1)
	subCentLimit = big.NewRat(1, 100)
)

var snapshotSets = []snapshotSet{
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
		{[]string{"gpt-4o"}, []string{"2.50", "10.00", "1.25"}},
		{[]string{"gpt-4o-mini"}, []string{"0.15", "0.60", "0.075"}},
		{[]string{"gpt-4.1"}, []string{"2.00", "8.00", "0.50"}},
		{[]string{"gpt-4.1-mini"}, []string{"0.40", "1.60", "0.10"}},
		{[]string{"gpt-4.1-nano"}, []string{"0.10", "0.40", "0.025"}},
		{[]string{"o3"}, []string{"10.00", "40.00", "2.50"}},
		{[]string{"o3-mini"}, []string{"1.10", "4.40", "0.55"}},
	}},
	{"deepseek", "https://api-docs.deepseek.com/quick_start/pricing", "deepseek-pricing-2026-09-10", []snapshotRow{
		{[]string{"deepseek-flash", "deepseek-v4-flash", "deepseek-chat", "deepseek-reasoner"}, []string{"0.15", "0.60", "0.003"}},
		{[]string{"deepseek-v4-pro"}, []string{"0.66", "1.98", "0.022"}},
	}},
	{"google", geminiPricingURL, "google-pricing-2026-10-13", []snapshotRow{
		{[]string{"gemini-4-argon"}, []string{"2.00", "10.00", "0.10"}},
	}},
	{"google", geminiPricingURL, "google-pricing-2026-09-02", []snapshotRow{
		{[]string{"gemini-3.8-flash", "gemini-3.7-flash"}, []string{"0.75", "3.75", "0.075"}},
	}},
	{"google", geminiPricingURL, "google-pricing-2026-07-28", []snapshotRow{
		{[]string{"gemini-3.6-flash"}, []string{"1.50", "7.50", "0.15"}},
		{[]string{"gemini-3.5-flash-lite"}, []string{"0.30", "2.50", "0.03"}},
	}},
	{"google", googlePricingURL, "google-pricing-2026-07-07", []snapshotRow{
		{[]string{"gemini-3.5-flash"}, []string{"1.50", "9.00", "0.15"}},
		{[]string{"gemini-3.1-flash-lite"}, []string{"0.25", "1.50", "0.025"}},
		{[]string{"gemini-3-pro-preview"}, []string{"2.00", "12.00", "0.20"}},
		{[]string{"gemini-3-flash-preview"}, []string{"0.50", "3.00", "0.05"}},
		{[]string{"gemini-2.5-flash"}, []string{"0.15", "0.60", "0.015"}},
		{[]string{"gemini-2.0-flash"}, []string{"0.10", "0.40", "0.01"}},
	}},
	{"bedrock", bedrockPricingURL, "anthropic-list-2026-07", []snapshotRow{
		{[]string{"anthropic.claude-opus-4-8", "anthropic.claude-opus-4-7", "anthropic.claude-opus-4-6"}, opusRates},
	}},
	{"bedrock", bedrockPricingURL, "anthropic-list-2026-09", []snapshotRow{
		{[]string{"anthropic.claude-opus-5"}, opusRates},
		{[]string{"anthropic.claude-opus-5-5"}, opus55Rates},
	}},
	{"bedrock", bedrockPricingURL, "bedrock-pricing-2026-06", []snapshotRow{
		{[]string{"anthropic.claude-sonnet-5"}, sonnetRates},
	}},
	{"bedrock", bedrockPricingURL, "bedrock-pricing-2026-04", []snapshotRow{
		{[]string{"anthropic.claude-sonnet-4-6", "anthropic.claude-sonnet-4-5"}, sonnetRates},
		{[]string{"anthropic.claude-haiku-4-5"}, []string{"0.80", "4.00", "0.08", "1.00"}},
		{[]string{"amazon.nova-pro"}, []string{"0.80", "3.20"}},
		{[]string{"amazon.nova-lite"}, []string{"0.06", "0.24"}},
		{[]string{"amazon.nova-micro"}, []string{"0.035", "0.14"}},
	}},
	{"minimax", "", "minimax-pricing-2026-04", []snapshotRow{{[]string{"minimax-m2.7"}, []string{"0.30", "1.20"}}}},
	{"minimax-cn", "", "minimax-pricing-2026-04", []snapshotRow{{[]string{"minimax-m2.7"}, []string{"0.30", "1.20"}}}},
	{"fireworks", "https://docs.fireworks.ai/serverless/pricing", "fireworks-pricing-2026-07", []snapshotRow{
		{[]string{"kimi-k2p6"}, []string{"0.95", "4.00", "0.16"}},
		{[]string{"kimi-k2p7-code"}, []string{"0.95", "4.00", "0.19"}},
		{[]string{"glm-5p2"}, []string{"1.40", "4.40", "0.14"}},
		{[]string{"deepseek-v4-pro"}, []string{"1.74", "3.48", "0.145"}},
		{[]string{"deepseek-v4-flash"}, []string{"0.14", "0.28", "0.028"}},
		{[]string{"qwen3p7-plus"}, []string{"0.40", "1.60", "0.08"}},
		{[]string{"minimax-m3"}, []string{"0.30", "1.20", "0.06"}},
		{[]string{"gpt-oss-120b"}, []string{"0.15", "0.60", "0.015"}},
		{[]string{"gpt-oss-20b"}, []string{"0.07", "0.30", "0.035"}},
		{[]string{"glm-5p1"}, []string{"1.40", "4.40", "0.26"}},
		{[]string{"minimax-m2p7"}, []string{"0.30", "1.20", "0.06"}},
		{[]string{"kimi-k2p6-fast", "kimi-k2p6-turbo"}, []string{"2.00", "8.00", "0.30"}},
		{[]string{"kimi-k2p7-code-fast"}, []string{"1.90", "8.00", "0.38"}},
		{[]string{"glm-5p2-fast"}, []string{"2.10", "6.60", "0.21"}},
		{[]string{"glm-5p1-fast"}, []string{"2.80", "8.80", "0.52"}},
	}},
}

var openAITieredRows = []tieredRow{
	{"gpt-6-astra", [4]string{"10.00", "50.00", "1.00", "12.50"}, [4]string{"20.00", "75.00", "2.00", "25.00"},
		"https://developers.openai.com/api/docs/models/gpt-6-astra", "openai-gpt-6-astra-2026-09", 272_000},
	{"gpt-6-sol", [4]string{"2.00", "10.00", "0.20", "2.50"}, [4]string{"4.00", "15.00", "0.40", "5.00"},
		"https://developers.openai.com/api/docs/models/gpt-6-sol", "openai-gpt-6-tiers-2026-09", 272_000},
	{"gpt-6.1-sol", [4]string{"2.00", "10.00", "0.10", "2.50"}, [4]string{"4.00", "15.00", "0.20", "5.00"},
		"https://developers.openai.com/api/docs/models/gpt-6.1-sol", "openai-gpt-6-tiers-2026-09", 272_000},
	{"gpt-6-luna", [4]string{"0.10", "0.50", "0.01", "0.125"}, [4]string{"0.20", "0.75", "0.02", "0.25"},
		"https://developers.openai.com/api/docs/models/gpt-6-luna", "openai-gpt-6-tiers-2026-09", 272_000},
}

var openAIUltrafastRows = []tieredRow{
	{"gpt-6-astra", [4]string{"60.00", "300.00", "6.00", "75.00"}, [4]string{"120.00", "450.00", "12.00", "150.00"},
		ultrafastPricingURL, "openai-ultrafast-2026-09", 272_000},
	{"gpt-6.1-sol", [4]string{"12.00", "60.00", "0.60", "15.00"}, [4]string{"24.00", "90.00", "1.20", "30.00"},
		ultrafastPricingURL, "openai-ultrafast-2026-10-08", 272_000},
}

func mustRat(text string) *big.Rat {
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		panic("catalog: pricing table carries a rate that is not a decimal: " + text)
	}
	return value
}

func snapshotEntry(rates []string, version, url string) *PricingEntry {
	entry := &PricingEntry{Input: mustRat(rates[0]), Output: mustRat(rates[1]), Source: CostSourceDocsSnapshot, SourceURL: url, PricingVersion: version}
	if len(rates) > 2 {
		entry.CacheRead = mustRat(rates[2])
	}
	if len(rates) > 3 {
		entry.CacheWrite = mustRat(rates[3])
	}
	return entry
}

func tieredEntry(row tieredRow) *PricingEntry {
	entry := snapshotEntry(row.base[:], row.version, row.url)
	entry.TierThreshold = row.threshold
	entry.InputAbove, entry.OutputAbove = mustRat(row.above[0]), mustRat(row.above[1])
	entry.CacheReadAbove, entry.CacheWriteAbove = mustRat(row.above[2]), mustRat(row.above[3])
	return entry
}

var officialDocsPricing = func() map[snapshotKey]*PricingEntry {
	out := map[snapshotKey]*PricingEntry{}
	for _, set := range snapshotSets {
		for _, row := range set.rows {
			entry := snapshotEntry(row.rates, set.version, set.url)
			for _, model := range row.models {
				out[snapshotKey{set.provider, model}] = entry
			}
		}
	}
	for _, row := range openAITieredRows {
		out[snapshotKey{"openai", row.slug}] = tieredEntry(row)
	}
	gemini31 := snapshotEntry([]string{"2.00", "12.00", "0.20"}, "google-pricing-2026-07-07", googlePricingURL)
	gemini31.TierThreshold = 200_000
	gemini31.InputAbove, gemini31.OutputAbove, gemini31.CacheReadAbove = mustRat("4.00"), mustRat("18.00"), mustRat("0.40")
	out[snapshotKey{"google", "gemini-3.1-pro"}] = gemini31
	gemini25 := snapshotEntry([]string{"1.25", "10.00", "0.125"}, "google-pricing-2026-07-07", googlePricingURL)
	gemini25.TierThreshold = 200_000
	gemini25.InputAbove, gemini25.OutputAbove = mustRat("2.50"), mustRat("15.00")
	out[snapshotKey{"google", "gemini-2.5-pro"}] = gemini25
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-sol", "gpt-6-luna", "gpt-6.1-sol"} {
		for _, suffix := range []string{"pro", "900k"} {
			out[snapshotKey{"openai", model + "-" + suffix}] = out[snapshotKey{"openai", model}]
		}
	}
	out[snapshotKey{"google", "gemini-3.1-pro-preview"}] = out[snapshotKey{"google", "gemini-3.1-pro"}]
	out[snapshotKey{"google", "gemini-3.1-flash-lite-preview"}] = out[snapshotKey{"google", "gemini-3.1-flash-lite"}]
	return out
}()

var openAIUltrafastPricing = func() map[string]*PricingEntry {
	out := map[string]*PricingEntry{}
	for _, row := range openAIUltrafastRows {
		out[row.slug] = tieredEntry(row)
	}
	return out
}()

var anthropicFastModePricing = func() map[string]*PricingEntry {
	url := anthropicPricingURL + "#fast-mode-pricing"
	premium := snapshotEntry([]string{"10.00", "50.00", "1.00", "12.50"}, "anthropic-fast-mode-2026-09", url)
	return map[string]*PricingEntry{
		"claude-opus-4-8": premium,
		"claude-opus-5":   premium,
		"claude-opus-5-5": snapshotEntry([]string{"8.00", "40.00", "0.40", "10.00"}, "anthropic-fast-mode-2026-09", url),
	}
}()

var includedEntry = &PricingEntry{
	Input: new(big.Rat), Output: new(big.Rat), CacheRead: new(big.Rat), CacheWrite: new(big.Rat),
	Source: CostSourceNone, PricingVersion: includedPricingVersion,
}

var modelsDevDirectHosts = map[string]string{
	"openai": "openai.com", "xai": "x.ai", "anthropic": "anthropic.com", "google": "googleapis.com",
	"deepseek": "deepseek.com", "xiaomi": "xiaomimimo.com",
}

var snapshotProviderAliases = map[string]string{
	"anthropic": "anthropic", "openai": "openai", "openai-api": "openai", "minimax": "minimax", "minimax-cn": "minimax-cn",
}

var googleProviderNames = []string{"google", "gemini", "vertex", "google-gemini", "google-ai-studio", "google-vertex", "vertex-ai"}

var bedrockRegionPrefixes = []string{"global.", "us.", "eu.", "apac.", "ap.", "au.", "jp.", "ca.", "sa.", "me.", "af."}

var (
	dottedVersion   = regexp.MustCompile(`(\d+)\.(\d+)`)
	bedrockTrailers = []*regexp.Regexp{regexp.MustCompile(`:\d+$`), regexp.MustCompile(`-v\d+$`), regexp.MustCompile(`-\d{8}$`)}
)

func formatFixed(amount *big.Rat, places int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	scaled := new(big.Rat).Mul(new(big.Rat).Abs(amount), new(big.Rat).SetInt(scale))
	quotient, remainder := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	twice := new(big.Int).Lsh(remainder, 1)
	switch twice.Cmp(scaled.Denom()) {
	case 1:
		quotient.Add(quotient, big.NewInt(1))
	case 0:
		if quotient.Bit(0) == 1 {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	digits := quotient.String()
	if places > 0 {
		for len(digits) <= places {
			digits = "0" + digits
		}
		digits = digits[:len(digits)-places] + "." + digits[len(digits)-places:]
	}
	if amount.Sign() < 0 {
		return "-" + digits
	}
	return digits
}

func FormatCostLabel(amount *big.Rat) string {
	if amount.Sign() == 0 {
		return "$0.00"
	}
	if amount.Cmp(subCentLimit) < 0 {
		label := "~$" + formatFixed(amount, 4)
		if label == "~$0.0000" {
			return "~$<0.0001"
		}
		return label
	}
	return "~$" + formatFixed(amount, 2)
}

func toRat(text string) *big.Rat {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	value, ok := new(big.Rat).SetString(strings.ReplaceAll(trimmed, "_", ""))
	if !ok {
		return nil
	}
	return value
}

func nonFiniteDecimal(text string) bool {
	word := strings.ToLower(strings.TrimSpace(text))
	word = strings.TrimLeft(word, "+-")
	if word == "inf" || word == "infinity" {
		return true
	}
	word = strings.TrimPrefix(word, "s")
	return strings.HasPrefix(word, "nan") && strings.Trim(word[3:], "0123456789") == ""
}

var ErrNonFinitePrice = errors.New("catalog: a price of this route is not a finite decimal")

func floatRat(value float64) *big.Rat {
	return toRat(strconv.FormatFloat(value, 'g', -1, 64))
}

func urlOrigin(baseURL string) (scheme, host string, port int) {
	host = providers.URLHostname(baseURL)
	if host == "" {
		return "", "", 0
	}
	u := parseBaseURL(baseURL, "none")
	if u == nil {
		return "", "", 0
	}
	if strings.Contains(normalizeBaseURL(baseURL), "://") {
		scheme = strings.ToLower(u.Scheme)
	}
	if explicit := u.Port(); explicit != "" {
		port, _ = strconv.Atoi(explicit)
		return scheme, host, port
	}
	switch scheme {
	case "https":
		port = 443
	case "http":
		port = 80
	}
	return scheme, host, port
}

func ResolveBillingRoute(model, provider, baseURL string) BillingRoute {
	providerName := strings.ToLower(strings.TrimSpace(provider))
	base := strings.ToLower(strings.TrimSpace(baseURL))
	model = strings.TrimSpace(model)
	if providerName == "" {
		if inferred, bare, ok := strings.Cut(model, "/"); ok && (inferred == "anthropic" || inferred == "openai" || inferred == "google") {
			providerName, model = inferred, bare
		}
	}
	bare := model[strings.LastIndex(model, "/")+1:]
	host := func(name string) bool { return providers.HostMatches(baseURL, name) }
	if providerName == "openai-codex" {
		return BillingRoute{Provider: "openai-codex", Model: model, BaseURL: baseURL, BillingMode: BillingSubscription}
	}
	if providerName == "openrouter" || host("openrouter.ai") {
		return BillingRoute{Provider: "openrouter", Model: model, BaseURL: baseURL, BillingMode: BillingModelsAPI}
	}
	if providerName == providers.ProviderPortal || host("inference-api.icebath.app") {
		routeBase := baseURL
		if routeBase == "" {
			routeBase = portalDefaultBaseURL
		}
		return BillingRoute{Provider: providers.ProviderPortal, Model: model, BaseURL: routeBase, BillingMode: BillingModelsAPI}
	}
	snapshotProvider, ok := snapshotProviderAliases[providerName]
	if !ok {
		switch {
		case slices.Contains(googleProviderNames, providerName) || host("aiplatform.googleapis.com") || host("generativelanguage.googleapis.com"):
			snapshotProvider = "google"
		case providerName == "fireworks" || host("api.fireworks.ai"):
			snapshotProvider = "fireworks"
		}
	}
	if snapshotProvider != "" {
		if snapshotProvider == "openai" && (baseURL == "" || host("api.openai.com")) {
			bare = StripCodexContextVariantSuffix(bare)
		}
		return BillingRoute{Provider: snapshotProvider, Model: bare, BaseURL: baseURL, BillingMode: BillingDocsSnapshot}
	}
	localHost := false
	if base != "" {
		name := providers.URLHostname(base)
		localHost = name == "localhost" || name == "127.0.0.1"
	}
	if providerName == "custom" || providerName == "local" || localHost {
		if providerName == "" {
			providerName = "custom"
		}
		return BillingRoute{Provider: providerName, Model: model, BaseURL: baseURL, BillingMode: BillingUnknown}
	}
	if providerName == "" {
		providerName = "unknown"
	}
	if model == "" {
		bare = ""
	}
	return BillingRoute{Provider: providerName, Model: bare, BaseURL: baseURL, BillingMode: BillingUnknown}
}

func stripFirstPrefix(name string, prefixes []string) string {
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return name[len(prefix):]
		}
	}
	return name
}

func normalizeBedrockModelName(model string) string {
	name := dottedVersion.ReplaceAllString(stripFirstPrefix(strings.TrimSpace(strings.ToLower(model)), bedrockRegionPrefixes), "${1}-${2}")
	for _, trailer := range bedrockTrailers {
		name = trailer.ReplaceAllString(name, "")
	}
	return name
}

func normalizeAnthropicModelName(model string) string {
	return dottedVersion.ReplaceAllString(stripFirstPrefix(strings.TrimSpace(strings.ToLower(model)), []string{"anthropic/"}), "${1}-${2}")
}

func lookupOfficialDocsPricing(route BillingRoute) *PricingEntry {
	model := strings.ToLower(route.Model)
	if entry := officialDocsPricing[snapshotKey{route.Provider, model}]; entry != nil {
		return entry
	}
	normalized := model
	switch route.Provider {
	case "anthropic":
		normalized = normalizeAnthropicModelName(model)
	case "bedrock":
		normalized = normalizeBedrockModelName(model)
	}
	if normalized == model {
		return nil
	}
	return officialDocsPricing[snapshotKey{route.Provider, normalized}]
}

func anthropicFastModeEntry(model string) *PricingEntry {
	name := strings.ToLower(model)
	if entry := anthropicFastModePricing[name]; entry != nil {
		return entry
	}
	return anthropicFastModePricing[normalizeAnthropicModelName(name)]
}

func pricingEntryFromMetadata(set *ModelSet, modelID, sourceURL, pricingVersion string) *PricingEntry {
	if set == nil {
		return nil
	}
	meta := set.items[modelID]
	if meta == nil {
		return nil
	}
	nonFinite := false
	perMillion := func(keys ...string) *big.Rat {
		raw := ""
		for _, key := range keys {
			if raw = meta.Pricing[key]; raw != "" {
				break
			}
		}
		value := toRat(raw)
		if value == nil {
			nonFinite = nonFinite || nonFiniteDecimal(raw)
			return nil
		}
		return value.Mul(value, oneMillion)
	}
	prompt, completion, request := perMillion("prompt"), perMillion("completion"), toRat(meta.Pricing["request"])
	nonFinite = nonFinite || (request == nil && nonFiniteDecimal(meta.Pricing["request"]))
	if prompt == nil && completion == nil && request == nil && !nonFinite {
		return nil
	}
	entry := &PricingEntry{
		Input: prompt, Output: completion,
		CacheRead:  perMillion("cache_read", "cached_prompt", "input_cache_read"),
		CacheWrite: perMillion("cache_write", "cache_creation", "input_cache_write"),
		Request:    request, Source: CostSourceModelsAPI, SourceURL: sourceURL, PricingVersion: pricingVersion,
		FetchedAt: time.Now().UTC(),
	}
	entry.nonFinite = nonFinite
	return entry
}

func (c *Catalog) modelsDevPricingEntry(ctx context.Context, route BillingRoute) *PricingEntry {
	domain := modelsDevDirectHosts[route.Provider]
	if domain == "" || route.Model == "" {
		return nil
	}
	if route.BaseURL != "" {
		scheme, host, port := urlOrigin(route.BaseURL)
		if scheme != "https" || port != 443 || !(host == domain || strings.HasSuffix(host, "."+domain)) {
			return nil
		}
	}
	info, ok := c.ModelInfo(ctx, route.Provider, route.Model, false, nil)
	if !ok || !info.HasCostData() {
		return nil
	}
	entry := &PricingEntry{
		Input: floatRat(info.CostInput), Output: floatRat(info.CostOutput),
		Source: CostSourceModelsAPI, SourceURL: "https://models.dev", PricingVersion: "models.dev", FetchedAt: time.Now().UTC(),
	}
	if info.CostCacheRead != nil {
		entry.CacheRead = floatRat(*info.CostCacheRead)
	}
	if info.CostCacheWrite != nil {
		entry.CacheWrite = floatRat(*info.CostCacheWrite)
	}
	return entry
}

func (c *Catalog) pricingEntry(ctx context.Context, route BillingRoute, key auth.Credential) *PricingEntry {
	if route.BillingMode == BillingSubscription {
		return includedEntry
	}
	if route.Provider == "openrouter" {
		return pricingEntryFromMetadata(c.openRouterModels(ctx), route.Model, openRouterModelsDocsURL, "openrouter-models-api")
	}
	if bundled := lookupOfficialDocsPricing(route); bundled != nil {
		return bundled
	}
	if route.BaseURL != "" {
		entry := pricingEntryFromMetadata(c.endpointModels(ctx, route.BaseURL, key), route.Model,
			strings.TrimRight(route.BaseURL, "/")+"/models", "openai-compatible-models-api")
		if entry != nil {
			return entry
		}
	}
	return c.modelsDevPricingEntry(ctx, route)
}

func copyRat(value *big.Rat) *big.Rat {
	if value == nil {
		return nil
	}
	return new(big.Rat).Set(value)
}

func (c *Catalog) PricingEntry(ctx context.Context, model, provider, baseURL string, key auth.Credential) (*PricingEntry, bool) {
	ctx = c.scoped(ctx)
	entry := c.pricingEntry(ctx, ResolveBillingRoute(model, provider, baseURL), key)
	if entry == nil {
		return nil, false
	}
	owned := *entry
	owned.Input, owned.Output = copyRat(entry.Input), copyRat(entry.Output)
	owned.CacheRead, owned.CacheWrite, owned.Request = copyRat(entry.CacheRead), copyRat(entry.CacheWrite), copyRat(entry.Request)
	owned.InputAbove, owned.OutputAbove = copyRat(entry.InputAbove), copyRat(entry.OutputAbove)
	owned.CacheReadAbove, owned.CacheWriteAbove = copyRat(entry.CacheReadAbove), copyRat(entry.CacheWriteAbove)
	return &owned, true
}

func (c *Catalog) HasKnownPricing(ctx context.Context, model, provider, baseURL string, key auth.Credential) bool {
	_, ok := c.PricingEntry(ctx, model, provider, baseURL, key)
	return ok
}

func unknownCost(source string, notes ...string) CostResult {
	return CostResult{Status: CostUnknown, Source: source, Label: "n/a", Notes: notes}
}

func reportedCost(cost *providers.Cost) CostResult {
	return CostResult{
		Amount: toRat(cost.AmountUSD), Status: cost.Status, Source: cost.Source, Label: cost.Label,
		FetchedAt: cost.FetchedAt, PricingVersion: cost.PricingVersion, Notes: slices.Clone(cost.Notes),
	}
}

func (c *Catalog) EstimateCost(ctx context.Context, model string, usage wire.Usage, provider, baseURL string, key auth.Credential) (CostResult, error) {
	result, entry := c.estimateCost(c.scoped(ctx), model, usage, provider, baseURL, key)
	if entry.NonFinite() {
		return CostResult{}, ErrNonFinitePrice
	}
	return result, nil
}

func (c *Catalog) estimateCost(ctx context.Context, model string, usage wire.Usage, provider, baseURL string, key auth.Credential) (CostResult, *PricingEntry) {
	if p, ok := c.reg.Get(ctx, provider); ok {
		if coster, ok := p.(providers.UsageCoster); ok {
			if reported, ok := coster.UsageCost(model, &usage); ok && reported != nil {
				return reportedCost(reported), nil
			}
		}
	}
	route := ResolveBillingRoute(model, provider, baseURL)
	if route.BillingMode == BillingSubscription {
		return CostResult{
			Amount: new(big.Rat), Status: CostIncluded, Source: CostSourceNone, Label: "included",
			PricingVersion: includedPricingVersion, Notes: []string{includedNote},
		}, nil
	}
	entry := c.pricingEntry(ctx, route, key)
	if route.Provider == "anthropic" && usage.Speed == "fast" {
		if entry = anthropicFastModeEntry(route.Model); entry == nil {
			return unknownCost(CostSourceDocsSnapshot, "fast-mode pricing unavailable for model"), nil
		}
	}
	if route.Provider == "openai" && usage.ServedTier == "ultrafast" {
		if entry = openAIUltrafastPricing[route.Model]; entry == nil {
			return unknownCost(CostSourceDocsSnapshot, "ultrafast pricing unavailable for model"), nil
		}
	}
	if entry == nil {
		return unknownCost(CostSourceNone), nil
	}
	if entry.nonFinite {
		return CostResult{}, entry
	}
	above := entry.TierThreshold > 0 && usage.PromptTokens() > entry.TierThreshold
	amount := new(big.Rat)
	buckets := []struct {
		tokens    int
		rate      *big.Rat
		rateAbove *big.Rat
		note      string
	}{
		{usage.InputTokens, entry.Input, entry.InputAbove, ""},
		{usage.OutputTokens, entry.Output, entry.OutputAbove, ""},
		{usage.CacheReadTokens, entry.CacheRead, entry.CacheReadAbove, "cache-read pricing unavailable for route"},
		{usage.CacheWriteTokens, entry.CacheWrite, entry.CacheWriteAbove, "cache-write pricing unavailable for route"},
	}
	for _, bucket := range buckets {
		rate := bucket.rate
		if above && bucket.rateAbove != nil {
			rate = bucket.rateAbove
		}
		if rate == nil {
			if bucket.tokens != 0 {
				if bucket.note == "" {
					return unknownCost(entry.Source), nil
				}
				return unknownCost(entry.Source, bucket.note), nil
			}
			continue
		}
		part := new(big.Rat).Mul(big.NewRat(int64(bucket.tokens), 1), rate)
		amount.Add(amount, part.Quo(part, oneMillion))
	}
	if entry.Request != nil && usage.Requests != 0 {
		amount.Add(amount, new(big.Rat).Mul(big.NewRat(int64(usage.Requests), 1), entry.Request))
	}
	result := CostResult{
		Amount: amount, Status: CostEstimated, Source: entry.Source, Label: FormatCostLabel(amount),
		FetchedAt: entry.FetchedAt, PricingVersion: entry.PricingVersion,
	}
	if entry.Source == CostSourceNone && amount.Sign() == 0 {
		result.Status, result.Label = CostIncluded, "included"
		result.Notes = append(result.Notes, includedNote)
	}
	if route.Provider == "openrouter" {
		result.Notes = append(result.Notes, "OpenRouter cost is estimated from the models API until reconciled.")
	}
	return result, nil
}
