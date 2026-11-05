package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ModelVerdict is the switch, persist and recognition outcome of a requested model, an empty Message is none
type ModelVerdict struct {
	Accepted   bool
	Persist    bool
	Recognized bool
	Message    string
}

func verdictAccept() *ModelVerdict {
	return &ModelVerdict{Accepted: true, Persist: true, Recognized: true}
}

func verdictAcceptWithNote(message string) *ModelVerdict {
	return &ModelVerdict{Accepted: true, Persist: true, Recognized: true, Message: message}
}

func verdictReject(message string) *ModelVerdict { return &ModelVerdict{Message: message} }

// Accepts and persists an unrecognized name with a warning
func verdictSoftAccept(message string) *ModelVerdict {
	return &ModelVerdict{Accepted: true, Persist: true, Message: message}
}

// Seams the validation tests replace the way the Python tests patch module attributes
var (
	validateProbeAPIModelsFn = ProbeAPIModels
	validateProbeLMStudioFn  = ProbeLMStudioModels
	validateProbeOllamaFn    = ProbeOllamaLocalModels
	validateFetchAnthropicFn = fetchAnthropicModels
	validateDiscoverBedrock  = DiscoverBedrockModels
	validateBedrockRegionFn  = ResolveBedrockRuntimeRegion
	validateStagedModelIDsFn = func() []string { return stagedModelIDs() }
	validateMoaPresetNamesFn = func(moa any) ([]string, error) { return moaPresetNames(moa) }
)

type catalogMatch struct {
	exact          bool
	suggestionText string
}

// Accepts on exact membership, else nil so the branch composes its own message
func (m catalogMatch) verdict() *ModelVerdict {
	if m.exact {
		return verdictAccept()
	}
	return nil
}

type catalogMatchOptions struct {
	caseInsensitive bool
	suggestQuery    *string
	cutoff          float64
	label           string
}

// Exact membership then up to three close matches as suggestion text, never rewriting the id
func matchInCatalog(query string, candidates []string, opts catalogMatchOptions) catalogMatch {
	if opts.cutoff == 0 {
		opts.cutoff = 0.5
	}
	if opts.label == "" {
		opts.label = "Similar models"
	}
	pool := cloneStrings(candidates)
	suggestQuery := query
	if opts.suggestQuery != nil {
		suggestQuery = *opts.suggestQuery
	}
	var display map[string]string
	if opts.caseInsensitive {
		display = map[string]string{}
		var order []string
		for _, c := range pool {
			lower := strings.ToLower(c)
			if _, seen := display[lower]; !seen {
				order = append(order, lower)
			}
			display[lower] = c
		}
		pool = order
		query, suggestQuery = strings.ToLower(query), strings.ToLower(suggestQuery)
	}
	show := func(id string) string {
		if display != nil {
			return display[id]
		}
		return id
	}
	if containsString(pool, query) {
		return catalogMatch{exact: true}
	}
	suggestions := difflibCloseMatches(suggestQuery, pool, 3, opts.cutoff)
	if len(suggestions) == 0 {
		return catalogMatch{}
	}
	quoted := make([]string, len(suggestions))
	for i, s := range suggestions {
		quoted[i] = "`" + show(s) + "`"
	}
	return catalogMatch{suggestionText: "\n  " + opts.label + ": " + strings.Join(quoted, ", ")}
}

func stringRef(s string) *string { return &s }

// Python isinstance int of a row value, booleans counting as integers
func pyIsInt(v any) (int, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case int:
		return t, true
	case int64:
		return int(t), true
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return 0, false
		}
		n, err := strconv.Atoi(string(t))
		return n, err == nil
	}
	return 0, false
}

// Python difflib get_close_matches over runes with the real quick, quick and full ratio gates
func difflibCloseMatches(word string, possibilities []string, n int, cutoff float64) []string {
	type scored struct {
		score float64
		text  string
	}
	b := []rune(word)
	var results []scored
	for _, x := range possibilities {
		a := []rune(x)
		m := newRuneMatcher(a, b)
		if m.realQuickRatio() >= cutoff && m.quickRatio() >= cutoff {
			if r := m.ratio(); r >= cutoff {
				results = append(results, scored{r, x})
			}
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].text > results[j].text
	})
	if len(results) > n {
		results = results[:n]
	}
	out := make([]string, len(results))
	for i, r := range results {
		out[i] = r.text
	}
	return out
}

type runeMatcher struct {
	a, b []rune
	b2j  map[rune][]int
}

// SequenceMatcher with autojunk popularity pruning for sequences of two hundred runes or more
func newRuneMatcher(a, b []rune) *runeMatcher {
	m := &runeMatcher{a: a, b: b, b2j: map[rune][]int{}}
	for j, r := range b {
		m.b2j[r] = append(m.b2j[r], j)
	}
	if n := len(b); n >= 200 {
		ntest := n/100 + 1
		for r, idx := range m.b2j {
			if len(idx) > ntest {
				delete(m.b2j, r)
			}
		}
	}
	return m
}

