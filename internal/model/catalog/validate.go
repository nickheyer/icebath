package catalog

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/local"
	"github.com/nickheyer/icebath/internal/model/providers"
)

type Verdict struct {
	Accepted   bool
	Persist    bool
	Recognized bool
	Message    string
}

type ValidationRequest struct {
	Model    string
	Provider string
	Key      auth.Credential
	BaseURL  string
	Mode     string
	Headers  map[string]string
}

type ProviderConflict struct {
	Model       string
	Provider    string
	Suggestions []string
	Message     string
}

type validation struct {
	requested  string
	lookup     string
	provider   string
	normalized string
	apiKey     auth.Secret
	baseURL    string
	mode       string
	headers    map[string]string
}

type catalogMatch struct {
	exact      bool
	suggestion string
}

var (
	selfHostedProviders  = []string{"lmstudio", providerOllama, "local", "vllm", LlamaCppProviderID, "llama.cpp", "llama-cpp"}
	managedLocalNames    = []string{LlamaCppProviderID, "llama.cpp", "llama-cpp"}
	nonPublicSuffixes    = []string{".local", ".lan", ".internal", ".home", ".localdomain"}
	staticFamilyPrefixes = map[string][]string{"openai-codex": {"gpt-", "codex-", "o1", "o3", "o4"}, "xai-oauth": {"grok-"}}
	staticLabels         = map[string]string{"openai-codex": "OpenAI Codex", "xai-oauth": "xAI Grok OAuth (SuperGrok / Premium+)"}
	presetSlug           = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	familySeparators     = "-./:"
)

func accept() *Verdict { return &Verdict{Accepted: true, Persist: true, Recognized: true} }

func acceptWithNote(message string) *Verdict {
	return &Verdict{Accepted: true, Persist: true, Recognized: true, Message: message}
}

func reject(message string) *Verdict { return &Verdict{Message: message} }

func softAccept(message string) *Verdict {
	return &Verdict{Accepted: true, Persist: true, Message: message}
}

func longestMatch(a, b []rune, positions map[rune][]int, alo, ahi, blo, bhi int) (int, int, int) {
	bestI, bestJ, bestSize := alo, blo, 0
	lengths := map[int]int{}
	for i := alo; i < ahi; i++ {
		next := map[int]int{}
		for _, j := range positions[a[i]] {
			if j < blo {
				continue
			}
			if j >= bhi {
				break
			}
			k := lengths[j-1] + 1
			next[j] = k
			if k > bestSize {
				bestI, bestJ, bestSize = i-k+1, j-k+1, k
			}
		}
		lengths = next
	}
	for bestI > alo && bestJ > blo && a[bestI-1] == b[bestJ-1] {
		bestI, bestJ, bestSize = bestI-1, bestJ-1, bestSize+1
	}
	for bestI+bestSize < ahi && bestJ+bestSize < bhi && a[bestI+bestSize] == b[bestJ+bestSize] {
		bestSize++
	}
	return bestI, bestJ, bestSize
}

func similarity(a, b []rune, positions map[rune][]int) float64 {
	total := len(a) + len(b)
	if total == 0 {
		return 1
	}
	matched := 0
	queue := [][4]int{{0, len(a), 0, len(b)}}
	for len(queue) > 0 {
		span := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		i, j, k := longestMatch(a, b, positions, span[0], span[1], span[2], span[3])
		if k == 0 {
			continue
		}
		matched += k
		if span[0] < i && span[2] < j {
			queue = append(queue, [4]int{span[0], i, span[2], j})
		}
		if i+k < span[1] && j+k < span[3] {
			queue = append(queue, [4]int{i + k, span[1], j + k, span[3]})
		}
	}
	return 2.0 * float64(matched) / float64(total)
}

