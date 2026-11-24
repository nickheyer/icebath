package provider

import (
	"context"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

const (
	copilotBaseURL         = "https://api.githubcopilot.com"
	copilotModelsURL       = copilotBaseURL + "/models"
	copilotModelsDevID     = "github-copilot"
	copilotCatalogCacheTTL = 300 * time.Second
	copilotContextCacheTTL = 3600.0
	copilotCatalogTimeout  = 5 * time.Second
)

var (
	copilotHostnames         = []string{"githubcopilot.com", "models.github.ai"}
	copilotMetadataHostnames = []string{"models.inference.ai.azure.com"}
	copilotCatalogHosts      = []string{"githubcopilot.com", "models.github.ai", "models.inference.ai.azure.com"}
	copilotEffortsGPT5       = []string{"minimal", "low", "medium", "high"}
	copilotEffortsOSeries    = []string{"low", "medium", "high"}
	copilotAstraEfforts      = []string{"low", "medium", "high", "xhigh", "max"}
	copilotChatEndpoints     = map[string]bool{"/chat/completions": true, "/responses": true, "/v1/messages": true}
	copilotResponsesRe       = regexp.MustCompile(`^gpt-(\d+)`)
)

// Curated list served when the live catalog is unreachable
var copilotStaticModels = []string{
	"gpt-5.4", "gpt-5.4-mini", "gpt-5-mini", "gpt-5.3-codex", "gpt-5.2-codex", "gpt-4.1", "gpt-4o", "gpt-4o-mini",
	"claude-sonnet-4.6", "claude-sonnet-5", "claude-sonnet-4", "claude-sonnet-4.5", "claude-haiku-4.5",
	"gemini-3.1-pro-preview", "gemini-3-pro-preview", "gemini-3-flash-preview", "gemini-2.5-pro",
}

// OpenRouter style and dash notation ids mapped onto the Copilot ids
var copilotModelAliases = map[string]string{
	"openai/gpt-5": "gpt-5-mini", "openai/gpt-5-chat": "gpt-5-mini", "openai/gpt-5-mini": "gpt-5-mini",
	"openai/gpt-5-nano": "gpt-5-mini", "openai/gpt-4.1": "gpt-4.1", "openai/gpt-4.1-mini": "gpt-4.1",
	"openai/gpt-4.1-nano": "gpt-4.1", "openai/gpt-4o": "gpt-4o", "openai/gpt-4o-mini": "gpt-4o-mini",
	"openai/o1": "gpt-5.2", "openai/o1-mini": "gpt-5-mini", "openai/o1-preview": "gpt-5.2",
	"openai/o3": "gpt-5.3-codex", "openai/o3-mini": "gpt-5-mini", "openai/o4-mini": "gpt-5-mini",
	"anthropic/claude-opus-4.6": "claude-opus-4.6", "anthropic/claude-sonnet-5": "claude-sonnet-5",
	"anthropic/claude-sonnet-4.6": "claude-sonnet-4.6", "anthropic/claude-sonnet-4": "claude-sonnet-4",
	"anthropic/claude-sonnet-4.5": "claude-sonnet-4.5", "anthropic/claude-haiku-4.5": "claude-haiku-4.5",
	"claude-sonnet-5": "claude-sonnet-5", "claude-opus-4-6": "claude-opus-4.6",
	"claude-sonnet-4-6": "claude-sonnet-4.6", "claude-sonnet-4-0": "claude-sonnet-4",
	"claude-sonnet-4-5": "claude-sonnet-4.5", "claude-haiku-4-5": "claude-haiku-4.5",
	"anthropic/claude-opus-4-6": "claude-opus-4.6", "anthropic/claude-sonnet-4-6": "claude-sonnet-4.6",
	"anthropic/claude-sonnet-4-0": "claude-sonnet-4", "anthropic/claude-sonnet-4-5": "claude-sonnet-4.5",
	"anthropic/claude-haiku-4-5": "claude-haiku-4.5",
}

// First stored GitHub token that validates and exchanges, every entry tried in order
func firstExchangeableCopilotToken(ctx context.Context, raws []any) string {
	for _, r := range raws {
		raw := strings.TrimSpace(cpOrStr(r))
		if raw == "" {
			continue
		}
		if valid, _ := validateCopilotToken(raw); !valid {
			continue
		}
		jwt, err := exchangeCopilotToken(ctx, raw, 10*time.Second)
		if err != nil {
			continue
		}
		if jwt.token != "" {
			return jwt.token
		}
	}
	return ""
}

// Copilot API token for the catalog, the exchanged env token then an exchanged pool login
func resolveCopilotCatalogAPIKey(ctx context.Context) string {
	envToken := func() (string, error) {
		raw, _, err := resolveCopilotToken(ctx)
		if err != nil || raw == "" {
			return "", err
		}
		token, _ := getCopilotAPIToken(ctx, raw)
		return token, nil
	}
	poolToken := func() (string, error) {
		rows, err := NewStore(home.Dir(ctx)).ReadCredentialPool("copilot")
		if err != nil {
			return "", err
		}
		var raws []any
		for _, row := range rows {
			raws = append(raws, cpGetOr(row, "access_token", nil))
		}
		return firstExchangeableCopilotToken(ctx, raws), nil
	}
	for _, source := range []func() (string, error){envToken, poolToken} {
		token, err := source()
		if err != nil {
			continue
		}
		if token != "" {
			return token
		}
	}
	return ""
}

// Standard Copilot request headers for catalog calls
func copilotDefaultHeaders(isAgentTurn bool) map[string]string {
	return copilotRequestHeaders(isAgentTurn, false)
}

// Object rows of a catalog payload, the data list of an object or the payload list itself
func copilotPayloadItems(payload any) []any {
	data := payload
	if cpIsDict(payload) {
		data = cpGetOr(payload, "data", []any{})
	}
	list, ok := data.([]any)
	if !ok {
		return []any{}
	}
	out := []any{}
	for _, item := range list {
		if cpIsDict(item) {
			out = append(out, item)
		}
	}
	return out
}

// Chat capable catalog row, the picker flag ignored when asked
func copilotCatalogItemIsTextModel(item any, ignorePickerFlag bool) bool {
	if strings.TrimSpace(cpOrStr(cpGetOr(item, "id", nil))) == "" {
		return false
	}
	if flag, ok := cpGet(item, "model_picker_enabled"); !ignorePickerFlag && ok && flag == false {
		return false
	}
	if caps := cpGetOr(item, "capabilities", nil); cpIsDict(caps) {
		modelType := strings.ToLower(strings.TrimSpace(cpOrStr(cpGetOr(caps, "type", nil))))
		if modelType != "" && modelType != "chat" {
			return false
		}
	}
	if endpoints, ok := cpGetOr(item, "supported_endpoints", nil).([]any); ok {
		any_, chat := false, false
		for _, e := range endpoints {
			if s := strings.TrimSpace(pyStr(e)); s != "" {
				any_ = true
				chat = chat || copilotChatEndpoints[s]
			}
		}
		if any_ && !chat {
			return false
		}
	}
	return true
}

// Chat capable catalog rows deduped by id in catalog order
func copilotTextModels(items []any, ignorePickerFlag bool) []any {
	models := []any{}
	seen := map[string]bool{}
	for _, item := range items {
		id := strings.TrimSpace(cpOrStr(cpGetOr(item, "id", nil)))
		if seen[id] {
			continue
		}
		if !copilotCatalogItemIsTextModel(item, ignorePickerFlag) {
			continue
		}
		seen[id] = true
		models = append(models, item)
	}
	return models
}

var (
	copilotCatalogMu       sync.Mutex
	copilotCatalogCache    []any
	copilotCatalogCacheKey string
	copilotCatalogCacheAt  time.Time
)

// Live chat catalog of the account cached five minutes per key, nil when every attempt failed
func fetchGithubModelCatalog(ctx context.Context, apiKey string, timeout time.Duration) []any {
	copilotCatalogMu.Lock()
	if copilotCatalogCache != nil && copilotCatalogCacheKey == apiKey && time.Since(copilotCatalogCacheAt) < copilotCatalogCacheTTL {
		out := any(anyutil.Clone(copilotCatalogCache)).([]any)
		copilotCatalogMu.Unlock()
		return out
	}
	copilotCatalogMu.Unlock()
	var attempts []map[string]string
	if apiKey != "" {
		h := copilotDefaultHeaders(true)
		h["Authorization"] = "Bearer " + apiKey
		attempts = append(attempts, h)
	}
	attempts = append(attempts, copilotDefaultHeaders(true))
	for _, headers := range attempts {
		payload, err := copilotGetJSON(ctx, copilotModelsURL, timeout, headers)
		if err != nil {
			continue
		}
		items := copilotPayloadItems(payload)
		models := copilotTextModels(items, false)
		if len(models) == 0 && len(items) > 0 {
			models = copilotTextModels(items, true)
		}
		if len(models) > 0 {
			copilotCatalogMu.Lock()
			copilotCatalogCache = any(anyutil.Clone(models)).([]any)
			copilotCatalogCacheKey = apiKey
			copilotCatalogCacheAt = time.Now()
			copilotCatalogMu.Unlock()
			return models
		}
	}
	return nil
}

// Live catalog ids, nil when the catalog is unavailable
func fetchGithubModels(ctx context.Context, apiKey string, timeout time.Duration) []string {
	catalog := fetchGithubModelCatalog(ctx, apiKey, timeout)
	if len(catalog) == 0 {
		return nil
	}
	ids := []string{}
	for _, item := range catalog {
		if id := cpGetOr(item, "id", ""); cpTruthy(id) {
			ids = append(ids, pyStr(id))
		}
	}
	return ids
}

// Account catalog ids for the picker, false so core serves the curated list for a home that never chose Copilot
func copilotPickerCatalog(ctx context.Context, _ bool) (live []string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("copilot live catalog fetch failed: %v", r))
			live, ok = nil, false
		}
	}()
	if !githubCallsAllowed(ctx) {
		return nil, false
	}
	live = fetchGithubModels(ctx, resolveCopilotCatalogAPIKey(ctx), copilotCatalogTimeout)
	if len(live) > 0 {
		return live, true
	}
	return nil, false
}

