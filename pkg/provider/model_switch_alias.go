package provider

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Suffixes that rank a model above the bare family member when versions tie
var flagshipSuffixes = map[string]bool{"pro": true, "max": true, "plus": true, "turbo": true, "sol": true, "astra": true}

// Sort key preferring higher versions, ranked suffix tokens, then newer date stamps after bare ids
func modelSortKey(modelID, prefix string) []any {
	runes := []rune(modelID)
	rest := ""
	if n := len([]rune(prefix)); n < len(runes) {
		rest = string(runes[n:])
	}
	rest = pyStripSpace(strings.TrimLeft(strings.TrimPrefix(rest, "/"), "-"))
	nums, suffixBuf := splitVersionSuffix(rest)
	suffix := pyStripSpace(strings.Trim(strings.ToLower(suffixBuf), "-_."))
	key := []any{}
	dateStamp := 0.0
	for _, n := range nums {
		if n < 19_000_101 {
			key = append(key, -n)
		} else if n > dateStamp {
			dateStamp = n
		}
	}
	rank := 1
	if flagshipSuffixes[suffix] {
		rank = 0
	}
	key = append(key, rank, suffix)
	if dateStamp == 0 {
		return append(key, 0.0, 0.0)
	}
	return append(key, 1.0, -dateStamp)
}

// Unicode digits outside Nd that Python isdigit accepts
var pyOtherDigits = &unicode.RangeTable{R16: []unicode.Range16{
	{Lo: 0x00b2, Hi: 0x00b3, Stride: 1}, {Lo: 0x00b9, Hi: 0x00b9, Stride: 1}, {Lo: 0x2070, Hi: 0x2070, Stride: 1},
	{Lo: 0x2074, Hi: 0x2079, Stride: 1}, {Lo: 0x2080, Hi: 0x2089, Stride: 1}, {Lo: 0x2460, Hi: 0x2468, Stride: 1},
	{Lo: 0x2474, Hi: 0x247c, Stride: 1}, {Lo: 0x2488, Hi: 0x2490, Stride: 1}, {Lo: 0x24ea, Hi: 0x24ea, Stride: 1},
	{Lo: 0x24f5, Hi: 0x24fd, Stride: 1}, {Lo: 0x24ff, Hi: 0x24ff, Stride: 1}, {Lo: 0x2776, Hi: 0x277e, Stride: 1},
	{Lo: 0x2780, Hi: 0x2788, Stride: 1}, {Lo: 0x278a, Hi: 0x2792, Stride: 1}}}

func pyIsDigit(r rune) bool { return unicode.IsDigit(r) || unicode.Is(pyOtherDigits, r) }

// Python float of a digit and dot run, decimal digits of any script mapped to ASCII
func pyFloatOfRun(run string) (float64, bool) {
	var b strings.Builder
	for _, r := range run {
		switch {
		case r == '.' || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		case unicode.IsDigit(r):
			zero := r
			for unicode.IsDigit(zero - 1) {
				zero--
			}
			b.WriteRune('0' + (r-zero)%10)
		default:
			return 0, false
		}
	}
	f, err := strconv.ParseFloat(b.String(), 64)
	return f, err == nil
}

// Leading version numbers and the remaining suffix, a second dot inside a run starting a new number
func splitVersionSuffix(rest string) ([]float64, string) {
	rs := []rune(rest)
	nums := []float64{}
	run := ""
	flush := func() {
		if f, ok := pyFloatOfRun(strings.TrimRight(run, ".")); ok {
			nums = append(nums, f)
		}
		run = ""
	}
	pos := 0
	for pos < len(rs) {
		ch := rs[pos]
		if ch == '-' || ch == '_' || ch == '.' {
			pos++
			continue
		}
		if !(ch == 'v' || ch == 'V' || pyIsDigit(ch)) {
			break
		}
		if ch == 'v' || ch == 'V' {
			pos++
		}
		for pos < len(rs) && (pyIsDigit(rs[pos]) || rs[pos] == '.') {
			if rs[pos] == '.' && strings.Contains(run, ".") {
				flush()
			} else {
				run += string(rs[pos])
			}
			pos++
		}
		flush()
		if pos < len(rs) && rs[pos] != '-' && rs[pos] != '_' {
			break
		}
	}
	return nums, string(rs[pos:])
}