func closeMatches(word string, pool []string, limit int, cutoff float64) []string {
	target := []rune(word)
	positions := map[rune][]int{}
	for j, r := range target {
		positions[r] = append(positions[r], j)
	}
	if n := len(target); n >= 200 {
		for r, at := range positions {
			if len(at) > n/100+1 {
				delete(positions, r)
			}
		}
	}
	type scored struct {
		score float64
		text  string
	}
	var results []scored
	for _, candidate := range pool {
		if score := similarity([]rune(candidate), target, positions); score >= cutoff {
			results = append(results, scored{score, candidate})
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].score != results[j].score {
			return results[i].score > results[j].score
		}
		return results[i].text > results[j].text
	})
	if len(results) > limit {
		results = results[:limit]
	}
	out := make([]string, 0, len(results))
	for _, row := range results {
		out = append(out, row.text)
	}
	return out
}

func matchInCatalog(query string, candidates []string, caseInsensitive bool, suggestQuery string, cutoff float64, label string) catalogMatch {
	pool := candidates
	var display map[string]string
	if suggestQuery == "" {
		suggestQuery = query
	}
	if caseInsensitive {
		display = map[string]string{}
		pool = nil
		for _, candidate := range candidates {
			lower := strings.ToLower(candidate)
			if _, seen := display[lower]; !seen {
				pool = append(pool, lower)
			}
			display[lower] = candidate
		}
		query, suggestQuery = strings.ToLower(query), strings.ToLower(suggestQuery)
	}
	if slices.Contains(pool, query) {
		return catalogMatch{exact: true}
	}
	suggestions := closeMatches(suggestQuery, pool, 3, cutoff)
	if len(suggestions) == 0 {
		return catalogMatch{}
	}
	shown := make([]string, 0, len(suggestions))
	for _, suggestion := range suggestions {
		if display != nil {
			suggestion = display[suggestion]
		}
		shown = append(shown, "`"+suggestion+"`")
	}
	return catalogMatch{suggestion: "\n  " + label + ": " + strings.Join(shown, ", ")}
}

func simpleMatch(query string, candidates []string) catalogMatch {
	return matchInCatalog(query, candidates, false, "", 0.5, "Similar models")
}

func hasSpace(text string) bool { return strings.IndexFunc(text, unicode.IsSpace) >= 0 }

func customBucket(provider string) bool {
	return provider == providerCustom || strings.HasPrefix(provider, "custom:")
}

func (c *Catalog) isSelfHostedProvider(ctx context.Context, provider string) bool {
	raw := strings.ToLower(strings.TrimSpace(provider))
	if raw == "" {
		return false
	}
	if customBucket(raw) {
		return true
	}
	normalized := c.normalizeProvider(ctx, raw)
	return customBucket(normalized) || slices.Contains(selfHostedProviders, raw) || slices.Contains(selfHostedProviders, normalized)
}

func nonPublicHost(host string) bool {
	host = strings.TrimRight(strings.ToLower(host), ".")
	if host == "" {
		return false
	}
	if slices.Contains(localHosts, host) || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if slices.ContainsFunc(nonPublicSuffixes, func(suffix string) bool { return strings.HasSuffix(host, suffix) }) {
		return true
	}
	if !strings.Contains(host, ".") {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	octets := make([]int, 0, 4)
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" || len(part) > 9 {
			return false
		}
		value := 0
		for _, digit := range part {
			value = value*10 + int(digit-'0')
		}
		octets = append(octets, value)
	}
	if octets[0] == 10 || octets[0] == 127 || (octets[0] == 192 && octets[1] == 168) {
		return true
	}
	return octets[0] == 172 && octets[1] >= 16 && octets[1] <= 31
}

func (c *Catalog) stockHost(ctx context.Context, provider string) string {
	token := strings.ToLower(strings.TrimSpace(provider))
	if token == "" {
		return ""
	}
	definition, ok := c.provider(ctx, token, false)
	if !ok || strings.ToLower(strings.TrimSpace(definition.ID)) != token {
		return ""
	}
	return providers.URLHostname(definition.BaseURL)
}

func (c *Catalog) providerAllowsModelWhitespace(ctx context.Context, provider, baseURL string) bool {
	if c.isSelfHostedProvider(ctx, provider) {
		return true
	}
	target := strings.TrimSpace(baseURL)
	if target == "" {
		return false
	}
	host := providers.URLHostname(target)
	if host == "" {
		return false
	}
	if nonPublicHost(host) {
		return true
	}
	raw := strings.ToLower(strings.TrimSpace(provider))
	normalized := ""
	if raw != "" {
		normalized = c.normalizeProvider(ctx, raw)
	}
	stock := c.stockHost(ctx, firstNonEmpty(normalized, raw))
	return stock != "" && host != stock
}