func (m *runeMatcher) findLongestMatch(alo, ahi, blo, bhi int) (int, int, int) {
	besti, bestj, bestsize := alo, blo, 0
	j2len := map[int]int{}
	for i := alo; i < ahi; i++ {
		newj2len := map[int]int{}
		for _, j := range m.b2j[m.a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := j2len[j-1] + 1
			newj2len[j] = k
			if k > bestsize {
				besti, bestj, bestsize = i-k+1, j-k+1, k
			}
		}
		j2len = newj2len
	}
	for besti > alo && bestj > blo && !m.isJunk(m.b[bestj-1]) && m.a[besti-1] == m.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && !m.isJunk(m.b[bestj+bestsize]) && m.a[besti+bestsize] == m.b[bestj+bestsize] {
		bestsize++
	}
	for besti > alo && bestj > blo && m.isJunk(m.b[bestj-1]) && m.a[besti-1] == m.b[bestj-1] {
		besti, bestj, bestsize = besti-1, bestj-1, bestsize+1
	}
	for besti+bestsize < ahi && bestj+bestsize < bhi && m.isJunk(m.b[bestj+bestsize]) && m.a[besti+bestsize] == m.b[bestj+bestsize] {
		bestsize++
	}
	return besti, bestj, bestsize
}

func (m *runeMatcher) isJunk(r rune) bool { return false }

func (m *runeMatcher) matches() int {
	type span struct{ alo, ahi, blo, bhi int }
	queue := []span{{0, len(m.a), 0, len(m.b)}}
	total := 0
	for len(queue) > 0 {
		s := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, k := m.findLongestMatch(s.alo, s.ahi, s.blo, s.bhi)
		if k == 0 {
			continue
		}
		total += k
		if s.alo < i && s.blo < j {
			queue = append(queue, span{s.alo, i, s.blo, j})
		}
		if i+k < s.ahi && j+k < s.bhi {
			queue = append(queue, span{i + k, s.ahi, j + k, s.bhi})
		}
	}
	return total
}

func difflibRatio(matches, length int) float64 {
	if length == 0 {
		return 1.0
	}
	return 2.0 * float64(matches) / float64(length)
}

func (m *runeMatcher) ratio() float64 { return difflibRatio(m.matches(), len(m.a)+len(m.b)) }

func (m *runeMatcher) quickRatio() float64 {
	full := map[rune]int{}
	for _, r := range m.b {
		full[r]++
	}
	avail := map[rune]int{}
	matches := 0
	for _, r := range m.a {
		n, ok := avail[r]
		if !ok {
			n = full[r]
		}
		avail[r] = n - 1
		if n > 0 {
			matches++
		}
	}
	return difflibRatio(matches, len(m.a)+len(m.b))
}

func (m *runeMatcher) realQuickRatio() float64 {
	return difflibRatio(min(len(m.a), len(m.b)), len(m.a)+len(m.b))
}

type modelValidateRequest struct {
	requested  string
	lookup     string
	provider   string
	normalized string
	apiKey     string
	baseURL    string
	apiMode    string
	headers    map[string]string
}

// Accepts a MoA preset that the moa config defines
func validateMoa(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	presets, err := validateMoaPresetNamesFn(modelsConfig(ctx)["moa"])
	if err != nil {
		return verdictReject("Could not read MoA presets: " + err.Error())
	}
	if containsString(presets, req.requested) {
		return verdictAccept()
	}
	return verdictReject(fmt.Sprintf("MoA preset `%s` was not found. Run `icebath moa list`.", req.requested))
}

func hasSpace(s string) bool { return strings.IndexFunc(s, pyIsSpace) >= 0 }

// Python str.isspace for one rune
func pyIsSpace(r rune) bool {
	switch r {
	case 0x1c, 0x1d, 0x1e, 0x1f, 0x85:
		return true
	}
	return unicode.IsSpace(r)
}

// Rejects a spaced id unless the provider is self hosted or the base URL is user configured
func rejectWhitespace(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	if hasSpace(req.requested) && !whitespaceAllowed(ctx, req) {
		return verdictReject("Model names cannot contain spaces.")
	}
	return nil
}

var selfHostedProviders = setOf("lmstudio", "ollama", "local", "vllm", "llamacpp", "llama.cpp", "llama-cpp")

func providerToken(provider string) string { return strings.ToLower(strings.TrimSpace(provider)) }

