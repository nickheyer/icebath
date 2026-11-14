package model

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/catalog"
)

type DirectAlias struct {
	Name     string
	Model    string
	Provider string
	BaseURL  string
	APIKey   auth.Secret
	KeyEnv   string
}

type directAliases struct {
	order  []string
	byName map[string]DirectAlias
}

func (d *directAliases) put(a DirectAlias) {
	if _, ok := d.byName[a.Name]; !ok {
		d.order = append(d.order, a.Name)
	}
	d.byName[a.Name] = a
}

func (d *directAliases) get(name string) (DirectAlias, bool) {
	a, ok := d.byName[name]
	return a, ok
}

func scalarOf(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

func cleanText(n *yaml.Node) string { return strings.TrimSpace(scalarOf(n)) }

func mappingPairs(n *yaml.Node) [][2]*yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([][2]*yaml.Node, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1]
		for v != nil && v.Kind == yaml.AliasNode {
			v = v.Alias
		}
		out = append(out, [2]*yaml.Node{n.Content[i], v})
	}
	return out
}

func loadDirectAliases(doc *config.YAMLDocument) *directAliases {
	merged := &directAliases{byName: map[string]DirectAlias{}}
	if doc == nil || !doc.IsMapping() {
		return merged
	}
	root := doc.Node()
	if root != nil && root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	for _, kv := range mappingPairs(yamlChild(root, "model_aliases")) {
		entry := kv[1]
		if entry == nil || entry.Kind != yaml.MappingNode || scalarOf(yamlChild(entry, "model")) == "" {
			continue
		}
		provider := "custom"
		if p := yamlChild(entry, "provider"); p != nil {
			provider = scalarOf(p)
		}
		merged.put(DirectAlias{Name: strings.ToLower(strings.TrimSpace(kv[0].Value)), Model: scalarOf(yamlChild(entry, "model")),
			Provider: provider, BaseURL: scalarOf(yamlChild(entry, "base_url")),
			APIKey: auth.NewSecret(cleanText(yamlChild(entry, "api_key"))), KeyEnv: cleanText(yamlChild(entry, "key_env"))})
	}
	section := yamlChild(root, "model")
	if section == nil || section.Kind != yaml.MappingNode {
		return merged
	}
	current := scalarOf(yamlChild(section, "provider"))
	for _, kv := range mappingPairs(yamlChild(section, "aliases")) {
		key := strings.ToLower(strings.TrimSpace(kv[0].Value))
		if _, taken := merged.get(key); key == "" || taken {
			continue
		}
		value := kv[1]
		switch {
		case value != nil && value.Kind == yaml.MappingNode:
			m := cleanText(yamlChild(value, "model"))
			if m == "" {
				continue
			}
			merged.put(DirectAlias{Name: key, Model: m, Provider: firstOf(cleanText(yamlChild(value, "provider")), current, "custom"),
				BaseURL: cleanText(yamlChild(value, "base_url")), APIKey: auth.NewSecret(cleanText(yamlChild(value, "api_key"))),
				KeyEnv: cleanText(yamlChild(value, "key_env"))})
		case strings.TrimSpace(scalarOf(value)) != "":
			val := strings.TrimSpace(scalarOf(value))
			provider, model := current, val
			if p, m, ok := strings.Cut(val, "/"); ok {
				provider, model = p, m
			}
			merged.put(DirectAlias{Name: key, Model: strings.TrimSpace(model), Provider: firstOf(strings.TrimSpace(provider), current)})
		}
	}
	return merged
}

func (r *Router) directAliases(ctx context.Context) *directAliases {
	doc, err := r.c.o.Config.UserEffective(ctx, false)
	if err != nil {
		return &directAliases{byName: map[string]DirectAlias{}}
	}
	return loadDirectAliases(doc)
}