func (c *Catalog) ProviderAllowsModelWhitespace(ctx context.Context, provider, baseURL string) bool {
	return c.providerAllowsModelWhitespace(c.scoped(ctx), provider, baseURL)
}

func (c *Catalog) OfferedModelIDs(ctx context.Context, models []string, provider, baseURL string) []string {
	return c.offeredModelIDs(c.scoped(ctx), models, provider, baseURL)
}

func (c *Catalog) offeredModelIDs(ctx context.Context, models []string, provider, baseURL string) []string {
	if c.providerAllowsModelWhitespace(ctx, provider, baseURL) {
		return slices.Clone(models)
	}
	return slices.DeleteFunc(slices.Clone(models), hasSpace)
}

func (c *Catalog) dropUnofferableModelIDs(ctx context.Context, rows []ProviderRow) {
	for i := range rows {
		row := &rows[i]
		if row.Models != nil {
			offered := c.offeredModelIDs(ctx, row.Models, row.Slug, row.APIURL)
			if removed := len(row.Models) - len(offered); removed > 0 {
				row.Models = offered
				row.TotalModels = max(0, row.TotalModels-removed)
			}
		}
		if row.Featured != nil {
			row.Featured = c.offeredModelIDs(ctx, row.Featured, row.Slug, row.APIURL)
		}
	}
}

func (c *Catalog) DropUnofferableModelIDs(ctx context.Context, rows []ProviderRow) {
	c.dropUnofferableModelIDs(c.scoped(ctx), rows)
}

func (c *Catalog) validateMoA(ctx context.Context, req *validation) *Verdict {
	cfg, err := c.cfg.LoadReadonly(ctx)
	if err != nil {
		return reject("Could not read MoA presets: " + err.Error())
	}
	if _, ok := cfg.MoA.Presets.Get(req.requested); ok {
		return accept()
	}
	return reject("MoA preset `" + req.requested + "` was not found. Run `icebath moa list`.")
}

func (c *Catalog) rejectWhitespace(ctx context.Context, req *validation) *Verdict {
	if !hasSpace(req.requested) {
		return nil
	}
	if c.isSelfHostedProvider(ctx, req.normalized) || c.isSelfHostedProvider(ctx, req.provider) {
		return nil
	}
	if c.providerAllowsModelWhitespace(ctx, firstNonEmpty(req.provider, req.normalized), req.baseURL) {
		return nil
	}
	return reject("Model names cannot contain spaces.")
}

func parseOpenRouterPreset(req *validation) *Verdict {
	const marker = "@preset/"
	if !strings.Contains(req.requested, marker) {
		return nil
	}
	slug, base := "", req.requested
	if strings.Count(req.requested, marker) == 1 {
		base, slug, _ = strings.Cut(req.requested, marker)
	}
	if !presetSlug.MatchString(slug) {
		return reject("OpenRouter preset slugs must be non-empty URL-safe identifiers using only " +
			"letters, digits, '.', '_', '~', or '-'.")
	}
	if base == "" {
		return softAccept("")
	}
	req.lookup = base
	return nil
}

func (c *Catalog) validateLMStudio(ctx context.Context, req *validation) (*Verdict, error) {
	models, reachable, err := c.local.ProbeLMStudioModels(ctx, local.ProbeKey{Static: req.apiKey.Reveal()}, req.baseURL, apiModelsTimeout)
	if err != nil {
		var authErr *auth.Error
		if errors.As(err, &authErr) {
			return reject(authErr.Error() + " Set `LM_API_KEY` (or update it) to match the server's bearer token."), nil
		}
		return nil, err
	}
	switch {
	case !reachable:
		return reject("Could not reach LM Studio's `/api/v1/models` to validate `" + req.requested + "`."), nil
	case len(models) == 0:
		return reject("LM Studio is reachable but no chat-capable models are loaded. " +
			"Load `" + req.requested + "` in LM Studio (Developer tab → Load Model) and try again."), nil
	case slices.Contains(models, req.lookup):
		return accept(), nil
	}
	return reject("Model `" + req.requested + "` was not found in LM Studio's model listing."), nil
}