// Local servers and the custom endpoint bucket, aliases that normalize to it included
func isSelfHostedProvider(provider string) bool {
	raw := providerToken(provider)
	if raw == "" {
		return false
	}
	if raw == "custom" || strings.HasPrefix(raw, "custom:") {
		return true
	}
	normalized := catalogNormalizeProvider(raw)
	if normalized == "custom" || strings.HasPrefix(normalized, "custom:") {
		return true
	}
	return selfHostedProviders[raw] || selfHostedProviders[normalized]
}

// Loopback, LAN and single label hosts are a user endpoint, never a vendor catalog
func nonPublicHost(host string) bool {
	host = strings.TrimRight(strings.ToLower(host), ".")
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" || strings.HasSuffix(host, ".localhost") {
		return host != ""
	}
	for _, suffix := range []string{".local", ".lan", ".internal", ".home", ".localdomain"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	if !strings.Contains(host, ".") {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) == 4 {
		octets := make([]int, 4)
		for i, part := range parts {
			if part == "" || strings.IndexFunc(part, func(r rune) bool { return !unicode.IsDigit(r) }) >= 0 {
				return false
			}
			n, err := strconv.Atoi(part)
			if err != nil {
				return false
			}
			octets[i] = n
		}
		if octets[0] == 10 || octets[0] == 127 || (octets[0] == 192 && octets[1] == 168) {
			return true
		}
		if octets[0] == 172 && octets[1] >= 16 && octets[1] <= 31 {
			return true
		}
	}
	return false
}

// Offline vendor host of a provider, empty when the definition is foreign or unknown
func stockHost(provider string) string {
	token := providerToken(provider)
	if token == "" {
		return ""
	}
	pdef, ok := lookupProviderDef(NormalizeProvider(token))
	if !ok || providerToken(pdef.Name) != token {
		return ""
	}
	return baseURLHostname(pdef.BaseURL)
}

// ProviderAllowsModelWhitespace reports whether a spaced id is a real selection rather than a cloud catalog typo
func ProviderAllowsModelWhitespace(provider, baseURL string) bool {
	if isSelfHostedProvider(provider) {
		return true
	}
	rawURL := strings.TrimSpace(baseURL)
	if rawURL == "" {
		return false
	}
	host := baseURLHostname(rawURL)
	if host == "" {
		return false
	}
	if nonPublicHost(host) {
		return true
	}
	raw := providerToken(provider)
	normalized := ""
	if raw != "" {
		normalized = catalogNormalizeProvider(raw)
	}
	key := normalized
	if key == "" {
		key = raw
	}
	stock := stockHost(key)
	return stock != "" && host != stock
}

func whitespaceAllowed(ctx context.Context, req *modelValidateRequest) bool {
	if isSelfHostedProvider(req.normalized) || isSelfHostedProvider(req.provider) {
		return true
	}
	provider := req.provider
	if provider == "" {
		provider = req.normalized
	}
	return ProviderAllowsModelWhitespace(provider, req.baseURL)
}

// OfferedModelIDs drops the ids a picker may not offer because validation refuses their whitespace
func OfferedModelIDs(models []any, provider, baseURL string) []any {
	ids := append([]any{}, models...)
	if ProviderAllowsModelWhitespace(provider, baseURL) {
		return ids
	}
	out := []any{}
	for _, id := range ids {
		if s, ok := id.(string); ok && hasSpace(s) {
			continue
		}
		out = append(out, id)
	}
	return out
}

// DropUnofferableModelIDs removes in place every picker row id validation would refuse for whitespace
func DropUnofferableModelIDs(rows []any) {
	for _, item := range rows {
		row, ok := item.(map[string]any)
		if !ok {
			continue
		}
		provider := pyOrStr(row["slug"])
		baseURLValue := row["api_url"]
		if !pyTruthy(baseURLValue) {
			baseURLValue = row["base_url"]
		}
		baseURL := pyOrStr(baseURLValue)
		if models, ok := row["models"].([]any); ok {
			filtered := OfferedModelIDs(models, provider, baseURL)
			if removed := len(models) - len(filtered); removed > 0 {
				row["models"] = filtered
				if total, ok := pyIsInt(row["total_models"]); ok {
					row["total_models"] = max(0, total-removed)
				}
			}
		}
		if featured, ok := row["featured_models"].([]any); ok {
			row["featured_models"] = OfferedModelIDs(featured, provider, baseURL)
		}
	}
}

var openRouterPresetSlugRe = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)

// Accepts a bare preset unverified and validates the base model of a suffixed preset
func parseOpenRouterPreset(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	const marker = "@preset/"
	if !strings.Contains(req.requested, marker) {
		return nil
	}
	presetSlug, presetBase := "", req.requested
	if strings.Count(req.requested, marker) == 1 {
		presetBase, presetSlug, _ = strings.Cut(req.requested, marker)
	}
	if !openRouterPresetSlugRe.MatchString(presetSlug) {
		return verdictReject("OpenRouter preset slugs must be non-empty URL-safe identifiers using only " +
			"letters, digits, '.', '_', '~', or '-'.")
	}
	if presetBase == "" {
		return verdictSoftAccept("")
	}
	req.lookup = presetBase
	return nil
}