// Python name of a sort key element type
func sortKeyTypeName(v any) string {
	switch v.(type) {
	case string:
		return "str"
	case int:
		return "int"
	}
	return "float"
}

// Numeric value of a number element
func keyNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}

// Python tuple ordering of two sort keys, a TypeError when a string meets a number at the deciding position
func pyTupleLess(a, b []any) (bool, error) {
	for i := 0; i < len(a) && i < len(b); i++ {
		as, aStr := a[i].(string)
		bs, bStr := b[i].(string)
		an, _ := keyNumber(a[i])
		bn, _ := keyNumber(b[i])
		switch {
		case aStr && bStr:
			if as != bs {
				return as < bs, nil
			}
		case !aStr && !bStr:
			if an != bn {
				return an < bn, nil
			}
		default:
			return false, fmt.Errorf("'<' not supported between instances of '%s' and '%s'", sortKeyTypeName(a[i]), sortKeyTypeName(b[i]))
		}
	}
	return len(a) < len(b), nil
}

// Stable sort of model ids by their sort key, the first comparison TypeError returned
func sortModelsByKey(models []string, prefix string) error {
	keys := map[string][]any{}
	for _, m := range models {
		keys[m] = modelSortKey(m, prefix)
	}
	var sortErr error
	sort.SliceStable(models, func(i, j int) bool {
		less, err := pyTupleLess(keys[models[i]], keys[models[j]])
		if err != nil && sortErr == nil {
			sortErr = err
		}
		return less
	})
	return sortErr
}

// AmbiguousAliasError reports an alias matching several catalog models, candidates best guess first
type AmbiguousAliasError struct {
	Alias      string
	Provider   string
	Candidates []string
}

func (e *AmbiguousAliasError) Error() string {
	return fmt.Sprintf("alias %s matches %d models on %s", pyStringRepr(e.Alias), len(e.Candidates), e.Provider)
}

// User facing disambiguation list of at most ten candidates
func ambiguousAliasMessage(err *AmbiguousAliasError) string {
	shown := err.Candidates
	if len(shown) > 10 {
		shown = shown[:10]
	}
	lines := make([]string, 0, len(shown))
	for i, m := range shown {
		lines = append(lines, fmt.Sprintf("  %d. %s", i+1, m))
	}
	more := ""
	if hidden := len(err.Candidates) - len(shown); hidden > 0 {
		more = fmt.Sprintf("\n  … and %d more", hidden)
	}
	return fmt.Sprintf("'%s' matches %d models on %s — not switching automatically:\n%s%s\nPick one with /model <exact-model-name>.",
		err.Alias, len(err.Candidates), err.Provider, strings.Join(lines, "\n"), more)
}

// Id a provider name routes to, custom colon name for a legacy custom provider entry
func providerIdentity(ctx context.Context, name string, userProviders map[string]any, customProviders []any) string {
	if name != "" {
		if pdef := ResolveProviderFull(ctx, name, userProviders, customProviders); pdef != nil {
			return pdef.ID
		}
	}
	return NormalizeProviderDef(ctx, name)
}

// AliasMatch is the provider, resolved model id and alias name a short alias resolved to
type AliasMatch struct {
	Provider string
	Model    string
	Alias    string
}

// Catalog listing used by alias resolution, replaced in tests
var listProviderModelsFn = func(ctx context.Context, provider string) []string {
	return ListModelsDevModels(ctx, provider, true)
}