// Reports a base url whose host shares the Copilot catalog
func isGithubModelsBaseURL(baseURL string) bool {
	for _, host := range copilotCatalogHosts {
		if HostMatches(strings.TrimSpace(baseURL), host) {
			return true
		}
	}
	return false
}

var (
	copilotContextMu      sync.Mutex
	copilotContextCache   = map[string]int{}
	copilotContextCacheAt float64
	copilotContextKeyFP   string
)

// Positive integer limit of a catalog row as Python isinstance int sees it, bools counting as ints
func copilotIntLimit(v any) (int, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, false
	case int64:
		return int(n), n > 0
	case *big.Int:
		if n.Sign() <= 0 {
			return 0, false
		}
		if n.IsInt64() {
			return int(n.Int64()), true
		}
		return int(^uint(0) >> 1), true
	}
	return 0, false
}

// max_prompt_tokens of a model from the live catalog cached an hour per credential, zero when unknown
func getCopilotModelContext(ctx context.Context, modelID, apiKey string) int {
	keyFP := ""
	if apiKey != "" {
		keyFP = copilotTokenFingerprint(apiKey)
	}
	copilotContextMu.Lock()
	if len(copilotContextCache) > 0 && copilotContextKeyFP == keyFP && copilotNow()-copilotContextCacheAt < copilotContextCacheTTL {
		v := copilotContextCache[modelID]
		copilotContextMu.Unlock()
		return v
	}
	copilotContextMu.Unlock()
	catalog := fetchGithubModelCatalog(ctx, apiKey, copilotCatalogTimeout)
	if len(catalog) == 0 {
		return 0
	}
	cache := map[string]int{}
	for _, item := range catalog {
		mid := strings.TrimSpace(cpOrStr(cpGetOr(item, "id", nil)))
		caps := cpGetOr(item, "capabilities", nil)
		if !cpTruthy(caps) {
			caps = jsonutil.NewObject()
		}
		if !cpIsDict(caps) {
			return 0
		}
		limits := cpGetOr(caps, "limits", nil)
		if !cpTruthy(limits) {
			limits = jsonutil.NewObject()
		}
		if !cpIsDict(limits) {
			return 0
		}
		if n, ok := copilotIntLimit(cpGetOr(limits, "max_prompt_tokens", nil)); mid != "" && ok {
			cache[mid] = n
		}
	}
	copilotContextMu.Lock()
	copilotContextCache = cache
	copilotContextCacheAt = copilotNow()
	copilotContextKeyFP = keyFP
	copilotContextMu.Unlock()
	return cache[modelID]
}