// LM Studio native listing decides, an auth rejection and an unreachable server both reject
func validateLMStudio(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	models, ok, err := validateProbeLMStudioFn(ctx, req.apiKey, req.baseURL, 0)
	if err != nil {
		return verdictReject(err.Error() + " Set `LM_API_KEY` (or update it) to match the server's bearer token.")
	}
	if !ok {
		return verdictReject(fmt.Sprintf("Could not reach LM Studio's `/api/v1/models` to validate `%s`.", req.requested))
	}
	if len(models) == 0 {
		return verdictReject("LM Studio is reachable but no chat-capable models are loaded. " +
			fmt.Sprintf("Load `%s` in LM Studio (Developer tab → Load Model) and try again.", req.requested))
	}
	if containsString(models, req.lookup) {
		return verdictAccept()
	}
	return verdictReject(fmt.Sprintf("Model `%s` was not found in LM Studio's model listing.", req.requested))
}

// Ollama probe headers, configured ones only for the configured endpoint, caller headers and key winning
func ollamaProbeHeaders(ctx context.Context, req *modelValidateRequest) map[string]string {
	configuredBase := configuredOllamaBaseURL(ctx)
	configuredAllowed := configuredBase == "" || sameOllamaNativeRoot(req.baseURL, configuredBase)
	configured := map[string]string{}
	if configuredAllowed {
		var key any
		if req.apiKey != "" {
			key = req.apiKey
		}
		configured = getOllamaNativeHeaders(ctx, req.baseURL, key)
	}
	if req.headers == nil {
		return configured
	}
	out := map[string]string{}
	for k, v := range configured {
		out[k] = v
	}
	dropAuthorization(out)
	for k, v := range req.headers {
		out[k] = v
	}
	if req.apiKey != "" && !hasAuthorization(req.headers) {
		dropAuthorization(out)
		out["Authorization"] = "Bearer " + req.apiKey
	}
	return out
}