func dropAuthorization(headers map[string]string) {
	for name := range headers {
		if strings.EqualFold(name, "authorization") {
			delete(headers, name)
		}
	}
}

func (c *Catalog) configuredOllamaBaseURL(ctx context.Context) string {
	cfg := c.config(ctx)
	if cfg == nil {
		return ""
	}
	entry, ok := cfg.Providers.Get(providerOllama)
	if !ok {
		return ""
	}
	return strings.TrimSpace(firstNonEmpty(entry.BaseURL, entry.API, entry.URL))
}

func (c *Catalog) ollamaProbeHeaders(ctx context.Context, req *validation) (map[string]string, error) {
	configured := map[string]string{}
	if base := c.configuredOllamaBaseURL(ctx); base == "" || local.SameOllamaNativeRoot(req.baseURL, base) {
		headers, err := c.local.OllamaNativeHeaders(ctx, req.baseURL, local.ProbeKey{Static: req.apiKey.Reveal()})
		if err != nil {
			return nil, err
		}
		for name, value := range headers {
			configured[name] = value
		}
	}
	if req.headers == nil {
		return configured, nil
	}
	dropAuthorization(configured)
	callerAuth := false
	for name, value := range req.headers {
		configured[name] = value
		callerAuth = callerAuth || strings.EqualFold(name, "authorization")
	}
	if key := req.apiKey.Reveal(); key != "" && !callerAuth {
		dropAuthorization(configured)
		configured["Authorization"] = "Bearer " + key
	}
	return configured, nil
}

func (c *Catalog) validateOllamaNative(ctx context.Context, req *validation) (*Verdict, error) {
	if strings.ToLower(strings.TrimSpace(req.provider)) == providerOllama && req.baseURL == "" {
		base, err := c.local.OllamaBaseURL(ctx)
		if err != nil {
			return nil, err
		}
		req.baseURL = base
	}
	headers, err := c.ollamaProbeHeaders(ctx, req)
	if err != nil {
		return nil, err
	}
	native, err := c.local.ShouldUseOllamaNativeCatalog(ctx, req.provider, req.baseURL, headers)
	if err != nil {
		return nil, err
	}
	if !native {
		return nil, nil
	}
	models, listed, err := c.local.ProbeOllamaLocalModels(ctx, req.baseURL, 0, headers)
	if err != nil {
		return nil, err
	}
	if !listed {
		models, listed = c.fetchAPIModels(ctx, req.apiKey, local.NormalizeOpenAIBaseURL(req.baseURL), apiModelsTimeout, "", headers)
	}
	if !listed {
		return softAccept("Note: could not reach this Ollama endpoint's `/api/tags` model listing to validate `" + req.requested + "`. " +
			"icebath will save the model name, but local Ollama model discovery could not verify it."), nil
	}
	match := matchInCatalog(req.lookup, models, false, "", 0.5, "Similar local Ollama models")
	if match.exact {
		return accept(), nil
	}
	emptyHint := ""
	if len(models) == 0 {
		emptyHint = " No models are currently listed by `/api/tags`."
	}
	return softAccept("Note: `" + req.requested + "` was not found in this Ollama endpoint's `/api/tags` model listing." +
		emptyHint + " It may still work if the server supports hidden or aliased models." + match.suggestion), nil
}