// Ids of a catalog, fetched for the key when no catalog was given
func copilotCatalogIDs(ctx context.Context, catalog []any, apiKey string) map[string]bool {
	if catalog == nil && apiKey != "" {
		catalog = fetchGithubModelCatalog(ctx, apiKey, copilotCatalogTimeout)
	}
	out := map[string]bool{}
	for _, item := range catalog {
		if mid := strings.TrimSpace(cpOrStr(cpGetOr(item, "id", nil))); mid != "" {
			out[mid] = true
		}
	}
	return out
}

// Copilot native id of a model id, aliases then catalog matches then a stripped vendor prefix
func normalizeCopilotModelID(ctx context.Context, modelID string, catalog []any, apiKey string) string {
	raw := strings.TrimSpace(modelID)
	if raw == "" {
		return ""
	}
	catalogIDs := copilotCatalogIDs(ctx, catalog, apiKey)
	if alias := copilotModelAliases[raw]; alias != "" {
		return alias
	}
	candidates := []string{raw}
	if strings.Contains(raw, "/") {
		candidates = append(candidates, strings.TrimSpace(strings.SplitN(raw, "/", 2)[1]))
	}
	if strings.HasSuffix(raw, "-mini") || strings.HasSuffix(raw, "-nano") || strings.HasSuffix(raw, "-chat") {
		candidates = append(candidates, raw[:len(raw)-5])
	}
	seen := map[string]bool{}
	for _, c := range candidates {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		if alias, ok := copilotModelAliases[c]; ok {
			return alias
		}
		if catalogIDs[c] {
			return c
		}
	}
	if strings.Contains(raw, "/") {
		stripped := strings.TrimSpace(strings.SplitN(raw, "/", 2)[1])
		if stripped != "" && !strings.Contains(stripped, "/") {
			return stripped
		}
	}
	return raw
}