// Validates against native api tags whenever the endpoint looks like a local Ollama server
func validateOllamaNative(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	if strings.ToLower(strings.TrimSpace(req.provider)) == "ollama" && req.baseURL == "" {
		req.baseURL = getOllamaBaseURL(ctx)
	}
	headers := ollamaProbeHeaders(ctx, req)
	if !shouldUseOllamaNativeCatalog(ctx, req.provider, req.baseURL, headers) {
		return nil
	}
	models, ok := validateProbeOllamaFn(ctx, req.baseURL, 0, headers)
	if !ok {
		probe := validateProbeAPIModelsFn(ctx, req.apiKey, normalizeOpenAIBaseURL(req.baseURL), 0, "", headers)
		models, ok = probe.Models, probe.Found
	}
	if !ok {
		return verdictSoftAccept(fmt.Sprintf("Note: could not reach this Ollama endpoint's `/api/tags` model listing to validate `%s`. ", req.requested) +
			"Icebath will save the model name, but local Ollama model discovery could not verify it.")
	}
	match := matchInCatalog(req.lookup, models, catalogMatchOptions{label: "Similar local Ollama models"})
	if match.exact {
		return verdictAccept()
	}
	emptyHint := ""
	if len(models) == 0 {
		emptyHint = " No models are currently listed by `/api/tags`."
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` was not found in this Ollama endpoint's `/api/tags` model listing.", req.requested) +
		emptyHint + " It may still work if the server supports hidden or aliased models." + match.suggestionText)
}

// Custom endpoint models probe with the api mode auth shape, chat modes persisting unverified names
func validateCustom(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	apiMode := ""
	if req.apiMode == "anthropic_messages" {
		apiMode = req.apiMode
	}
	probe := validateProbeAPIModelsFn(ctx, req.apiKey, req.baseURL, 0, apiMode, req.headers)
	if probe.Found {
		match := matchInCatalog(req.lookup, probe.Models, catalogMatchOptions{suggestQuery: stringRef(req.requested)})
		if v := match.verdict(); v != nil {
			return v
		}
		message := fmt.Sprintf("Note: `%s` was not found in this custom endpoint's model listing (%s). ", req.requested, pyNoneStr(probe.ProbedURL)) +
			"It may still work if the server supports hidden or aliased models." + match.suggestionText
		if probe.UsedFallback {
			message += fmt.Sprintf("\n  Endpoint verification succeeded after trying `%s`. ", pyNoneStr(probe.ResolvedBaseURL)) +
				"Consider saving that as your base URL."
		}
		return verdictSoftAccept(message)
	}
	accepted := req.apiMode == "chat_completions" || req.apiMode == "anthropic_messages"
	message := fmt.Sprintf("Note: could not reach this custom endpoint's model listing at `%s`. ", pyNoneStr(probe.ProbedURL))
	if accepted {
		message += fmt.Sprintf("`%s` was accepted without verification — if this endpoint does not ", req.requested) +
			"serve it, inference will fail; check the provider's model catalog or the model name."
	} else {
		message += fmt.Sprintf("`%s` was not saved; the endpoint should expose `/models` for verification.", req.requested)
	}
	if probe.SuggestedBaseURL != "" {
		message += fmt.Sprintf("\n  If this server expects `/v1`, try base URL: `%s`", probe.SuggestedBaseURL)
	}
	return &ModelVerdict{Accepted: accepted, Persist: true, Message: message}
}

// Python str of an optional string, None when empty
func pyNoneStr(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

// The picker catalog of a provider, empty on any failure
func validateStaticCatalog(ctx context.Context, normalized string) []string {
	ids := providerModelIDsFn(ctx, normalized, false).IDs
	if ids == nil {
		return []string{}
	}
	return ids
}

var staticFamilyPrefixes = map[string][]string{
	"openai-codex": {"gpt-", "codex-", "o1", "o3", "o4"},
	"xai-oauth":    {"grok-"},
}

var staticLabels = map[string]string{"openai-codex": "OpenAI Codex", "xai-oauth": "xAI Grok OAuth (SuperGrok / Premium+)"}

var familyHeadSplitRe = regexp.MustCompile(`[-./:]`)

// Vendor family token of a model id such as gpt for gpt-5.5
func familyHead(modelID string) string {
	return familyHeadSplitRe.Split(strings.ToLower(strings.TrimSpace(modelID)), 2)[0]
}

// StaticModelConflict names a model another vendor's curated catalog owns while the provider's family lacks it
type StaticModelConflict struct {
	Model       string
	Provider    string
	Suggestions []string
	Message     string
}

// StaticModelProviderConflict checks model and provider coherence offline from the curated catalogs, false when coherent or undecidable
func StaticModelProviderConflict(ctx context.Context, modelName, provider string, limit int) (StaticModelConflict, bool) {
	if limit <= 0 {
		limit = 5
	}
	requested := strings.TrimSpace(modelName)
	normalized := catalogNormalizeProvider(provider)
	catalog := staticCatalogFn(ctx, normalized)
	if requested == "" || len(catalog) == 0 || normalized == "moa" || containsString(AggregatorProviders(), normalized) {
		return StaticModelConflict{}, false
	}
	if modelInProviderCatalog(ctx, strings.ToLower(requested), providerKeys(normalized)) {
		return StaticModelConflict{}, false
	}
	head := familyHead(requested)
	for _, m := range catalog {
		if familyHead(m) == head {
			return StaticModelConflict{}, false
		}
	}
	_, strict := staticFamilyPrefixes[normalized]
	if !strict && len(staticCatalogMatches(ctx, requested, normalized)) == 0 {
		return StaticModelConflict{}, false
	}
	suggestions := difflibCloseMatches(requested, catalog, limit, 0.4)
	if len(suggestions) == 0 {
		suggestions = cloneStrings(catalog[:min(limit, len(catalog))])
	}
	label := CatalogProviderLabel(normalized)
	if label == "" {
		label = normalized
	}
	quoted := make([]string, len(suggestions))
	for i, s := range suggestions {
		quoted[i] = "`" + s + "`"
	}
	return StaticModelConflict{
		Model: requested, Provider: normalized, Suggestions: suggestions,
		Message: fmt.Sprintf("Model `%s` is not served by provider `%s` (%s). ", requested, normalized, label) +
			fmt.Sprintf("Closest %s models: ", label) + strings.Join(quoted, ", ") + ".",
	}, true
}

// Codex and xAI OAuth curated catalog validation with the family plausibility gate
func validateStaticCatalogBranch(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	catalog := validateStaticCatalog(ctx, req.normalized)
	if req.normalized == "openai-codex" {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.lookup)), CodexContextVariantSuffix) && !containsString(catalog, req.lookup) {
			if IsCodexContextVariant(req.lookup) {
				return verdictAccept()
			}
			baseGuess := req.lookup[:len(req.lookup)-len(CodexContextVariantSuffix)]
			return verdictReject(fmt.Sprintf("`%s` is not a valid large-context variant — `%s` enforces the ", req.requested, baseGuess) +
				"standard 272K window on Codex, so no `-900k` option exists for it. Pick the base model, " +
				"or a verified variant from the `/model` picker (e.g. `gpt-5.6-sol-900k`).")
		}
	}
	if len(catalog) == 0 {
		return nil
	}
	match := matchInCatalog(req.lookup, catalog, catalogMatchOptions{})
	if v := match.verdict(); v != nil {
		return v
	}
	label := staticLabels[req.normalized]
	prefixes := staticFamilyPrefixes[req.normalized]
	lower := strings.ToLower(strings.TrimSpace(req.lookup))
	familyMatch := false
	for _, p := range prefixes {
		if strings.HasPrefix(lower, p) {
			familyMatch = true
		}
	}
	if len(prefixes) > 0 && !familyMatch {
		return verdictReject(fmt.Sprintf("`%s` doesn't look like a %s model and isn't in its listing, so it was not ", req.requested, label) +
			"accepted. If it belongs to another configured provider, switch with `--provider <slug>` " +
			"(or select it from the `/model` picker)." + match.suggestionText)
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` was not found in the %s model listing. ", req.requested, label) +
		"It may still work if your account has access to a newer or hidden model ID." + match.suggestionText)
}