func (c *Catalog) validateCustom(ctx context.Context, req *validation) *Verdict {
	mode := ""
	if req.mode == ModeAnthropicMessages {
		mode = req.mode
	}
	probe := c.probeAPIModels(ctx, req.apiKey, req.baseURL, apiModelsTimeout, mode, req.headers)
	if probe.Listed {
		match := matchInCatalog(req.lookup, probe.Models, false, req.requested, 0.5, "Similar models")
		if match.exact {
			return accept()
		}
		message := "Note: `" + req.requested + "` was not found in this custom endpoint's model listing " +
			"(" + probe.ProbedURL + "). It may still work if the server supports hidden or aliased models." + match.suggestion
		if probe.UsedFallback {
			message += "\n  Endpoint verification succeeded after trying `" + probe.ResolvedBaseURL + "`. " +
				"Consider saving that as your base URL."
		}
		return softAccept(message)
	}
	accepted := req.mode == "chat_completions" || req.mode == ModeAnthropicMessages
	message := "Note: could not reach this custom endpoint's model listing at `" + probe.ProbedURL + "`. "
	if accepted {
		message += "`" + req.requested + "` was accepted without verification — if this endpoint does not " +
			"serve it, inference will fail; check the provider's model catalog or the model name."
	} else {
		message += "`" + req.requested + "` was not saved; the endpoint should expose `/models` for verification."
	}
	if probe.SuggestedBaseURL != "" {
		message += "\n  If this server expects `/v1`, try base URL: `" + probe.SuggestedBaseURL + "`"
	}
	return &Verdict{Accepted: accepted, Persist: true, Message: message}
}

func (c *Catalog) pickerCatalog(ctx context.Context, normalized string) []string {
	rows, err := c.providerModelIDs(ctx, normalized, false)
	if err != nil {
		return nil
	}
	return rows.ids
}

func familyHead(modelID string) string {
	lower := strings.ToLower(strings.TrimSpace(modelID))
	if i := strings.IndexAny(lower, familySeparators); i >= 0 {
		return lower[:i]
	}
	return lower
}

func (c *Catalog) StaticModelProviderConflict(ctx context.Context, modelName, provider string, limit int) (ProviderConflict, bool) {
	ctx = c.scoped(ctx)
	requested := strings.TrimSpace(modelName)
	normalized := c.normalizeProvider(ctx, provider)
	catalog, _ := c.builtinStaticRow(ctx, normalized)
	if requested == "" || len(catalog) == 0 || normalized == "moa" || c.isAggregatorProvider(ctx, normalized) {
		return ProviderConflict{}, false
	}
	if c.modelInProviderCatalog(ctx, strings.ToLower(requested), c.providerKeys(ctx, normalized)) {
		return ProviderConflict{}, false
	}
	head := familyHead(requested)
	if slices.ContainsFunc(catalog, func(id string) bool { return familyHead(id) == head }) {
		return ProviderConflict{}, false
	}
	if _, strict := staticFamilyPrefixes[normalized]; !strict && len(c.staticCatalogMatches(ctx, requested, normalized)) == 0 {
		return ProviderConflict{}, false
	}
	suggestions := closeMatches(requested, catalog, limit, 0.4)
	if len(suggestions) == 0 {
		suggestions = slices.Clone(catalog[:min(limit, len(catalog))])
	}
	label, ok := c.tables(ctx).labels[normalized]
	if !ok {
		label = normalized
	}
	quoted := make([]string, 0, len(suggestions))
	for _, suggestion := range suggestions {
		quoted = append(quoted, "`"+suggestion+"`")
	}
	return ProviderConflict{
		Model: requested, Provider: normalized, Suggestions: suggestions,
		Message: "Model `" + requested + "` is not served by provider `" + normalized + "` (" + label + "). " +
			"Closest " + label + " models: " + strings.Join(quoted, ", ") + ".",
	}, true
}

func (c *Catalog) validateStaticCatalog(ctx context.Context, req *validation) *Verdict {
	catalog := c.pickerCatalog(ctx, req.normalized)
	if req.normalized == "openai-codex" {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(req.lookup)), CodexContextVariantSuffix) && !slices.Contains(catalog, req.lookup) {
			if IsCodexContextVariant(req.lookup) {
				return accept()
			}
			baseGuess := req.lookup[:len(req.lookup)-len(CodexContextVariantSuffix)]
			return reject("`" + req.requested + "` is not a valid large-context variant — `" + baseGuess + "` enforces the " +
				"standard 272K window on Codex, so no `-900k` option exists for it. Pick the base model, " +
				"or a verified variant from the `/model` picker (e.g. `gpt-5.6-sol-900k`).")
		}
	}
	if len(catalog) == 0 {
		return nil
	}
	match := simpleMatch(req.lookup, catalog)
	if match.exact {
		return accept()
	}
	label := staticLabels[req.normalized]
	lower := strings.ToLower(strings.TrimSpace(req.lookup))
	prefixes := staticFamilyPrefixes[req.normalized]
	if len(prefixes) > 0 && !slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(lower, prefix) }) {
		return reject("`" + req.requested + "` doesn't look like a " + label + " model and isn't in its listing, so it was not " +
			"accepted. If it belongs to another configured provider, switch with `--provider <slug>` " +
			"(or select it from the `/model` picker)." + match.suggestion)
	}
	return softAccept("Note: `" + req.requested + "` was not found in the " + label + " model listing. " +
		"It may still work if your account has access to a newer or hidden model ID." + match.suggestion)
}