// Effort for a non empty supported list, the nearest weaker listed level, else medium or the first listed
func clampGithubReasoningEffort(effort string, supported []string) string {
	if effort == "" {
		effort = "medium"
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	if !containsStr(supported, effort) {
		effort = clampEffort(effort, supported, nil)
		if !containsStr(supported, effort) {
			if containsStr(supported, "medium") {
				effort = "medium"
			} else {
				effort = supported[0]
			}
		}
	}
	return effort
}

// Pattern efforts of a model id, o series, Astra and GPT-5 families
func githubReasoningEffortsForModelID(ctx context.Context, modelID string) []string {
	raw := strings.ToLower(strings.TrimSpace(modelID))
	for _, p := range []string{"openai/o1", "openai/o3", "openai/o4", "o1", "o3", "o4"} {
		if strings.HasPrefix(raw, p) {
			return append([]string(nil), copilotEffortsOSeries...)
		}
	}
	normalized := strings.ToLower(normalizeCopilotModelID(ctx, modelID, nil, ""))
	if isAstraModel(normalized) {
		return append([]string(nil), copilotAstraEfforts...)
	}
	if strings.HasPrefix(normalized, "gpt-5") {
		return append([]string(nil), copilotEffortsGPT5...)
	}
	return []string{}
}

// Items a Python for loop yields over a legacy capabilities value, panicking where Python raises TypeError
func copilotIterCapabilities(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case string:
		out := []any{}
		for _, r := range t {
			out = append(out, string(r))
		}
		return out
	}
	panic(fmt.Errorf("Copilot model capabilities are %s, expected a list", errchain.Kind(v)))
}