// MiniMax static catalog matched case insensitively since it has no models endpoint
func validateMinimax(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	catalog := validateStaticCatalog(ctx, req.normalized)
	if len(catalog) == 0 {
		return nil
	}
	match := matchInCatalog(req.lookup, catalog, catalogMatchOptions{caseInsensitive: true})
	if v := match.verdict(); v != nil {
		return v
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` was not found in the MiniMax catalog.", req.requested) +
		match.suggestionText +
		"\n  MiniMax does not expose a /models endpoint, so Icebath cannot verify the model name." +
		"\n  The model may still work if it exists on the server.")
}

// Native Anthropic listing through the x-api-key fetcher, nil when no token or the network failed
func validateAnthropic(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	models, ok := validateFetchAnthropicFn(ctx, 0, req.baseURL, req.apiKey)
	if !ok {
		return nil
	}
	match := matchInCatalog(req.lookup, models, catalogMatchOptions{suggestQuery: stringRef(req.requested)})
	if v := match.verdict(); v != nil {
		return v
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` was not found in Anthropic's /v1/models listing. ", req.requested) +
		"It may still work if you have early-access or snapshot IDs." + match.suggestionText)
}

// Anthropic Messages transport soft accepts either way and says whether the listing answered
func validateAnthropicMessages(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	models, ok := fetchAPIModelsFn(ctx, req.apiKey, req.baseURL, 0, req.apiMode, nil)
	if !ok {
		return verdictSoftAccept(fmt.Sprintf("Note: could not verify `%s` against this endpoint's model listing.  Many ", req.requested) +
			"Anthropic-compatible proxies do not implement GET /v1/models.  The model name has been accepted " +
			"without verification.")
	}
	match := matchInCatalog(req.lookup, models, catalogMatchOptions{caseInsensitive: true, suggestQuery: stringRef(req.requested), cutoff: 0.4})
	if v := match.verdict(); v != nil {
		return v
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` is not named in this endpoint's model listing (it may still serve it ", req.requested) +
		"under an alias)." + match.suggestionText +
		"\n  The model name has been accepted without verification.")
}

// Lowercased ids of the Portal free and paid recommended feeds, empty on any failure
func nousPortalRecommendedNames(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	payload := fetchNousRecommendedModelsFn(ctx, resolveNousPortalURL(ctx), false)
	for _, tier := range []string{"freeRecommendedModels", "paidRecommendedModels"} {
		entries, _ := payload[tier].([]any)
		for _, entry := range entries {
			if name := extractModelName(entry); name != "" {
				out[strings.ToLower(name)] = true
			}
		}
	}
	return out
}

// A model staged in the managed llama.cpp library accepts, anything else falls through to the live listing
func validateManagedLocal(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	want := strings.ToLower(strings.TrimSpace(req.lookup))
	for _, sid := range validateStagedModelIDsFn() {
		if strings.ToLower(sid) == want {
			return verdictAcceptWithNote(fmt.Sprintf("Note: `%s` was not found in the live /v1/models listing ", req.requested) +
				"but is downloaded in the managed local-models library — accepted.")
		}
	}
	return nil
}

var fetchModelsOverrideProfiles = setOf("actual", "meta-ai", "openrouter", "router", "commandcode", "kimi-coding", "kimi-coding-cn", "custom", "anthropic")

// Whether a profile replaces the generic models listing fetch with its own
func profileOverridesFetchModels(ctx context.Context, p Profile) bool {
	return p.FetchModelsHook != nil || fetchModelsOverrideProfiles[p.Name]
}

// Catalog and authority of a profile whose catalog is not the generic models listing
func profileCatalog(ctx context.Context, normalized, baseURL string) ([]string, bool) {
	profile, ok := modelsProfileForFn(normalized)
	if !ok {
		return []string{}, false
	}
	if baseURL != "" && strings.TrimRight(baseURL, "/") != strings.TrimRight(profile.BaseURL, "/") {
		return []string{}, false
	}
	generic := strings.TrimRight(profile.BaseURL, "/") + "/models"
	ownEndpoint := profile.ModelsURL != "" && strings.TrimRight(profile.ModelsURL, "/") != generic
	if !ownEndpoint && !profileOverridesFetchModels(ctx, profile) {
		return []string{}, false
	}
	catalog := validateStaticCatalog(ctx, normalized)
	return catalog, ownEndpoint && len(catalog) > 0
}

// Generic live models probe, nil when the API was unreachable
func validateLiveListing(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	catalog, authoritative := profileCatalog(ctx, req.normalized, req.baseURL)
	if len(catalog) > 0 {
		match := matchInCatalog(req.lookup, catalog, catalogMatchOptions{suggestQuery: stringRef(req.requested)})
		if match.exact {
			return verdictAccept()
		}
		if authoritative {
			return verdictReject(fmt.Sprintf("Model `%s` was not found in this provider's catalog.%s", req.requested, match.suggestionText))
		}
	}
	apiModels, ok := fetchAPIModelsFn(ctx, req.apiKey, req.baseURL, 0, "", nil)
	if !ok {
		return nil
	}
	if req.normalized == "gemini" {
		stripped := make([]string, len(apiModels))
		for i, m := range apiModels {
			stripped[i] = strings.TrimPrefix(m, "models/")
		}
		apiModels = stripped
	}
	match := matchInCatalog(req.lookup, apiModels, catalogMatchOptions{})
	if match.exact {
		return verdictAccept()
	}
	variantBase := ""
	if req.normalized == "openrouter" {
		variantBase = OpenRouterVariantBase(req.lookup)
	}
	if variantBase != "" && containsString(apiModels, variantBase) {
		return verdictAccept()
	}
	listingAuthoritative := false
	if req.normalized == "openai" || req.normalized == "openai-api" {
		listingAuthoritative = IsOfficialOpenAIHost(req.baseURL)
	}
	curatedKey := req.lookup
	if variantBase != "" {
		curatedKey = variantBase
	}
	if !listingAuthoritative && modelInProviderCatalog(ctx, strings.ToLower(curatedKey), providerKeys(req.normalized)) {
		return verdictAcceptWithNote(fmt.Sprintf("Note: `%s` was not found in the live /v1/models listing ", req.requested) +
			"but exists in the curated catalog — accepted.")
	}
	if req.normalized == "nous" && nousPortalRecommendedNames(ctx)[strings.ToLower(req.lookup)] {
		return verdictAcceptWithNote(fmt.Sprintf("Note: `%s` was not found in the live /v1/models listing ", req.requested) +
			"but is a current Nous Portal recommendation — accepted.")
	}
	return verdictReject(fmt.Sprintf("Model `%s` was not found in this provider's model listing.%s", req.requested, match.suggestionText))
}

// Bedrock control plane discovery, soft accepting a miss for custom inference profiles
func validateBedrock(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	region := validateBedrockRegionFn(ctx)
	seen := map[string]bool{}
	var ids []string
	for _, m := range validateDiscoverBedrock(ctx, region, nil) {
		if !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	match := matchInCatalog(req.requested, ids, catalogMatchOptions{cutoff: 0.4})
	if match.exact {
		return verdictAccept()
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` was not found in Bedrock model discovery for %s. ", req.requested, region) +
		"It may still work with custom inference profiles or cross-account access." + match.suggestionText)
}