func (r *Router) DirectAliases(ctx context.Context) []DirectAlias {
	d := r.directAliases(r.bind(ctx))
	out := make([]DirectAlias, 0, len(d.order))
	for _, name := range d.order {
		out = append(out, d.byName[name])
	}
	return out
}

func (r *Router) scopedKeyEnv(ctx context.Context, name string) string {
	if name == "" {
		return ""
	}
	sec := r.c.o.Secrets
	if sec.Bound(ctx) || sec.Multiplexed(ctx) {
		v, err := sec.Get(ctx, name, "")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(v)
	}
	v, err := r.envPreferDotenv(ctx, name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

func (r *Router) envPreferDotenv(ctx context.Context, name string) (string, error) {
	file, err := r.c.o.Config.Env().File(ctx)
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(file[name])
	scoped, err := r.c.o.Secrets.Get(ctx, name, "")
	if err != nil {
		return "", err
	}
	scoped = strings.TrimSpace(scoped)
	if strings.HasPrefix(raw, "op://") && scoped != "" {
		return scoped, nil
	}
	return firstOf(raw, scoped), nil
}

func (r *Router) DirectAliasAPIKey(ctx context.Context, a DirectAlias) auth.Secret {
	ctx = r.bind(ctx)
	raw := strings.TrimSpace(a.APIKey.Reveal())
	if strings.HasPrefix(raw, "${") && strings.HasSuffix(raw, "}") {
		return auth.NewSecret(r.scopedKeyEnv(ctx, strings.TrimSpace(raw[2:len(raw)-1])))
	}
	if raw != "" {
		return auth.NewSecret(raw)
	}
	return auth.NewSecret(r.scopedKeyEnv(ctx, strings.TrimSpace(a.KeyEnv)))
}

func (r *Router) DirectAliasRuntimeRequest(ctx context.Context, a DirectAlias) (string, auth.Secret) {
	requested := firstOf(a.Provider, "custom")
	if a.BaseURL != "" {
		requested = "custom"
	}
	return requested, r.DirectAliasAPIKey(ctx, a)
}

func mayReuseSessionCredential(sessionBase, aliasBase string) bool {
	session, alias := baseURLOrigin(sessionBase), baseURLOrigin(aliasBase)
	if session.host == "" || session != alias {
		return false
	}
	switch alias.host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	return alias.scheme == "https"
}

type AmbiguousAliasError struct {
	Alias      string
	Provider   string
	Candidates []string
}

func (e *AmbiguousAliasError) Error() string {
	return fmt.Sprintf("alias '%s' matches %d models on %s", e.Alias, len(e.Candidates), e.Provider)
}

func (e *AmbiguousAliasError) Message() string {
	shown := e.Candidates
	if len(shown) > 10 {
		shown = shown[:10]
	}
	lines := make([]string, len(shown))
	for i, m := range shown {
		lines[i] = fmt.Sprintf("  %d. %s", i+1, m)
	}
	more := ""
	if hidden := len(e.Candidates) - len(shown); hidden > 0 {
		more = fmt.Sprintf("\n  … and %d more", hidden)
	}
	return fmt.Sprintf("'%s' matches %d models on %s — not switching automatically:\n%s%s\nPick one with /model <exact-model-name>.",
		e.Alias, len(e.Candidates), e.Provider, strings.Join(lines, "\n"), more)
}

type sortElem struct {
	num   float64
	text  string
	isNum bool
}

func splitVersionSuffix(rest string) ([]float64, string) {
	var nums []float64
	run, pos := "", 0
	flush := func() {
		if f, err := strconv.ParseFloat(strings.TrimRight(run, "."), 64); err == nil {
			nums = append(nums, f)
		}
		run = ""
	}
	digit := func(c byte) bool { return c >= '0' && c <= '9' }
	for pos < len(rest) {
		ch := rest[pos]
		if strings.IndexByte("-_.", ch) >= 0 {
			pos++
			continue
		}
		if !(ch == 'v' || ch == 'V' || digit(ch)) {
			break
		}
		if ch == 'v' || ch == 'V' {
			pos++
		}
		for pos < len(rest) && (digit(rest[pos]) || rest[pos] == '.') {
			if rest[pos] == '.' && strings.Contains(run, ".") {
				flush()
			} else {
				run += string(rest[pos])
			}
			pos++
		}
		flush()
		if pos < len(rest) && rest[pos] != '-' && rest[pos] != '_' {
			break
		}
	}
	return nums, rest[pos:]
}

func modelSortKey(modelID, prefix string) []sortElem {
	rest := modelID
	if len(prefix) <= len(rest) {
		rest = rest[len(prefix):]
	}
	rest = strings.TrimSpace(strings.TrimLeft(strings.TrimPrefix(rest, "/"), "-"))
	nums, suffixBuf := splitVersionSuffix(rest)
	suffix := strings.TrimSpace(strings.Trim(strings.ToLower(suffixBuf), "-_."))
	var key []sortElem
	date := 0.0
	for _, n := range nums {
		if n < 19_000_101 {
			key = append(key, sortElem{num: -n, isNum: true})
		} else if n > date {
			date = n
		}
	}
	rank := 1.0
	switch suffix {
	case "pro", "max", "plus", "turbo", "sol", "astra":
		rank = 0
	}
	key = append(key, sortElem{num: rank, isNum: true}, sortElem{text: suffix})
	if date == 0 {
		key = append(key, sortElem{isNum: true}, sortElem{isNum: true})
	} else {
		key = append(key, sortElem{num: 1, isNum: true}, sortElem{num: -date, isNum: true})
	}
	return key
}

func compareSortKeys(a, b []sortElem) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		switch {
		case x.isNum && y.isNum:
			if x.num != y.num {
				if x.num < y.num {
					return -1
				}
				return 1
			}
		case !x.isNum && !y.isNum:
			if c := strings.Compare(x.text, y.text); c != 0 {
				return c
			}
		case x.isNum:
			return -1
		default:
			return 1
		}
	}
	return len(a) - len(b)
}