// Reasoning efforts a Copilot visible model supports, the structured catalog list authoritative when present
func githubModelReasoningEfforts(ctx context.Context, modelID string, catalog []any, apiKey string) []string {
	normalized := normalizeCopilotModelID(ctx, modelID, catalog, apiKey)
	if normalized == "" {
		return []string{}
	}
	if catalog == nil && apiKey != "" {
		catalog = fetchGithubModelCatalog(ctx, apiKey, copilotCatalogTimeout)
	}
	var entry any
	for _, item := range catalog {
		if id, ok := cpGetOr(item, "id", nil).(string); ok && id == normalized {
			entry = item
			break
		}
	}
	if entry != nil {
		caps := cpGetOr(entry, "capabilities", nil)
		if cpIsDict(caps) {
			supports := cpGetOr(caps, "supports", nil)
			var efforts any
			if cpIsDict(supports) {
				efforts = cpGetOr(supports, "reasoning_effort", nil)
			}
			list, ok := efforts.([]any)
			if !ok {
				return []string{}
			}
			out := []string{}
			seen := map[string]bool{}
			for _, e := range list {
				if s := strings.ToLower(strings.TrimSpace(pyStr(e))); s != "" && !seen[s] {
					seen[s] = true
					out = append(out, s)
				}
			}
			return out
		}
		reasoning := false
		for _, c := range copilotIterCapabilities(cpGetOr(entry, "capabilities", []any{})) {
			reasoning = reasoning || strings.ToLower(strings.TrimSpace(pyStr(c))) == "reasoning"
		}
		if !reasoning {
			return []string{}
		}
	}
	if modelID == "" {
		modelID = normalized
	}
	return githubReasoningEffortsForModelID(ctx, modelID)
}

// GPT-5 and later use the Responses API except gpt-5-mini, other families use chat completions
func copilotRequiresResponsesAPI(modelID string) bool {
	m := copilotResponsesRe.FindStringSubmatch(modelID)
	if m == nil {
		return false
	}
	n, err := strconv.Atoi(m[1])
	return err == nil && n >= 5 && !strings.HasPrefix(modelID, "gpt-5-mini")
}

// Wire of a Copilot model from its normalized id, the catalog fetched once for the key when not given
func copilotModelAPIMode(ctx context.Context, modelID string, catalog []any, apiKey string) APIMode {
	if catalog == nil && apiKey != "" {
		catalog = fetchGithubModelCatalog(ctx, apiKey, copilotCatalogTimeout)
	}
	normalized := normalizeCopilotModelID(ctx, modelID, catalog, apiKey)
	if normalized != "" && copilotRequiresResponsesAPI(normalized) {
		return ModeCodexResponses
	}
	return ModeChatCompletions
}

// Catalog backed models probe for Copilot and GitHub Models hosts, nil for any other host
func copilotProbeModels(ctx context.Context, baseURL, apiKey string, timeout time.Duration) *ModelsProbe {
	if !isGithubModelsBaseURL(baseURL) {
		return nil
	}
	ids := fetchGithubModels(ctx, apiKey, timeout)
	return &ModelsProbe{IDs: ids, Listed: ids != nil, ProbedURL: copilotModelsURL, ResolvedBaseURL: copilotBaseURL}
}