func (c *Catalog) validateMiniMax(ctx context.Context, req *validation) *Verdict {
	catalog := c.pickerCatalog(ctx, req.normalized)
	if len(catalog) == 0 {
		return nil
	}
	match := matchInCatalog(req.lookup, catalog, true, "", 0.5, "Similar models")
	if match.exact {
		return accept()
	}
	return softAccept("Note: `" + req.requested + "` was not found in the MiniMax catalog." + match.suggestion +
		"\n  MiniMax does not expose a /models endpoint, so icebath cannot verify the model name." +
		"\n  The model may still work if it exists on the server.")
}

func (c *Catalog) validateAnthropic(ctx context.Context, req *validation) *Verdict {
	models, ok := c.fetchAnthropicModels(ctx, anthropicListTimeout, req.baseURL, req.apiKey)
	if !ok {
		return nil
	}
	match := matchInCatalog(req.lookup, models, false, req.requested, 0.5, "Similar models")
	if match.exact {
		return accept()
	}
	return softAccept("Note: `" + req.requested + "` was not found in Anthropic's /v1/models listing. " +
		"It may still work if you have early-access or snapshot IDs." + match.suggestion)
}

func (c *Catalog) validateAnthropicMessages(ctx context.Context, req *validation) *Verdict {
	models, listed := c.fetchAPIModels(ctx, req.apiKey, req.baseURL, apiModelsTimeout, req.mode, nil)
	if !listed {
		return softAccept("Note: could not verify `" + req.requested + "` against this endpoint's model listing.  Many " +
			"Anthropic-compatible proxies do not implement GET /v1/models.  The model name has been accepted " +
			"without verification.")
	}
	match := matchInCatalog(req.lookup, models, true, req.requested, 0.4, "Similar models")
	if match.exact {
		return accept()
	}
	return softAccept("Note: `" + req.requested + "` is not named in this endpoint's model listing (it may still serve it " +
		"under an alias)." + match.suggestion + "\n  The model name has been accepted without verification.")
}

func (c *Catalog) portalRecommendedNames(ctx context.Context) []string {
	payload := c.fetchPortalRecommendedModels(ctx, c.resolvePortalURL(ctx), recommendedTimeout, false)
	var names []string
	for _, tier := range []string{"freeRecommendedModels", "paidRecommendedModels"} {
		for _, entry := range payload.Get(tier).Elems() {
			if name, ok := extractModelName(entry); ok {
				names = append(names, strings.ToLower(name))
			}
		}
	}
	return names
}

func (c *Catalog) validateManagedLocal(req *validation) *Verdict {
	wanted := strings.ToLower(strings.TrimSpace(req.lookup))
	for _, staged := range c.local.StagedModelIDs() {
		if strings.ToLower(staged) == wanted {
			return acceptWithNote("Note: `" + req.requested + "` was not found in the live /v1/models listing " +
				"but is downloaded in the managed local-models library — accepted.")
		}
	}
	return nil
}

func (c *Catalog) profileCatalog(ctx context.Context, normalized, baseURL string) (catalog []string, authoritative bool) {
	p, ok := c.reg.Get(ctx, normalized)
	if !ok {
		return nil, false
	}
	spec := p.Spec()
	if baseURL != "" && strings.TrimRight(baseURL, "/") != strings.TrimRight(spec.BaseURL, "/") {
		return nil, false
	}
	generic := strings.TrimRight(spec.BaseURL, "/") + "/models"
	ownEndpoint := spec.ModelsURL != "" && strings.TrimRight(spec.ModelsURL, "/") != generic
	if _, overrides := p.(providers.ModelLister); !ownEndpoint && !overrides {
		return nil, false
	}
	catalog = c.pickerCatalog(ctx, normalized)
	return catalog, ownEndpoint && len(catalog) > 0
}