// ResolveAlias resolves direct aliases, process provider ids and short family aliases against the provider catalog
func ResolveAlias(ctx context.Context, rawInput, currentProvider string, userProviders map[string]any, customProviders []any) (*AliasMatch, error) {
	key := strings.ToLower(pyStripSpace(rawInput))
	ensureDirectAliases(ctx)
	if direct, ok := DirectAliases.Get(key); ok {
		return &AliasMatch{Provider: direct.Provider, Model: direct.Model, Alias: key}, nil
	}
	var reverseFallback *AliasMatch
	currentID, haveCurrentID := "", false
	for _, e := range DirectAliases.Entries() {
		if strings.ToLower(e.Alias.Model) != key {
			continue
		}
		if !haveCurrentID {
			currentID, haveCurrentID = providerIdentity(ctx, currentProvider, userProviders, customProviders), true
		}
		if providerIdentity(ctx, e.Alias.Provider, userProviders, customProviders) == currentID {
			return &AliasMatch{Provider: e.Alias.Provider, Model: e.Alias.Model, Alias: e.Name}, nil
		}
		if reverseFallback == nil {
			reverseFallback = &AliasMatch{Provider: e.Alias.Provider, Model: e.Alias.Model, Alias: e.Name}
		}
	}
	if reverseFallback != nil {
		return reverseFallback, nil
	}
	processCatalog, processAliases := externalProcessCatalog(currentProvider)
	if len(processCatalog) > 0 {
		declared, ok, err := externalProcessMatch(processCatalog, processAliases, key, currentProvider)
		if err != nil {
			return nil, err
		}
		if ok {
			return &AliasMatch{Provider: currentProvider, Model: declared, Alias: key}, nil
		}
	}
	vendor, family, ok := modelAliasIdentity(key)
	if !ok {
		return nil, nil
	}
	if len(processCatalog) > 0 {
		declared, ok, err := externalProcessMatch(processCatalog, processAliases, family, currentProvider)
		if err != nil || !ok || declared == "" {
			return nil, err
		}
		return &AliasMatch{Provider: currentProvider, Model: declared, Alias: key}, nil
	}
	catalog := append([]string{}, listProviderModelsFn(ctx, currentProvider)...)
	seen := map[string]bool{}
	for _, m := range catalog {
		seen[strings.ToLower(m)] = true
	}
	for _, m := range staticCatalogFn(ctx, currentProvider) {
		if !seen[strings.ToLower(m)] {
			catalog = append(catalog, m)
		}
	}
	prefix := family
	if isAggregatorCtx(ctx, currentProvider) {
		prefix = vendor + "/" + family
	}
	var matches []string
	for _, mid := range catalog {
		if strings.HasPrefix(strings.ToLower(mid), strings.ToLower(prefix)) {
			matches = append(matches, mid)
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}
	if err := sortModelsByKey(matches, prefix); err != nil {
		return nil, err
	}
	if len(matches) > 1 {
		return nil, &AmbiguousAliasError{Alias: key, Provider: currentProvider, Candidates: matches}
	}
	return &AliasMatch{Provider: currentProvider, Model: matches[0], Alias: key}, nil
}

// Declared model ids and lowercased own aliases of an external process profile, empty for any other provider
func externalProcessCatalog(provider string) ([]string, map[string]string) {
	profile, ok := modelsProfileForFn(provider)
	if !ok || profile.AuthType != AuthTypeExternalProcess {
		return nil, map[string]string{}
	}
	aliases := map[string]string{}
	for k, v := range profile.ModelAliasMap() {
		aliases[strings.ToLower(k)] = v
	}
	return append([]string{}, profile.FallbackModels...), aliases
}

// Provider alias, exact id, else the single declared id extending the typed text, several raising ambiguity
func externalProcessMatch(catalog []string, aliases map[string]string, typed, provider string) (string, bool, error) {
	wanted := strings.ToLower(pyStripSpace(typed))
	if v, ok := aliases[wanted]; ok {
		return v, true, nil
	}
	for _, m := range catalog {
		if strings.ToLower(m) == wanted {
			return m, true, nil
		}
	}
	var matches []string
	for _, m := range catalog {
		if strings.HasPrefix(strings.ToLower(m), wanted) {
			matches = append(matches, m)
		}
	}
	if len(matches) > 1 {
		return "", false, &AmbiguousAliasError{Alias: wanted, Provider: provider, Candidates: matches}
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return "", false, nil
}

// GetAuthenticatedProviderSlugs lists providers holding credentials from cached catalogs without blocking on a fetch
func GetAuthenticatedProviderSlugs(ctx context.Context, currentProvider string, userProviders map[string]any, customProviders []any) []string {
	zero := 0
	rows, err := ListAuthenticatedProviders(ctx, AuthenticatedProvidersOptions{
		CurrentProvider: currentProvider, UserProviders: userProviders, CustomProviders: customProviders,
		MaxModels: &zero, NonBlockingCatalogs: true})
	if err != nil {
		return []string{}
	}
	slugs := make([]string, 0, len(rows))
	for _, row := range rows {
		slug, ok := row["slug"]
		if !ok {
			return []string{}
		}
		slugs = append(slugs, pyStr(slug))
	}
	return slugs
}

// First alias resolution across the authenticated providers, openrouter then nous when none are given
func resolveAliasFallback(ctx context.Context, rawInput string, authenticated []string, userProviders map[string]any, customProviders []any) (*AliasMatch, error) {
	if len(authenticated) == 0 {
		authenticated = []string{"openrouter", "nous"}
	}
	for _, p := range authenticated {
		match, err := ResolveAlias(ctx, rawInput, p, userProviders, customProviders)
		if err != nil || match != nil {
			return match, err
		}
	}
	return nil, nil
}

// DisplayContextRequest is the route and configured pin a context length display is computed for
type DisplayContextRequest struct {
	Model               string
	Provider            string
	BaseURL             string
	APIKey              string
	ModelInfo           *ModelsDevEntry
	CustomProviders     []map[string]any
	ConfigContextLength *int
	ConfiguredModel     string
	ConfiguredProvider  string
	ConfiguredBaseURL   string
}

// ResolveDisplayContextLength prefers the provider aware resolver and falls back to the catalog window
func ResolveDisplayContextLength(ctx context.Context, req DisplayContextRequest) (int, bool) {
	pin := req.ConfigContextLength
	if pin != nil && (req.ConfiguredModel != "" || req.ConfiguredProvider != "" || req.ConfiguredBaseURL != "") &&
		ShouldClearContextPin(req.ConfiguredModel, req.Model, req.ConfiguredBaseURL, req.BaseURL, req.ConfiguredProvider, req.Provider) {
		pin = nil
	}
	lengthReq := ContextLengthRequest{Model: req.Model, BaseURL: req.BaseURL, APIKey: req.APIKey, Provider: req.Provider,
		CustomProviders: req.CustomProviders}
	if pin != nil {
		lengthReq.ConfigContextLength = *pin
	}
	if n := GetModelContextLength(ctx, lengthReq); n != 0 {
		return n, true
	}
	if req.ModelInfo != nil && req.ModelInfo.ContextWindow != 0 {
		return req.ModelInfo.ContextWindow, true
	}
	return 0, false
}

// ResolveDisplayContextLengthAsync runs the display resolution off the caller goroutine, returning early on cancellation
func ResolveDisplayContextLengthAsync(ctx context.Context, req DisplayContextRequest) (int, bool, error) {
	type result struct {
		n  int
		ok bool
	}
	done := make(chan result, 1)
	go func() {
		n, ok := ResolveDisplayContextLength(ctx, req)
		done <- result{n, ok}
	}()
	select {
	case r := <-done:
		return r.n, r.ok, nil
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}
}

// ConfiguredMatch is a configured provider slug and the declared model id that matched a typed name
type ConfiguredMatch struct {
	Slug  string
	Model string
}

// Index of a slug among configured matches, -1 when absent
func configuredMatchIndex(matches []ConfiguredMatch, slug string) int {
	for i, m := range matches {
		if m.Slug == slug {
			return i
		}
	}
	return -1
}

// Configured providers whose declared models contain the typed name exactly, folding compat duplicates onto their row
func configuredProviderMatches(modelName string, userProviders map[string]any, customProviders []any) []ConfiguredMatch {
	if pyStripSpace(modelName) == "" {
		return nil
	}
	target := strings.ToLower(pyStripSpace(modelName))
	type candidate struct {
		slug string
		cfg  map[string]any
	}
	var candidates []candidate
	rows := []configuredRow{}
	rowSlugs := map[string]bool{}
	for _, slug := range sortedKeys(userProviders) {
		if cfg, ok := userProviders[slug].(map[string]any); ok {
			candidates = append(candidates, candidate{slug, cfg})
			rows = append(rows, configuredRow{slug, configuredProviderIdentity(slug, cfg)})
			rowSlugs[slug] = true
		}
	}
	all := append([]candidate{}, candidates...)
	for _, e := range customEntries(customProviders) {
		if name, ok := e["name"].(string); ok && pyStripSpace(name) != "" {
			all = append(all, candidate{"custom:" + name, e})
		}
	}
	var matches []ConfiguredMatch
	for _, c := range all {
		hit := ""
		for _, key := range []string{"models", "model", "default_model"} {
			for _, mid := range declaredModelIDs(c.cfg[key]) {
				if strings.ToLower(mid) == target {
					hit = mid
					break
				}
			}
			if hit != "" {
				break
			}
		}
		if hit == "" {
			continue
		}
		owner := ""
		if !rowSlugs[c.slug] {
			owner = duplicatesConfiguredRow(c.slug, c.cfg, rows)
		}
		if owner == "" {
			owner = c.slug
		}
		if configuredMatchIndex(matches, owner) < 0 {
			matches = append(matches, ConfiguredMatch{Slug: owner, Model: hit})
		}
	}
	return matches
}

// One providers row slug with its name, endpoint, credential and api mode identity
type configuredRow struct {
	slug     string
	identity [4]string
}

// Name, endpoint, credential and api mode of a configured provider read through the compat normalizer
func configuredProviderIdentity(slug string, cfg map[string]any) [4]string {
	stripped := map[string]any{}
	for k, v := range cfg {
		if k != "provider_key" {
			stripped[k] = v
		}
	}
	providerKey := slug
	if strings.HasPrefix(slug, "custom:") {
		providerKey = ""
	}
	entry := NormalizeCustomProviderEntry(stripped, providerKey)
	if len(entry) == 0 {
		entry = cfg
	}
	name := strings.ToLower(msClean(entry["name"]))
	if name == "" {
		name = strings.ToLower(strings.TrimPrefix(slug, "custom:"))
	}
	name = strings.ReplaceAll(name, " ", "-")
	baseURL := strings.ToLower(strings.TrimRight(msClean(pyOr(entry["base_url"], entry["url"], entry["api"])), "/"))
	apiKey, keyEnv := msClean(entry["api_key"]), msClean(pyOr(entry["key_env"], entry["api_key_env"]))
	if strings.HasPrefix(apiKey, "${") && strings.HasSuffix(apiKey, "}") && len(apiKey) >= 3 {
		apiKey, keyEnv = "", pyStripSpace(apiKey[2:len(apiKey)-1])
	}
	credential := ""
	switch {
	case apiKey != "":
		credential = "key:" + apiKey
	case keyEnv != "":
		credential = "env:" + keyEnv
	case msClean(entry["key_cmd"]) != "":
		credential = "cmd:" + msClean(entry["key_cmd"])
	}
	apiMode := msClean(pyOr(entry["api_mode"], entry["transport"]))
	if apiMode != "" {
		apiMode = strings.ToLower(CanonicalAPIMode(apiMode))
	}
	return [4]string{name, baseURL, credential, apiMode}
}

// First truthy value as Python or evaluates it, the last value when none is truthy
func pyOr(values ...any) any {
	for _, v := range values {
		if pyTruthy(v) {
			return v
		}
	}
	return values[len(values)-1]
}

// Slug of the providers row a custom_providers entry is a second view of, empty when it is its own provider
func duplicatesConfiguredRow(slug string, entry map[string]any, rows []configuredRow) string {
	identity := configuredProviderIdentity(slug, entry)
	providerKey := strings.ToLower(msClean(entry["provider_key"]))
	for _, row := range rows {
		if identity == row.identity || (providerKey == strings.ToLower(row.slug) &&
			identity[1] == row.identity[1] && identity[2] == row.identity[2]) {
			return row.slug
		}
	}
	return ""
}

// Slug among the matches the session already runs on, including the compat projection of a providers row
func currentProviderMatch(currentProvider string, userProviders map[string]any, matches []ConfiguredMatch) (string, bool) {
	if configuredMatchIndex(matches, currentProvider) >= 0 {
		return currentProvider, true
	}
	current := strings.ToLower(msClean(currentProvider))
	for _, m := range matches {
		cfg, ok := userProviders[m.Slug].(map[string]any)
		if !ok {
			continue
		}
		name := ""
		if pyTruthy(cfg["name"]) {
			name = pyStr(cfg["name"])
		}
		if CustomProviderAliases(name, m.Slug)[current] {
			return m.Slug, true
		}
	}
	return "", false
}

// Configured id behind a picker prefixed custom model selection, the input unchanged when nothing matches
func resolveNamedCustomModelID(modelName, targetProvider string, customProviders []any) string {
	provider := strings.ToLower(msClean(targetProvider))
	if !strings.HasPrefix(provider, "custom:") || !strings.Contains(modelName, "/") {
		return modelName
	}
	before, after, _ := strings.Cut(modelName, "/")
	prefix, candidate := pyStripSpace(before), pyStripSpace(after)
	if prefix == "" || candidate == "" {
		return modelName
	}
	for _, entry := range customEntries(customProviders) {
		slugs := entryAliases(entry)
		if slugs[provider] && slugs["custom:"+strings.ToLower(prefix)] {
			for _, modelID := range declaredModelIDs(entry["models"]) {
				if strings.ToLower(modelID) == strings.ToLower(candidate) {
					return modelID
				}
			}
		}
	}
	return modelName
}

// Mapping shaped entries of a custom_providers list, anything else ignored
func customEntries(customProviders any) []map[string]any {
	out := []map[string]any{}
	switch list := customProviders.(type) {
	case []any:
		for _, e := range list {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
	case []map[string]any:
		for _, m := range list {
			if m != nil {
				out = append(out, m)
			}
		}
	}
	return out
}

// Slug aliases a custom provider entry answers to
func entryAliases(entry map[string]any) map[string]bool {
	name, key := "", ""
	if pyTruthy(entry["name"]) {
		name = pyStr(entry["name"])
	}
	if pyTruthy(entry["provider_key"]) {
		key = pyStr(entry["provider_key"])
	}
	return CustomProviderAliases(name, key)
}

// Inline api_key with env template expansion, else key_env or api_key_env through the reader
func entryConfiguredKey(cfg map[string]any, readEnv func(string) string) string {
	key := msClean(cfg["api_key"])
	if strings.HasPrefix(key, "${") && strings.HasSuffix(key, "}") && len(key) >= 3 {
		key = readEnv(key[2 : len(key)-1])
	}
	if key == "" {
		if keyEnv := msClean(pyOr(cfg["key_env"], cfg["api_key_env"])); keyEnv != "" {
			key = readEnv(keyEnv)
		}
	}
	return key
}

// The providers.ollama block and its configured base URL
func ollamaConfiguredBase(ctx context.Context) (map[string]any, string) {
	cfg := getProviderConfigDict(ctx, "ollama")
	return cfg, msClean(pyOr(cfg["base_url"], cfg["api"], cfg["url"]))
}

// Unknown provider error with the left core install hint or up to three config structure issues
func unknownProviderMessage(ctx context.Context, explicitProvider string) string {
	if moved := ProviderInstallHint(ctx, explicitProvider); moved != "" {
		return fmt.Sprintf("Unknown provider '%s'. %s", explicitProvider, moved)
	}
	msg := fmt.Sprintf("Unknown provider '%s'. Check 'icebath model' for available "+
		"providers, or define it in config.yaml under 'providers:'.", explicitProvider)
	h := home.Dir(ctx)
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	var issues []config.ConfigIssue
	if err != nil {
		issues = []config.ConfigIssue{configLoadIssue(err)}
	} else {
		issues = config.ValidateConfigStructure(map[string]any(tree), tools.RemovedBackendNote)
	}
	if len(issues) > 0 {
		msg += "\n\nRun 'icebath doctor' — config issues detected:"
		for i, ci := range issues {
			if i == 3 {
				break
			}
			msg += "\n  • " + ci.Message
		}
	}
	return msg
}

// Refusal for a vendor alias that routes through an aggregator holding no credentials, empty when the hop is fine
func aggregatorAliasError(ctx context.Context, explicitProvider, targetProvider, currentProvider string, userProviders map[string]any, customProviders []any) string {
	explicitNorm := strings.ToLower(pyStripSpace(explicitProvider))
	aliasTarget := staticAliases()[explicitNorm]
	if aliasTarget == "" {
		aliasTarget = profileAliasIndex(ctx)[explicitNorm]
	}
	if !(aliasTarget != "" && aliasTarget == targetProvider && targetProvider != explicitNorm && IsAggregatorProvider(targetProvider)) {
		return ""
	}
	authed := GetAuthenticatedProviderSlugs(ctx, currentProvider, userProviders, customProviders)
	var suggestions []string
	for _, s := range authed {
		if s == targetProvider {
			return ""
		}
	}
	for _, s := range authed {
		if strings.HasPrefix(s, explicitNorm) && s != explicitNorm {
			suggestions = append(suggestions, s)
		}
	}
	hint := ""
	if len(suggestions) > 0 {
		hint = " Did you mean: " + strings.Join(suggestions, ", ") + "?"
	}
	return fmt.Sprintf("Provider '%s' is an alias that routes through %s, which has no credentials configured.%s",
		explicitNorm, Label(targetProvider), hint)
}

// Exact case insensitive catalog id, then the bare part after the vendor slash
func aggregatorCatalogMatch(newModel string, catalog []string) (string, bool) {
	wanted := strings.ToLower(newModel)
	for _, mid := range catalog {
		if strings.ToLower(mid) == wanted {
			return mid, true
		}
	}
	for _, mid := range catalog {
		if _, bare, ok := strings.Cut(mid, "/"); ok && strings.ToLower(bare) == wanted {
			return mid, true
		}
	}
	return "", false
}

// Reports whether providers or custom_providers config declares the model for the target provider or base URL
func configDeclaresModel(newModel, targetProvider, baseURL string, userProviders map[string]any, customProviders []any) bool {
	if len(userProviders) > 0 {
		if cfg, ok := userProviders[targetProvider].(map[string]any); ok && IsProviderEnabled(cfg) &&
			containsString(declaredModelIDs(cfgOrEmptyMap(cfg, "models")), newModel) {
			return true
		}
	}
	for _, entry := range customEntries(customProviders) {
		urlMatch := false
		if v, present := entry["base_url"]; present {
			urlMatch = v == any(baseURL)
		} else {
			urlMatch = baseURL == ""
		}
		if !(entryAliases(entry)[strings.ToLower(targetProvider)] || urlMatch) {
			continue
		}
		modelMatch := false
		if v, present := entry["model"]; present {
			modelMatch = v == any(newModel)
		} else {
			modelMatch = newModel == ""
		}
		if modelMatch || containsString(declaredModelIDs(cfgOrEmptyMap(entry, "models")), newModel) {
			return true
		}
	}
	return false
}

// Value under a key, an empty mapping when the key is absent
func cfgOrEmptyMap(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	return map[string]any{}
}

// SwitchRoute is the endpoint state a model switch carries, embedded by the switch pipeline state
type SwitchRoute struct {
	BaseURL               string
	APIKey                string
	NewModel              string
	TargetProvider        string
	APIMode               string
	ValidationHeaders     map[string]string
	SuppressOllamaHeaders bool
}

// Routes a direct alias to its own base URL with its declared key, a same origin session key, or a fresh host resolution
func applyDirectAliasEndpoint(ctx context.Context, st *SwitchRoute, da DirectAlias) error {
	aliasKey, err := DirectAliasAPIKey(ctx, da)
	if err != nil {
		return err
	}
	sameHost := mayReuseSessionCredential(st.BaseURL, da.BaseURL)
	switch {
	case aliasKey != "":
		st.BaseURL, st.APIKey = da.BaseURL, aliasKey
	case st.APIKey != "" && st.APIKey != "no-key-required" && sameHost:
		st.BaseURL = da.BaseURL
	default:
		resolvedBase, resolvedKey := "", ""
		if req, explicit, err := DirectAliasRuntimeRequest(ctx, da); err == nil {
			if rt, err := modelsResolver(ctx).ResolveRuntime(req, explicit, da.BaseURL, nil, st.NewModel); err == nil && rt != nil {
				resolvedBase, resolvedKey = rt.BaseURL, rt.APIKey
			}
		}
		st.BaseURL = resolvedBase
		if st.BaseURL == "" {
			st.BaseURL = da.BaseURL
		}
		if resolvedKey == "no-key-required" {
			resolvedKey = ""
		}
		switch {
		case resolvedKey != "":
			st.APIKey = resolvedKey
		case sameHost && st.APIKey != "":
		default:
			st.APIKey = "no-key-required"
		}
	}
	if aliasKey == "" && strings.ToLower(pyStripSpace(st.TargetProvider)) == "ollama" {
		ollamaCfg, ollamaBase := ollamaConfiguredBase(ctx)
		if ollamaBase != "" && sameOllamaNativeRoot(st.BaseURL, ollamaBase) {
			if key := entryConfiguredKey(ollamaCfg, func(n string) string { return pyStripSpace(os.Getenv(n)) }); key != "" {
				st.APIKey = key
			}
		} else {
			st.ValidationHeaders, st.SuppressOllamaHeaders, st.APIKey = map[string]string{}, true, "no-key-required"
		}
	}
	if st.APIKey == "" {
		st.APIKey = "no-key-required"
	}
	st.APIMode = ""
	return nil
}