// Process providers validate against the picker list plus the profile's short aliases
func validateExternalProcess(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	profile, ok := modelsProfileForFn(req.normalized)
	if !ok || profile.AuthType != AuthTypeExternalProcess {
		return nil
	}
	lookupLower := strings.ToLower(req.lookup)
	for alias := range profile.ModelAliasMap() {
		if strings.ToLower(alias) == lookupLower {
			return verdictAccept()
		}
	}
	catalog := validateStaticCatalog(ctx, req.normalized)
	if len(catalog) == 0 {
		catalog = cloneStrings(profile.FallbackModels)
	}
	match := matchInCatalog(req.lookup, catalog, catalogMatchOptions{caseInsensitive: true})
	if match.exact {
		return verdictAccept()
	}
	name := profile.DisplayName
	if name == "" {
		name = profile.Name
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` is not declared by %s.", req.requested, name) +
		match.suggestionText + "\n  The model may still work if the local client accepts it.")
}

// Curated catalog verdict when the models endpoint is unreachable, always deciding
func validateCatalogFallback(ctx context.Context, req *modelValidateRequest) *ModelVerdict {
	label := CatalogProviderLabel(req.normalized)
	if label == "" {
		label = req.normalized
	}
	catalog := validateStaticCatalog(ctx, req.normalized)
	if len(catalog) == 0 {
		return verdictSoftAccept(fmt.Sprintf("Note: could not reach the %s API to validate `%s`. ", label, req.requested) +
			"If the service isn't down, this model may not be valid.")
	}
	match := matchInCatalog(req.lookup, catalog, catalogMatchOptions{caseInsensitive: true})
	if match.exact {
		return verdictAccept()
	}
	if req.normalized == "openrouter" {
		if variantBase := OpenRouterVariantBase(req.lookup); variantBase != "" {
			for _, m := range catalog {
				if strings.ToLower(m) == strings.ToLower(variantBase) {
					return verdictAccept()
				}
			}
		}
	}
	return verdictSoftAccept(fmt.Sprintf("Note: `%s` was not found in the %s curated catalog ", req.requested, label) +
		"and the /models endpoint was unreachable." + match.suggestionText +
		"\n  The model may still work if it exists on the provider.")
}

func isCustomRequest(req *modelValidateRequest) bool {
	return req.normalized == "custom" || strings.HasPrefix(req.normalized, "custom:")
}

func gateFor(providers ...string) func(*modelValidateRequest) bool {
	set := setOf(providers...)
	return func(req *modelValidateRequest) bool { return set[req.normalized] }
}

func gateAlways(*modelValidateRequest) bool { return true }

type validateRung struct {
	gate   func(*modelValidateRequest) bool
	branch func(context.Context, *modelValidateRequest) *ModelVerdict
}

// Ladder order is behaviour, the first non nil verdict wins and the curated fallback always decides
var validateLadder = []validateRung{
	{gateFor("moa"), validateMoa},
	{gateAlways, rejectWhitespace},
	{gateFor("openrouter"), parseOpenRouterPreset},
	{gateFor("lmstudio"), validateLMStudio},
	{gateAlways, validateOllamaNative},
	{isCustomRequest, validateCustom},
	{gateFor("openai-codex", "xai-oauth"), validateStaticCatalogBranch},
	{gateFor("minimax", "minimax-cn"), validateMinimax},
	{gateFor("llamacpp", "llama.cpp", "llama-cpp"), validateManagedLocal},
	{gateFor("anthropic"), validateAnthropic},
	{func(req *modelValidateRequest) bool { return req.apiMode == "anthropic_messages" }, validateAnthropicMessages},
	{gateAlways, validateExternalProcess},
	{gateAlways, validateLiveListing},
	{gateFor("bedrock"), validateBedrock},
	{gateAlways, validateCatalogFallback},
}

// ValidateModelOptions carries the endpoint a requested model is validated against, nil Headers is none
type ValidateModelOptions struct {
	APIKey  string
	BaseURL string
	APIMode string
	Headers map[string]string
}

// Native id from the profile hook given the request key, empty when no hook exists or it declines
func profileNormalizeModelIDWithKey(provider, model, apiKey string) string {
	modelIDNormalizersMu.RLock()
	fn := modelIDNormalizers[provider]
	if p := getProviderProfile(context.Background(), provider); p != nil && p.NormalizeModelIDHook != nil {
		fn = p.NormalizeModelIDHook
	}
	modelIDNormalizersMu.RUnlock()
	if fn == nil {
		return ""
	}
	return fn(model, apiKey)
}

// ValidateRequestedModel validates a model value for the active provider without ever rewriting the requested id
func ValidateRequestedModel(ctx context.Context, modelName, provider string, opts ValidateModelOptions) ModelVerdict {
	requested := strings.TrimSpace(modelName)
	normalized := catalogNormalizeProvider(provider)
	if normalized == "openrouter" && opts.BaseURL != "" && !baseURLHostMatches(opts.BaseURL, "openrouter.ai") {
		normalized = "custom"
	}
	lookup := ""
	if normalized != "" {
		if profile, ok := modelsProfileForFn(normalized); ok {
			lookup = profileNormalizeModelIDWithKey(profile.Name, requested, opts.APIKey)
		}
	}
	if lookup == "" {
		lookup = requested
	}
	if requested == "" {
		return *verdictReject("Model name cannot be empty.")
	}
	req := &modelValidateRequest{
		requested: requested, lookup: lookup, provider: provider, normalized: normalized,
		apiKey: opts.APIKey, baseURL: opts.BaseURL, apiMode: opts.APIMode, headers: opts.Headers,
	}
	for _, rung := range validateLadder {
		if rung.gate(req) {
			if v := rung.branch(ctx, req); v != nil {
				return *v
			}
		}
	}
	panic("unreachable: the curated catalog fallback always decides")
}