func (c *Catalog) validateLiveListing(ctx context.Context, req *validation) *Verdict {
	catalog, authoritative := c.profileCatalog(ctx, req.normalized, req.baseURL)
	if len(catalog) > 0 {
		match := matchInCatalog(req.lookup, catalog, false, req.requested, 0.5, "Similar models")
		if match.exact {
			return accept()
		}
		if authoritative {
			return reject("Model `" + req.requested + "` was not found in this provider's catalog." + match.suggestion)
		}
	}
	listedModels, listed := c.fetchAPIModels(ctx, req.apiKey, req.baseURL, apiModelsTimeout, "", nil)
	if !listed {
		return nil
	}
	if req.normalized == "gemini" {
		for i, id := range listedModels {
			listedModels[i] = strings.TrimPrefix(id, "models/")
		}
	}
	match := simpleMatch(req.lookup, listedModels)
	if match.exact {
		return accept()
	}
	variantBase, variant := "", false
	if req.normalized == providerOpenRouter {
		variantBase, variant = OpenRouterVariantBase(req.lookup)
	}
	if variant && slices.Contains(listedModels, variantBase) {
		return accept()
	}
	authoritativeListing := (req.normalized == "openai" || req.normalized == "openai-api") && providers.IsOfficialOpenAIHost(req.baseURL)
	curatedName := req.lookup
	if variant && variantBase != "" {
		curatedName = variantBase
	}
	if !authoritativeListing && c.modelInProviderCatalog(ctx, strings.ToLower(curatedName), c.providerKeys(ctx, req.normalized)) {
		return acceptWithNote("Note: `" + req.requested + "` was not found in the live /v1/models listing " +
			"but exists in the curated catalog — accepted.")
	}
	if req.normalized == providers.ProviderPortal && slices.Contains(c.portalRecommendedNames(ctx), strings.ToLower(req.lookup)) {
		return acceptWithNote("Note: `" + req.requested + "` was not found in the live /v1/models listing " +
			"but is a current Icebath Portal recommendation — accepted.")
	}
	return reject("Model `" + req.requested + "` was not found in this provider's model listing." + match.suggestion)
}

func (c *Catalog) validateBedrock(ctx context.Context, req *validation) *Verdict {
	owner, ok := c.reg.BedrockOwner()
	if !ok {
		return nil
	}
	access, err := owner.Bedrock(ctx, true)
	if err != nil {
		return nil
	}
	region := access.RuntimeRegion()
	var discovered []string
	for _, model := range c.DiscoverBedrockModels(ctx, region, nil) {
		if !slices.Contains(discovered, model.ID) {
			discovered = append(discovered, model.ID)
		}
	}
	match := matchInCatalog(req.requested, discovered, false, "", 0.4, "Similar models")
	if match.exact {
		return accept()
	}
	return softAccept("Note: `" + req.requested + "` was not found in Bedrock model discovery for " + region + ". " +
		"It may still work with custom inference profiles or cross-account access." + match.suggestion)
}

func (c *Catalog) validateExternalProcess(ctx context.Context, req *validation) *Verdict {
	p, ok := c.reg.Get(ctx, req.normalized)
	if !ok || p.Spec().AuthTypeOrDefault() != auth.AuthTypeExternalProcess {
		return nil
	}
	spec := p.Spec()
	for alias := range spec.ModelAliases {
		if strings.EqualFold(alias, req.lookup) {
			return accept()
		}
	}
	catalog := c.pickerCatalog(ctx, req.normalized)
	if len(catalog) == 0 {
		catalog = spec.FallbackModels
	}
	match := matchInCatalog(req.lookup, catalog, true, "", 0.5, "Similar models")
	if match.exact {
		return accept()
	}
	return softAccept("Note: `" + req.requested + "` is not declared by " + firstNonEmpty(spec.DisplayName, spec.Name) + "." +
		match.suggestion + "\n  The model may still work if the local client accepts it.")
}