func (r *Router) providerIdentity(ctx context.Context, name string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) string {
	if name != "" {
		if def, ok := r.c.o.Catalog.ResolveProviderFull(ctx, name, user, custom); ok {
			return def.ID
		}
	}
	return r.c.o.Providers.NormalizeProvider(ctx, name)
}

func (r *Router) externalProcessCatalog(ctx context.Context, provider string) ([]string, map[string]string) {
	p, ok := r.c.o.Providers.Get(ctx, provider)
	if !ok || p.Spec().AuthType != "external_process" {
		return nil, nil
	}
	spec := p.Spec()
	aliases := map[string]string{}
	for k, v := range spec.ModelAliases {
		aliases[strings.ToLower(k)] = v
	}
	return slices.Clone(spec.FallbackModels), aliases
}

func externalProcessMatch(models []string, aliases map[string]string, typed, provider string) (string, error) {
	wanted := strings.ToLower(strings.TrimSpace(typed))
	if v, ok := aliases[wanted]; ok {
		return v, nil
	}
	for _, m := range models {
		if strings.ToLower(m) == wanted {
			return m, nil
		}
	}
	var matches []string
	for _, m := range models {
		if strings.HasPrefix(strings.ToLower(m), wanted) {
			matches = append(matches, m)
		}
	}
	if len(matches) > 1 {
		return "", &AmbiguousAliasError{Alias: wanted, Provider: provider, Candidates: matches}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return "", nil
}

type aliasMatch struct {
	provider string
	model    string
	alias    string
}

func (r *Router) ResolveAlias(ctx context.Context, raw, currentProvider string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) (provider, model, alias string, ok bool, err error) {
	m, ok, err := r.resolveAlias(r.bind(ctx), raw, currentProvider, user, custom)
	return m.provider, m.model, m.alias, ok, err
}

func (r *Router) resolveAlias(ctx context.Context, raw, current string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) (aliasMatch, bool, error) {
	key := strings.ToLower(strings.TrimSpace(raw))
	aliases := r.directAliases(ctx)
	if da, ok := aliases.get(key); ok {
		return aliasMatch{da.Provider, da.Model, key}, true, nil
	}
	var reverse *aliasMatch
	currentID := r.providerIdentity(ctx, current, user, custom)
	for _, name := range aliases.order {
		da := aliases.byName[name]
		if strings.ToLower(da.Model) != key {
			continue
		}
		if r.providerIdentity(ctx, da.Provider, user, custom) == currentID {
			return aliasMatch{da.Provider, da.Model, name}, true, nil
		}
		if reverse == nil {
			reverse = &aliasMatch{da.Provider, da.Model, name}
		}
	}
	if reverse != nil {
		return *reverse, true, nil
	}
	processModels, processAliases := r.externalProcessCatalog(ctx, current)
	if len(processModels) > 0 {
		declared, err := externalProcessMatch(processModels, processAliases, key, current)
		if err != nil {
			return aliasMatch{}, false, err
		}
		if declared != "" {
			return aliasMatch{current, declared, key}, true, nil
		}
	}
	identity, ok := catalog.ModelAlias(key)
	if !ok {
		return aliasMatch{}, false, nil
	}
	if len(processModels) > 0 {
		declared, err := externalProcessMatch(processModels, processAliases, identity.Family, current)
		if err != nil || declared == "" {
			return aliasMatch{}, false, err
		}
		return aliasMatch{current, declared, key}, true, nil
	}
	cat := r.c.o.Catalog
	models := cat.ListProviderModels(ctx, current, true)
	seen := map[string]bool{}
	for _, m := range models {
		seen[strings.ToLower(m)] = true
	}
	for _, m := range cat.StaticCatalog(ctx, current) {
		if !seen[strings.ToLower(m)] {
			seen[strings.ToLower(m)] = true
			models = append(models, m)
		}
	}
	prefix := identity.Family
	if cat.IsAggregator(ctx, current) {
		prefix = identity.Vendor + "/" + identity.Family
	}
	var matches []string
	for _, m := range models {
		if strings.HasPrefix(strings.ToLower(m), strings.ToLower(prefix)) {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		return aliasMatch{}, false, nil
	}
	slices.SortStableFunc(matches, func(a, b string) int {
		return compareSortKeys(modelSortKey(a, prefix), modelSortKey(b, prefix))
	})
	if len(matches) > 1 {
		return aliasMatch{}, false, &AmbiguousAliasError{Alias: key, Provider: current, Candidates: matches}
	}
	return aliasMatch{current, matches[0], key}, true, nil
}

func (r *Router) resolveAliasFallback(ctx context.Context, raw string, authed []string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) (aliasMatch, bool, error) {
	if len(authed) == 0 {
		authed = []string{"openrouter", "portal"}
	}
	for _, p := range authed {
		m, ok, err := r.resolveAlias(ctx, raw, p, user, custom)
		if err != nil {
			return aliasMatch{}, false, err
		}
		if ok {
			return m, true, nil
		}
	}
	return aliasMatch{}, false, nil
}

func (r *Router) AuthenticatedProviderSlugs(ctx context.Context, current string, user config.Map[config.ProviderEntry], custom []config.CustomEndpoint) []string {
	ctx = r.bind(ctx)
	zero := 0
	rows, err := r.c.o.Catalog.AuthenticatedProviders(ctx, catalog.ProvidersQuery{
		PickerContext: catalog.PickerContext{CurrentProvider: current, UserProviders: user, CustomProviders: custom},
		MaxModels:     &zero, NonBlockingCatalogs: true})
	if err != nil {
		return nil
	}
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Slug
	}
	return out
}