func (c *Catalog) validateCatalogFallback(ctx context.Context, req *validation) *Verdict {
	label, ok := c.tables(ctx).labels[req.normalized]
	if !ok {
		label = req.normalized
	}
	catalog := c.pickerCatalog(ctx, req.normalized)
	if len(catalog) == 0 {
		return softAccept("Note: could not reach the " + label + " API to validate `" + req.requested + "`. " +
			"If the service isn't down, this model may not be valid.")
	}
	match := matchInCatalog(req.lookup, catalog, true, "", 0.5, "Similar models")
	if match.exact {
		return accept()
	}
	if req.normalized == providerOpenRouter {
		if base, ok := OpenRouterVariantBase(req.lookup); ok && slices.ContainsFunc(catalog, func(id string) bool { return strings.EqualFold(id, base) }) {
			return accept()
		}
	}
	return softAccept("Note: `" + req.requested + "` was not found in the " + label + " curated catalog " +
		"and the /models endpoint was unreachable." + match.suggestion +
		"\n  The model may still work if it exists on the provider.")
}

func (c *Catalog) ValidateRequestedModel(ctx context.Context, q ValidationRequest) (Verdict, error) {
	ctx = c.scoped(ctx)
	requested := strings.TrimSpace(q.Model)
	normalized := c.normalizeProvider(ctx, q.Provider)
	if normalized == providerOpenRouter && q.BaseURL != "" && !providers.HostMatches(q.BaseURL, "openrouter.ai") {
		normalized = providerCustom
	}
	apiKey := q.Key.ProbeKey(ctx)
	lookup := requested
	if p, ok := c.reg.Get(ctx, normalized); ok {
		if hook, ok := p.(providers.ModelNormalizer); ok {
			if rewritten, ok := hook.NormalizeModel(ctx, requested, apiKey); ok && rewritten != "" {
				lookup = rewritten
			}
		}
	}
	if requested == "" {
		return *reject("Model name cannot be empty."), nil
	}
	req := &validation{
		requested: requested, lookup: lookup, provider: q.Provider, normalized: normalized,
		apiKey: apiKey, baseURL: q.BaseURL, mode: q.Mode, headers: q.Headers,
	}
	is := func(names ...string) bool { return slices.Contains(names, req.normalized) }
	decided := func(v *Verdict) (Verdict, error) { return *v, nil }
	if is("moa") {
		return decided(c.validateMoA(ctx, req))
	}
	if v := c.rejectWhitespace(ctx, req); v != nil {
		return decided(v)
	}
	if is(providerOpenRouter) {
		if v := parseOpenRouterPreset(req); v != nil {
			return decided(v)
		}
	}
	if is("lmstudio") {
		v, err := c.validateLMStudio(ctx, req)
		if err != nil {
			return Verdict{}, err
		}
		return decided(v)
	}
	if v, err := c.validateOllamaNative(ctx, req); err != nil {
		return Verdict{}, err
	} else if v != nil {
		return decided(v)
	}
	if customBucket(req.normalized) {
		return decided(c.validateCustom(ctx, req))
	}
	if is("openai-codex", "xai-oauth") {
		if v := c.validateStaticCatalog(ctx, req); v != nil {
			return decided(v)
		}
	}
	if is("minimax", "minimax-cn") {
		if v := c.validateMiniMax(ctx, req); v != nil {
			return decided(v)
		}
	}
	if is(managedLocalNames...) {
		if v := c.validateManagedLocal(req); v != nil {
			return decided(v)
		}
	}
	if is("anthropic") {
		if v := c.validateAnthropic(ctx, req); v != nil {
			return decided(v)
		}
	}
	if req.mode == ModeAnthropicMessages {
		return decided(c.validateAnthropicMessages(ctx, req))
	}
	if v := c.validateExternalProcess(ctx, req); v != nil {
		return decided(v)
	}
	if v := c.validateLiveListing(ctx, req); v != nil {
		return decided(v)
	}
	if is("bedrock") {
		if v := c.validateBedrock(ctx, req); v != nil {
			return decided(v)
		}
	}
	return decided(c.validateCatalogFallback(ctx, req))
}
