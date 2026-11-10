package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// ProviderSelectionError is the KeyError of a provider selection naming an unknown toolset or provider
type ProviderSelectionError struct{ Message string }

func (e *ProviderSelectionError) Error() string { return pyrepr.StrRepr(e.Message) }

func rowGet(p *pyjson.Object, key string) any {
	v, _ := p.Get(key)
	return v
}

func rowGetOr(p *pyjson.Object, key string, def any) any {
	if v, ok := p.Get(key); ok {
		return v
	}
	return def
}

func rowHas(p *pyjson.Object, key string) bool {
	_, ok := p.Get(key)
	return ok
}

// Text a row value formats as inside an f-string
func rowText(p *pyjson.Object, key string) string { return pyrepr.Str(rowGet(p, key)) }

// Python str of a raised exception, empty for the bare Ctrl-C and EOF interrupts
func pyExcText(err error) string {
	if errors.Is(err, cliui.ErrInterrupt) || errors.Is(err, cliui.ErrEOF) {
		return ""
	}
	return pyrepr.ExceptionMessage(err)
}

// Managed Nous feature a row names, empty for an unmanaged row
func managedFeatureOf(p *pyjson.Object) string { return pyOrStr(rowGet(p, "managed_nous_feature")) }

// Rows of a category, the AttributeError of a provider entry that is not a dict raised as a panic
func catProviders(cat *pyjson.Object) []*pyjson.Object {
	raw, _ := cat.GetOr("providers", []any{}).([]any)
	out := make([]*pyjson.Object, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(*pyjson.Object)
		if !ok {
			panic(fmt.Sprintf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(item)))
		}
		out = append(out, row)
	}
	return out
}

// Category of the catalog by toolset key, nil when absent
func catalogCategory(tsKey string) *pyjson.Object {
	v, _ := ToolCategories.Get(tsKey)
	cat, _ := v.(*pyjson.Object)
	return cat
}

// List membership by equality the way provider in cat["providers"] compares dicts
func providerIn(provider *pyjson.Object, rows []*pyjson.Object) bool {
	for _, r := range rows {
		if pyjson.Equal(provider, r) {
			return true
		}
	}
	return false
}

// Set membership of a value, the TypeError hermes raises when the value is unhashable
func pySetContains(v any, members ...any) (bool, error) {
	switch v.(type) {
	case []any, []string, map[string]any, config.Tree, *pyjson.Object:
		return false, fmt.Errorf("TypeError: unhashable type: '%s'", pyrepr.TypeName(v))
	}
	for _, m := range members {
		if pyjson.Equal(v, m) {
			return true, nil
		}
	}
	return false, nil
}

// Config section as a dict when it is one, the empty default dict when the key is absent
func cfgDictOr(cfg map[string]any, key string) (map[string]any, bool) {
	v, ok := cfg[key]
	if !ok {
		return map[string]any{}, true
	}
	return pyDictOf(v)
}

// Python setdefault of a dict section, the error of using a non dict existing value the way op does
func setdefaultSection(cfg map[string]any, key, op string) (map[string]any, error) {
	v, ok := cfg[key]
	if !ok {
		m := map[string]any{}
		cfg[key] = m
		return m, nil
	}
	if m, ok := pyDictOf(v); ok {
		return m, nil
	}
	if op == "pop" {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'pop'", pyrepr.TypeName(v))
	}
	return nil, fmt.Errorf("TypeError: '%s' object does not support item assignment", pyrepr.TypeName(v))
}

// Value with plugin schema dicts turned into ordered rows, the key prompt url order first then the rest sorted
func pickerValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := pyjson.NewObject()
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		rank := map[string]int{"key": 0, "prompt": 1, "url": 2, "default": 3}
		slices.SortFunc(keys, func(a, b string) int {
			ra, oka := rank[a]
			rb, okb := rank[b]
			switch {
			case oka && okb:
				return ra - rb
			case oka:
				return -1
			case okb:
				return 1
			}
			return strings.Compare(a, b)
		})
		for _, k := range keys {
			o.Set(k, pickerValue(x[k]))
		}
		return o
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = pickerValue(m)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = pickerValue(item)
		}
		return out
	}
	return v
}

// Dict entries of a schema variants value as iteration sees them, a TypeError panic for a non iterable
func pyIterDicts(v any) []map[string]any {
	if !pyTruthy(v) {
		return nil
	}
	var out []map[string]any
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			switch m := item.(type) {
			case map[string]any:
				out = append(out, m)
			case *pyjson.Object:
				out = append(out, m.Vals)
			}
		}
	case []map[string]any:
		for _, m := range x {
			if m != nil {
				out = append(out, m)
			}
		}
	case string, map[string]any:
	default:
		panic(fmt.Sprintf("TypeError: '%s' object is not iterable", pyrepr.TypeName(v)))
	}
	return out
}

// Web providers as picker providers, the setup schema defaulting like ProviderBase get_setup_schema
type webPickerProvider struct {
	WebProvider
	env *Env
}

func (p webPickerProvider) GetSetupSchema() map[string]any {
	if sp, ok := p.WebProvider.(interface{ SetupSchema() map[string]any }); ok {
		return sp.SetupSchema()
	}
	return map[string]any{"name": p.DisplayName(), "badge": "", "tag": "", "env_vars": []any{}}
}

func (p webPickerProvider) IsAvailable() bool { return p.Available(p.env) }

// Picker view of the web search registry this package owns
var webPickerRegistry = &PickerRegistry{
	List: func(_ context.Context, env *Env) []PickerProvider {
		var out []PickerProvider
		for _, p := range ListWebProviders(env) {
			out = append(out, webPickerProvider{p, env})
		}
		return out
	},
	Get: func(_ context.Context, env *Env, name string) PickerProvider {
		if p := GetWebProvider(env, name); p != nil {
			return webPickerProvider{p, env}
		}
		return nil
	},
}

// Installed registry of a hermes registry module, nil when no package provides it
func pickerRegistryFor(module string) *PickerRegistry {
	switch module {
	case "agent.image_gen_registry":
		return ToolsConfigHooks.ImageGenRegistry
	case "agent.video_gen_registry":
		return ToolsConfigHooks.VideoGenRegistry
	case "agent.web_search_registry":
		return webPickerRegistry
	case "agent.browser_registry":
		return ToolsConfigHooks.BrowserRegistry
	case "agent.tts_registry":
		return ToolsConfigHooks.TTSRegistry
	}
	return nil
}

// Plugin registry of a module after plugin discovery, nil on any failure
func (s *ToolsSetup) pluginRegistry(module string) (reg *PickerRegistry) {
	defer func() {
		if recover() != nil {
			reg = nil
		}
	}()
	reg = pickerRegistryFor(module)
	if reg == nil || EnsurePluginsDiscovered == nil {
		return nil
	}
	if err := EnsurePluginsDiscovered(s.ctx()); err != nil {
		return nil
	}
	return reg
}

// Setup schema of a provider, false when the call fails
func pickerSchema(p PickerProvider) (schema map[string]any, ok bool) {
	defer func() {
		if recover() != nil {
			schema, ok = nil, false
		}
	}()
	return p.GetSetupSchema(), true
}

// Picker rows of a plugin registry, every marker key set to the provider name so config writes route through it
func (s *ToolsSetup) pluginProviderRows(module string, markerKeys []string, requireName, skipBuiltin, flattenVariants bool) []*pyjson.Object {
	reg := s.pluginRegistry(module)
	if reg == nil {
		return []*pyjson.Object{}
	}
	providers, builtin, ok := func() (ps []PickerProvider, b map[string]bool, ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		ps = reg.List(s.ctx(), s.toolEnv())
		if skipBuiltin {
			if reg.BuiltinNames == nil {
				return nil, nil, false
			}
			b = reg.BuiltinNames
		}
		return ps, b, true
	}()
	if !ok {
		return []*pyjson.Object{}
	}
	rows := []*pyjson.Object{}
	for _, provider := range providers {
		var name string
		if requireName {
			name = provider.Name()
			if name == "" || (skipBuiltin && builtin[pystr.Strip(pystr.Lower(name))]) {
				continue
			}
		}
		schema, ok := pickerSchema(provider)
		if !ok || schema == nil {
			continue
		}
		if !requireName {
			name = provider.Name()
		}
		entries := []map[string]any{schema}
		if flattenVariants {
			entries = append(entries, pyIterDicts(schema["variants"])...)
		}
		for _, entry := range entries {
			row := pyjson.NewObject()
			display := provider.DisplayName()
			row.Set("name", mapGetOr(entry, "name", display))
			row.Set("badge", mapGetOr(entry, "badge", ""))
			row.Set("tag", mapGetOr(entry, "tag", ""))
			row.Set("env_vars", pickerValue(mapGetOr(entry, "env_vars", []any{})))
			for _, key := range markerKeys {
				row.Set(key, name)
			}
			if flattenVariants && pyTruthy(entry["web_tier"]) {
				row.Set("web_tier", entry["web_tier"])
			}
			if pyTruthy(entry["post_setup"]) {
				row.Set("post_setup", entry["post_setup"])
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func mapGetOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Registry module, marker keys and row options of one plugin backed category
type pluginRowSpec struct {
	module                                    string
	markers                                   []string
	requireName, skipBuiltin, flattenVariants bool
}

var pluginProviderRowSpecs = map[string]pluginRowSpec{
	"image_gen": {module: "agent.image_gen_registry", markers: []string{"image_gen_plugin_name"}},
	"video_gen": {module: "agent.video_gen_registry", markers: []string{"video_gen_plugin_name"}},
	"web":       {module: "agent.web_search_registry", markers: []string{"web_backend", "web_search_plugin_name"}, requireName: true, flattenVariants: true},
	"browser":   {module: "agent.browser_registry", markers: []string{"browser_provider", "browser_plugin_name"}, requireName: true},
	"tts":       {module: "agent.tts_registry", markers: []string{"tts_provider", "tts_plugin_name"}, requireName: true, skipBuiltin: true},
}

// Picker rows of the plugin registered providers of one category
func (s *ToolsSetup) pluginRowsFor(category string) []*pyjson.Object {
	spec, ok := pluginProviderRowSpecs[category]
	if !ok {
		panic("KeyError: " + pyrepr.StrRepr(category))
	}
	return s.pluginProviderRows(spec.module, spec.markers, spec.requireName, spec.skipBuiltin, spec.flattenVariants)
}

func (s *ToolsSetup) pluginImageGenProviders() []*pyjson.Object  { return s.pluginRowsFor("image_gen") }
func (s *ToolsSetup) pluginVideoGenProviders() []*pyjson.Object  { return s.pluginRowsFor("video_gen") }
func (s *ToolsSetup) pluginWebSearchProviders() []*pyjson.Object { return s.pluginRowsFor("web") }
func (s *ToolsSetup) pluginBrowserProviders() []*pyjson.Object   { return s.pluginRowsFor("browser") }
func (s *ToolsSetup) pluginTTSProviders() []*pyjson.Object       { return s.pluginRowsFor("tts") }
func pluginRowsBuilder(fn func(*ToolsSetup) []*pyjson.Object) func(*ToolsSetup) ([]*pyjson.Object, error) {
	return func(s *ToolsSetup) ([]*pyjson.Object, error) { return fn(s), nil }
}

// Capabilities a web backend supports per its registry provider, both when unknown or the lookup fails
func (s *ToolsSetup) WebProviderCapabilities(backend string) (caps []string) {
	defer func() {
		if recover() != nil {
			caps = []string{"search", "extract"}
		}
	}()
	if p := GetWebProvider(s.toolEnv(), backend); p != nil {
		caps = []string{}
		if p.SupportsSearch() {
			caps = append(caps, "search")
		}
		if p.SupportsExtract() {
			caps = append(caps, "extract")
		}
		return caps
	}
	return []string{"search", "extract"}
}

// Plugin row builders keyed by the category display name
var pluginRowBuilders = map[string]func(*ToolsSetup) ([]*pyjson.Object, error){
	"Image Generation":                   pluginRowsBuilder((*ToolsSetup).pluginImageGenProviders),
	"Video Generation":                   pluginRowsBuilder((*ToolsSetup).pluginVideoGenProviders),
	"Web Search & Extract":               pluginRowsBuilder((*ToolsSetup).pluginWebSearchProviders),
	"Browser Automation":                 pluginRowsBuilder((*ToolsSetup).pluginBrowserProviders),
	"Text-to-Speech":                     pluginRowsBuilder((*ToolsSetup).pluginTTSProviders),
	"Computer Use (macOS/Windows/Linux)": (*ToolsSetup).computerUseProviderRows,
}

// Rows for installed computer use providers other than the built in cua row, read without loading any
func (s *ToolsSetup) computerUseProviderRows() ([]*pyjson.Object, error) {
	discover, def := ToolsConfigHooks.ComputerUseProviders, ToolsConfigHooks.ComputerUseDefaultBackend
	requireToolsConfigHook("ComputerUseProviders", discover != nil)
	requireToolsConfigHook("ComputerUseDefaultBackend", def != nil)
	pairs, err := discover(s.toolEnv())
	if err != nil {
		return nil, err
	}
	rows := []*pyjson.Object{}
	for _, pair := range pairs {
		if pair[0] == def() {
			continue
		}
		row := pyjson.NewObject()
		row.Set("name", pair[0])
		row.Set("badge", "")
		row.Set("tag", pair[1])
		row.Set("env_vars", []any{})
		row.Set("computer_use_backend", pair[0])
		rows = append(rows, row)
	}
	return rows, nil
}

func isTrue(b *bool) bool { return b != nil && *b }

// Provider rows visible for the auth and config state, managed rows always shown and plugin rows after the catalog rows
func (s *ToolsSetup) VisibleProviders(cat *pyjson.Object, cfg config.Tree, forceFresh bool, features *NousToolFeatures) ([]*pyjson.Object, error) {
	if features == nil {
		f := s.nousFeatures(cfg, forceFresh)
		features = &f
	}
	acct := features.Account
	poolOnly := acct != nil && acct.LoggedIn && !isTrue(acct.PaidServiceAccess) && acct.ToolGatewayEntitled
	visible := []*pyjson.Object{}
	for _, provider := range catProviders(cat) {
		managed := rowGet(provider, "managed_nous_feature")
		if pyTruthy(rowGet(provider, "requires_nous_auth")) && !pyTruthy(managed) && !features.NousAuthPresent {
			continue
		}
		if poolOnly && pyjson.Equal(managed, "video_gen") && !(acct != nil && acct.EntitledFor("fal-video")) {
			continue
		}
		visible = append(visible, provider)
	}
	if name, ok := rowGet(cat, "name").(string); ok {
		if builder, ok := pluginRowBuilders[name]; ok {
			rows, err := builder(s)
			if err != nil {
				return nil, err
			}
			visible = append(visible, rows...)
		}
	}
	return visible, nil
}

// Readiness of a picker row, ready, needs_keys, needs_auth or needs_setup
func (s *ToolsSetup) ProviderReadinessStatus(provider *pyjson.Object, cfg config.Tree, features *NousToolFeatures, isActive *bool) (string, error) {
	if pyTruthy(rowGetOr(provider, "env_vars", []any{})) {
		ready, err := providerEnvReady(s.ctx(), s.home(), provider)
		if err != nil {
			return "", err
		}
		if ready {
			return "ready", nil
		}
		return "needs_keys", nil
	}
	managedFeature := rowGet(provider, "managed_nous_feature")
	if pyTruthy(rowGet(provider, "requires_nous_auth")) || pyTruthy(managedFeature) {
		if features == nil {
			f := s.nousFeatures(cfg, false)
			features = &f
		}
		if !features.NousAuthPresent {
			return "needs_auth", nil
		}
		if pyTruthy(managedFeature) {
			acct := features.Account
			category := managedCoverageCategory(pyrepr.Str(managedFeature))
			entitled := false
			if acct != nil && acct.LoggedIn {
				if category != "" {
					entitled = acct.EntitledFor(category)
				} else {
					entitled = acct.ToolGatewayEntitled
				}
			}
			if !entitled {
				return "needs_auth", nil
			}
		}
	}
	if postSetup := rowGet(provider, "post_setup"); pyTruthy(postSetup) {
		key := pyrepr.Str(postSetup)
		if authReady, ok := postSetupAuthReady[key]; ok {
			if authReady(s) {
				return "ready", nil
			}
			return "needs_auth", nil
		}
		if predicate, ok := postSetupReady[key]; ok {
			if ready, failed := s.readyPredicate(predicate); failed || ready {
				return "ready", nil
			}
			return "needs_setup", nil
		}
		if isActive == nil {
			active, err := s.IsProviderActive(provider, cfg, false)
			if err != nil {
				return "", err
			}
			isActive = &active
		}
		if *isActive {
			return "ready", nil
		}
		return "needs_setup", nil
	}
	return s.webRegistryReadiness(provider), nil
}

// Result of an installed check, failed when it errors so flaky detection never reads as a warning state
func (s *ToolsSetup) readyPredicate(predicate func(*ToolsSetup) (bool, error)) (ready, failed bool) {
	defer func() {
		if recover() != nil {
			ready, failed = false, true
		}
	}()
	ok, err := predicate(s)
	if err != nil {
		return false, true
	}
	return ok, false
}

// Coverage category gating a managed feature, empty when the feature has none
func managedCoverageCategory(feature string) string {
	fn := ToolsConfigHooks.ManagedCoverageCategory
	requireToolsConfigHook("ManagedCoverageCategory", fn != nil)
	category, _ := fn(feature)
	return category
}

// Keyless untiered web rows read ready only when their registry provider is available
func (s *ToolsSetup) webRegistryReadiness(provider *pyjson.Object) string {
	backend := rowGet(provider, "web_backend")
	if !pyTruthy(backend) || pyTruthy(rowGet(provider, "web_tier")) {
		return "ready"
	}
	env := s.toolEnv()
	registered := GetWebProvider(env, pyrepr.Str(backend))
	if registered == nil || registered.Available(env) {
		return "ready"
	}
	return "needs_auth"
}

// Whether enabling a toolset should open provider setup
func (s *ToolsSetup) toolsetNeedsConfigurationPrompt(tsKey string, cfg config.Tree, forceFresh bool) (bool, error) {
	cat := catalogCategory(tsKey)
	if cat == nil || cat.Len() == 0 {
		has, err := s.toolsetHasKeys(tsKey, cfg, forceFresh, nil)
		return !has, err
	}
	providers, err := s.VisibleProviders(cat, cfg, forceFresh, nil)
	if err != nil {
		return false, err
	}
	for _, provider := range providers {
		if postSetup := rowGet(provider, "post_setup"); pyTruthy(postSetup) && !s.PostSetupAlreadyInstalled(pyrepr.Str(postSetup)) {
			return true, nil
		}
	}
	if selectionKey, ok := map[string]string{"tts": "provider", "web": "backend", "browser": "cloud_provider"}[tsKey]; ok {
		section, ok := cfgDictOr(cfg, tsKey)
		if !ok {
			return true, nil
		}
		_, has := section[selectionKey]
		return !has, nil
	}
	if tsKey == "image_gen" {
		return !FalKeyIsConfigured(s.toolEnv()) && !s.anyPluginProviderAvailable("agent.image_gen_registry"), nil
	}
	if tsKey == "video_gen" {
		return !s.anyPluginProviderAvailable("agent.video_gen_registry"), nil
	}
	has, err := s.toolsetHasKeys(tsKey, cfg, forceFresh, nil)
	return !has, err
}

// Whether any provider of a plugin registry reports itself available
func (s *ToolsSetup) anyPluginProviderAvailable(module string) (found bool) {
	reg := s.pluginRegistry(module)
	defer func() {
		if recover() != nil {
			found = false
		}
	}()
	if reg == nil {
		return false
	}
	for _, p := range reg.List(s.ctx(), s.toolEnv()) {
		if pickerAvailable(p) {
			return true
		}
	}
	return false
}

// Availability of one provider, false when it has no check or the check fails
func pickerAvailable(p PickerProvider) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	a, has := p.(interface{ IsAvailable() bool })
	return has && a.IsAvailable()
}

// Provider selection for a tool category, then key setup for the chosen row
func (s *ToolsSetup) configureToolCategory(tsKey string, cat *pyjson.Object, cfg config.Tree, forceFresh, reconfigure bool) error {
	name, ok := cat.Get("name")
	if !ok {
		return fmt.Errorf("KeyError: 'name'")
	}
	providers, err := s.VisibleProviders(cat, cfg, forceFresh, nil)
	if err != nil {
		return err
	}
	single := len(providers) == 1
	title := "Choose a provider"
	if !reconfigure {
		title = pyrepr.Str(cat.GetOr("setup_title", "Choose a provider"))
	}
	s.println("")
	heading := "- " + title
	if single {
		heading = "(" + rowText(providers[0], "name") + ")"
	}
	s.println(s.color("  --- "+pyrepr.Str(cat.GetOr("icon", ""))+" "+pyrepr.Str(name)+" "+heading+" ---", cliui.Cyan))
	if single && !reconfigure && pyTruthy(rowGet(providers[0], "tag")) {
		s.Con.PrintInfo("  " + rowText(providers[0], "tag"))
	}
	if !reconfigure && pyTruthy(rowGet(cat, "setup_note")) {
		s.Con.PrintInfo("  " + rowText(cat, "setup_note"))
	}
	if single {
		return s.configureProvider(providers[0], cfg, forceFresh, reconfigure)
	}
	s.println("")
	nousLoggedIn := false
	if !reconfigure {
		func() {
			defer func() { _ = recover() }()
			nousLoggedIn = s.nousFeatures(cfg, forceFresh).NousAuthPresent
		}()
	}
	choices := []string{}
	for _, p := range providers {
		badge, tag, configured, subMarker := "", "", "", ""
		if pyTruthy(rowGet(p, "badge")) {
			badge = " [" + rowText(p, "badge") + "]"
		}
		if pyTruthy(rowGet(p, "tag")) {
			tag = " — " + rowText(p, "tag")
		}
		ready, err := providerEnvReady(s.ctx(), s.home(), p)
		if err != nil {
			return err
		}
		if ready {
			active, err := s.IsProviderActive(p, cfg, forceFresh)
			if err != nil {
				return err
			}
			if active {
				configured = " [active]"
			} else if pyTruthy(rowGetOr(p, "env_vars", []any{})) {
				configured = " [configured]"
			}
		}
		if !reconfigure && pyTruthy(rowGet(p, "managed_nous_feature")) {
			subMarker = "  ★ via Nous Portal (login on select)"
			if nousLoggedIn {
				subMarker = "  ★ Included with your Nous subscription"
			}
		}
		choices = append(choices, rowText(p, "name")+badge+tag+configured+subMarker)
	}
	if !reconfigure {
		choices = append(choices, "Skip — keep defaults / configure later")
	}
	defaultIdx, err := s.detectActiveProviderIndex(providers, cfg, forceFresh)
	if err != nil {
		return err
	}
	question := "  " + title + ":"
	if reconfigure {
		question = "  Select provider:"
	}
	idx, err := s.promptChoice(question, choices, defaultIdx)
	if err != nil {
		return err
	}
	if idx >= len(providers) {
		s.Con.PrintInfo("  Skipped " + pyrepr.Str(name))
		return nil
	}
	return s.configureProvider(providers[idx], cfg, forceFresh, reconfigure)
}

// Provider env lookup of the tier auto detection, swapped by tests like hermes patches get_provider_env
var webTierProviderEnv = providerEnv

// Whether a tiered web row matches the configured tier, auto meaning paid exactly when the vendor key is present
func (s *ToolsSetup) webTierMatches(provider *pyjson.Object, cfg config.Tree) bool {
	rowTier := rowGet(provider, "web_tier")
	if !pyTruthy(rowTier) {
		return true
	}
	webCfg, ok := pyDictOf(cfg["web"])
	if !ok {
		webCfg = map[string]any{}
	}
	tiers, ok := pyDictOf(webCfg["provider_tier"])
	if !ok {
		tiers = map[string]any{}
	}
	backendValue, has := provider.Get("web_backend")
	if !has {
		panic("KeyError: 'web_backend'")
	}
	backend := pyrepr.Str(backendValue)
	configured := pystr.Strip(pystr.Lower(pyOrStr(mapGetOr(tiers, backend, ""))))
	if configured == "free" || configured == "paid" {
		return pyjson.Equal(configured, rowTier)
	}
	hasKey := func() (has bool) {
		defer func() {
			if recover() != nil {
				has = false
			}
		}()
		keyVar := map[string]string{"exa": "EXA_API_KEY", "parallel": "PARALLEL_API_KEY"}[backend]
		if keyVar == "" {
			return false
		}
		v, err := webTierProviderEnv(s.ctx(), s.toolEnv(), keyVar)
		return err == nil && v != ""
	}()
	want := "free"
	if hasKey {
		want = "paid"
	}
	return pyjson.Equal(rowTier, want)
}

// Backend names serving web search or extract, each capability override first then the shared backend
func webServingBackends(cfg config.Tree) map[string]bool {
	webCfg, ok := pyDictOf(cfg["web"])
	if !ok {
		webCfg = map[string]any{}
	}
	name := func(key string) string {
		if v, ok := webCfg[key].(string); ok {
			return pystr.Strip(pystr.Lower(v))
		}
		return ""
	}
	shared := NousManagedProvider
	if !truthyValue(webCfg["use_gateway"], false) {
		shared = name("backend")
	}
	serving := map[string]bool{}
	for _, key := range []string{"search_backend", "extract_backend"} {
		v := name(key)
		if v == "" {
			v = shared
		}
		serving[v] = true
	}
	delete(serving, "")
	return serving
}

// Whether a web row's backend serves a capability as configured, sibling rows sharing a backend split by their credential
func (s *ToolsSetup) webBackendActive(provider *pyjson.Object, cfg config.Tree) (bool, error) {
	backend := rowGet(provider, "web_backend")
	if !(pyTruthy(backend) && webServingBackends(cfg)[pyrepr.Str(backend)] && s.webTierMatches(provider, cfg)) {
		return false, nil
	}
	if !pyTruthy(rowGet(provider, "env_vars")) {
		return true, nil
	}
	ready, err := providerEnvReady(s.ctx(), s.home(), provider)
	if err != nil || ready {
		return ready, err
	}
	if !pyTruthy(rowGet(provider, "web_search_plugin_name")) {
		return false, nil
	}
	for _, row := range catProviders(catalogCategory("web")) {
		if pyjson.Equal(rowGet(row, "web_backend"), backend) && pyTruthy(rowGet(row, "env_vars")) {
			ready, err := providerEnvReady(s.ctx(), s.home(), row)
			if err != nil {
				return false, err
			}
			if ready {
				return false, nil
			}
		}
	}
	return true, nil
}

// Managed row marker, config section and key the pick writes, in check order
var managedSelectionKeys = [][3]string{
	{"tts_provider", "tts", "provider"}, {"stt_provider", "stt", "provider"}, {"browser_provider", "browser", "cloud_provider"},
}

// Browser provider markers test membership since a local row carries an empty one, every other marker truthiness
func hasMarker(provider *pyjson.Object, marker string) bool {
	if marker == "browser_provider" {
		return rowHas(provider, marker)
	}
	return pyTruthy(rowGet(provider, marker))
}

// Active check of a Nous managed row, the feature managed and the category selection the vendor or nous
func (s *ToolsSetup) managedProviderActive(provider *pyjson.Object, cfg config.Tree, managedFeature string, forceFresh bool) (bool, error) {
	feature, ok := s.nousFeatures(cfg, forceFresh).Features[managedFeature]
	if !ok {
		return false, nil
	}
	if managedFeature == "image_gen" || managedFeature == "video_gen" {
		if genCfg, ok := cfgDictOr(cfg, managedFeature); ok {
			configured := genCfg["provider"]
			allowed, err := pySetContains(configured, nil, "", "fal", NousManagedProvider)
			if err != nil {
				return false, err
			}
			if !allowed {
				return false, nil
			}
			if !pyjson.Equal(configured, NousManagedProvider) && genCfg["use_gateway"] != nil && !truthyValue(genCfg["use_gateway"], false) {
				return false, nil
			}
		}
		return feature.ManagedByNous, nil
	}
	if hasMarker(provider, "web_backend") {
		return feature.ManagedByNous && webServingBackends(cfg)[NousManagedProvider], nil
	}
	for _, sel := range managedSelectionKeys {
		if hasMarker(provider, sel[0]) {
			if !feature.ManagedByNous {
				return false, nil
			}
			current, _ := config.CfgGet(cfg, sel[1], sel[2])
			return pySetContains(current, rowGet(provider, sel[0]), NousManagedProvider)
		}
	}
	return feature.ManagedByNous, nil
}

// Whether a browser row matches the configured cloud provider and, for the local engine rows, the engine
func browserProviderActive(provider *pyjson.Object, cfg config.Tree) bool {
	current, _ := config.CfgGet(cfg, "browser", "cloud_provider")
	if !pyjson.Equal(rowGet(provider, "browser_provider"), current) {
		return false
	}
	if pyTruthy(rowGet(provider, "browser_engine")) {
		raw, _ := config.CfgGet(cfg, "browser", "engine")
		engine := pyOrStr(raw)
		if engine == "" {
			engine = "auto"
		}
		return pyjson.Equal(pystr.Lower(pystr.Strip(engine)), rowGet(provider, "browser_engine"))
	}
	return true
}

// Whether the in tree FAL row is the configured direct key image backend
func imagegenBackendActive(provider *pyjson.Object, cfg config.Tree) (bool, error) {
	imageCfg, ok := cfgDictOr(cfg, "image_gen")
	if !ok || !pyjson.Equal(rowGet(provider, "imagegen_backend"), "fal") {
		return false, nil
	}
	allowed, err := pySetContains(imageCfg["provider"], nil, "", "fal")
	if err != nil || !allowed {
		return false, err
	}
	return !truthyValue(imageCfg["use_gateway"], false), nil
}

// Unmanaged active checks in order, the first marker a row carries decides
var activeChecks = []struct {
	marker string
	check  func(s *ToolsSetup, p *pyjson.Object, c config.Tree) (bool, error)
}{
	{"tts_provider", func(_ *ToolsSetup, p *pyjson.Object, c config.Tree) (bool, error) {
		v, _ := config.CfgGet(c, "tts", "provider")
		return pyjson.Equal(v, rowGet(p, "tts_provider")), nil
	}},
	{"stt_provider", func(_ *ToolsSetup, p *pyjson.Object, c config.Tree) (bool, error) {
		v, _ := config.CfgGet(c, "stt", "provider")
		if !pyTruthy(v) {
			v = "local"
		}
		return pyjson.Equal(v, rowGet(p, "stt_provider")), nil
	}},
	{"browser_provider", func(_ *ToolsSetup, p *pyjson.Object, c config.Tree) (bool, error) {
		return browserProviderActive(p, c), nil
	}},
	{"web_backend", (*ToolsSetup).webBackendActive},
	{"computer_use_backend", func(_ *ToolsSetup, p *pyjson.Object, c config.Tree) (bool, error) {
		v, _ := config.CfgGet(c, "computer_use", "backend")
		return pyjson.Equal(v, rowGet(p, "computer_use_backend")), nil
	}},
	{"imagegen_backend", func(_ *ToolsSetup, p *pyjson.Object, c config.Tree) (bool, error) { return imagegenBackendActive(p, c) }},
}

// Whether a provider row matches the active config
func (s *ToolsSetup) IsProviderActive(provider *pyjson.Object, cfg config.Tree, forceFresh bool) (bool, error) {
	managedFeature := managedFeatureOf(provider)
	for _, section := range []string{"image_gen", "video_gen"} {
		pluginName := rowGet(provider, section+"_plugin_name")
		if pyTruthy(pluginName) && managedFeature == "" {
			genCfg, ok := cfgDictOr(cfg, section)
			if !(ok && pyjson.Equal(genCfg["provider"], pluginName)) {
				return false, nil
			}
			return section == "video_gen" || !truthyValue(genCfg["use_gateway"], false), nil
		}
	}
	if managedFeature != "" {
		return s.managedProviderActive(provider, cfg, managedFeature, forceFresh)
	}
	for _, c := range activeChecks {
		if hasMarker(provider, c.marker) {
			return c.check(s, provider, cfg)
		}
	}
	return false, nil
}

// Index of the active provider row, else the first keyed row whose keys are present, else 0
func (s *ToolsSetup) detectActiveProviderIndex(providers []*pyjson.Object, cfg config.Tree, forceFresh bool) (int, error) {
	for i, p := range providers {
		active, err := s.IsProviderActive(p, cfg, forceFresh)
		if err != nil {
			return 0, err
		}
		if active {
			return i, nil
		}
		if pyTruthy(rowGetOr(p, "env_vars", []any{})) {
			ready, err := providerEnvReady(s.ctx(), s.home(), p)
			if err != nil {
				return 0, err
			}
			if ready {
				return i, nil
			}
		}
	}
	return 0, nil
}

// Model ids in catalog order with the metadata of each
type modelCatalog struct {
	ids  []string
	meta map[string]map[string]any
}

func newModelCatalog() *modelCatalog { return &modelCatalog{meta: map[string]map[string]any{}} }

// Adds a model, a repeated id keeping its first position and taking the later metadata
func (c *modelCatalog) put(id string, meta map[string]any) {
	if _, ok := c.meta[id]; !ok {
		c.ids = append(c.ids, id)
	}
	c.meta[id] = meta
}

func (c *modelCatalog) has(id string) bool {
	_, ok := c.meta[id]
	return ok
}

func catalogOf(ids []string, meta map[string]map[string]any) *modelCatalog {
	c := newModelCatalog()
	for _, id := range ids {
		c.put(id, meta[id])
	}
	return c
}

// The FAL model catalog and its default model
func (s *ToolsSetup) falModelCatalog(cfg config.Tree) (*modelCatalog, string) {
	fn := ToolsConfigHooks.FALModels
	requireToolsConfigHook("FALModels", fn != nil)
	ids, meta, def := fn()
	return catalogOf(ids, meta), def
}

// Managed union catalog without the gateways a free pool account cannot use
func (s *ToolsSetup) managedImageCatalog(cfg config.Tree) (*modelCatalog, string) {
	fn := ToolsConfigHooks.ManagedImageCatalog
	requireToolsConfigHook("ManagedImageCatalog", fn != nil)
	acct := s.nousFeatures(cfg, false).Account
	poolOnly := acct != nil && acct.LoggedIn && !isTrue(acct.PaidServiceAccess)
	ids, meta, def := fn(s.ctx(), s.toolEnv(), !poolOnly || acct.EntitledFor("krea"), !poolOnly)
	return catalogOf(ids, meta), def
}

// One in tree image backend, its display name, config section and catalog
type imagegenBackend struct {
	display   string
	configKey string
	catalogFn func(s *ToolsSetup, cfg config.Tree) (*modelCatalog, string)
}

var imagegenBackends = map[string]imagegenBackend{
	"fal":  {display: "FAL.ai", configKey: "image_gen", catalogFn: (*ToolsSetup).falModelCatalog},
	"nous": {display: "Nous Subscription", configKey: "image_gen", catalogFn: (*ToolsSetup).managedImageCatalog},
}

// Catalog and default model of a plugin provider, empty when unregistered or without models
func (s *ToolsSetup) pluginModelCatalog(module, pluginName string) (cat *modelCatalog, def string) {
	reg := s.pluginRegistry(module)
	defer func() {
		if recover() != nil {
			cat, def = newModelCatalog(), ""
		}
	}()
	if reg == nil {
		return newModelCatalog(), ""
	}
	p, _ := reg.Get(s.ctx(), s.toolEnv(), pluginName).(interface {
		ListModels() []map[string]any
		DefaultModel() string
	})
	if p == nil {
		return newModelCatalog(), ""
	}
	models := p.ListModels()
	def = p.DefaultModel()
	cat = newModelCatalog()
	for _, m := range models {
		if m == nil {
			continue
		}
		if id, ok := m["id"]; ok {
			cat.put(pyrepr.Str(id), m)
		}
	}
	return cat, def
}

func (s *ToolsSetup) pluginImageGenCatalog(pluginName string) (*modelCatalog, string) {
	return s.pluginModelCatalog("agent.image_gen_registry", pluginName)
}

func (s *ToolsSetup) pluginVideoGenCatalog(pluginName string) (*modelCatalog, string) {
	return s.pluginModelCatalog("agent.video_gen_registry", pluginName)
}

// Text of a metadata field the widths measure, the TypeError len raises on a non string
func metaText(meta map[string]any, key string) (string, error) {
	v, ok := meta[key]
	if !ok {
		return "", nil
	}
	text, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("TypeError: object of type '%s' has no len()", pyrepr.TypeName(v))
	}
	return text, nil
}

// Left aligned text padded to a width in code points like a str format spec
func padRight(text string, width int) string {
	if n := utf8.RuneCountInString(text); n < width {
		return text + strings.Repeat(" ", width-n)
	}
	return text
}

// Column aligned model picker writing the choice to cfg[cfgKey].model, the current model listed first
func (s *ToolsSetup) pickModelFromCatalog(cat *modelCatalog, defaultModel, cfgKey, display string, cfg config.Tree, rowIndent string) error {
	if len(cat.ids) == 0 {
		return nil
	}
	curCfg := cfgSection(cfg, cfgKey)
	current := curCfg["model"]
	if !pyTruthy(current) {
		current = defaultModel
	}
	currentModel, isStr := current.(string)
	if !isStr || !cat.has(currentModel) {
		if _, err := pySetContains(current); err != nil {
			return err
		}
		currentModel = cat.ids[0]
		if defaultModel != "" && cat.has(defaultModel) {
			currentModel = defaultModel
		}
	}
	ordered := []string{currentModel}
	for _, m := range cat.ids {
		if m != currentModel {
			ordered = append(ordered, m)
		}
	}
	widthModel, widthSpeed, widthStrengths := 0, 0, 0
	for _, m := range cat.ids {
		widthModel = max(widthModel, utf8.RuneCountInString(m))
		speed, err := metaText(cat.meta[m], "speed")
		if err != nil {
			return err
		}
		strengths, err := metaText(cat.meta[m], "strengths")
		if err != nil {
			return err
		}
		widthSpeed = max(widthSpeed, utf8.RuneCountInString(speed))
		widthStrengths = max(widthStrengths, utf8.RuneCountInString(strengths))
	}
	s.println("")
	header := "  " + padRight("Model", widthModel) + "  " + padRight("Speed", widthSpeed) + "  " + padRight("Strengths", widthStrengths) + "  Price"
	s.println(s.color(header, cliui.Cyan))
	rows := make([]string, 0, len(ordered))
	for _, mid := range ordered {
		meta := cat.meta[mid]
		speed, _ := metaText(meta, "speed")
		strengths, _ := metaText(meta, "strengths")
		row := rowIndent + padRight(mid, widthModel) + "  " + padRight(speed, widthSpeed) + "  " + padRight(strengths, widthStrengths) +
			"  " + pyrepr.Str(mapGetOr(meta, "price", ""))
		if mid == currentModel {
			row += "  ← currently in use"
		}
		rows = append(rows, row)
	}
	idx, err := s.promptChoice("  Choose "+display+" model:", rows, 0)
	if err != nil {
		return err
	}
	chosen := ordered[idx]
	curCfg["model"] = chosen
	s.Con.PrintSuccess("  Model set to: " + chosen)
	return nil
}

// Model prompt of an in tree image backend, nothing for an unknown backend
func (s *ToolsSetup) configureImagegenModel(backendName string, cfg config.Tree) error {
	backend, ok := imagegenBackends[backendName]
	if !ok {
		return nil
	}
	cat, def := backend.catalogFn(s, cfg)
	return s.pickModelFromCatalog(cat, def, backend.configKey, backend.display, cfg, "")
}

// Model prompt over a plugin image or video catalog, video rows keeping their two space indent
func (s *ToolsSetup) configureGenModelForPlugin(section, pluginName string, cfg config.Tree) error {
	catalogFn, indent := s.pluginVideoGenCatalog, "  "
	if section == "image_gen" {
		catalogFn, indent = s.pluginImageGenCatalog, ""
	}
	cat, def := catalogFn(pluginName)
	return s.pickModelFromCatalog(cat, def, section, pluginName, cfg, indent)
}

func (s *ToolsSetup) configureImagegenModelForPlugin(pluginName string, cfg config.Tree) error {
	return s.configureGenModelForPlugin("image_gen", pluginName, cfg)
}

func (s *ToolsSetup) configureVideogenModelForPlugin(pluginName string, cfg config.Tree) error {
	return s.configureGenModelForPlugin("video_gen", pluginName, cfg)
}

// Stored public URL choice of xAI Imagine for an image or video section
func (s *ToolsSetup) configureXAIImagineStorage(sectionName string, cfg config.Tree) error {
	storageCfg := cfgSection(config.Tree(cfgSection(config.Tree(cfgSection(cfg, sectionName)), "xai")), "storage")
	s.Con.PrintWarning("  xAI Imagine can store generated media and create reusable public URLs. " +
		"xAI may bill for stored files and public URL hosting.")
	choices := []string{"Enable public URLs without automatic expiry (recommended)", "Disable stored public URLs",
		"Enable public URLs for 2 days"}
	idx, err := s.promptChoice("  Stored public URLs:", choices, 0)
	if err != nil {
		return err
	}
	if idx == 1 {
		storageCfg["enabled"] = false
		s.Con.PrintSuccess("  xAI stored public URLs disabled")
		return nil
	}
	storageCfg["enabled"] = true
	storageCfg["public_url"] = true
	if idx == 2 {
		storageCfg["expires_after"] = 2 * 24 * 60 * 60
		s.Con.PrintSuccess("  xAI stored public URLs enabled for 2 days")
		return nil
	}
	storageCfg["expires_after"] = nil
	s.Con.PrintSuccess("  xAI stored public URLs enabled without automatic expiry")
	return nil
}

// Writes the vendor, or nous for a managed pick, into cfg[section][key] and drops the legacy use_gateway key
func selectInto(cfg config.Tree, section, key string, vendor any, managed bool) map[string]any {
	sec := cfgSection(cfg, section)
	if managed {
		sec[key] = NousManagedProvider
	} else {
		sec[key] = vendor
	}
	delete(sec, "use_gateway")
	return sec
}

// Persists a plugin image or video provider pick then runs its model picker
func (s *ToolsSetup) selectPluginGenProvider(section, pluginName string, cfg config.Tree, useGateway bool) error {
	sec := selectInto(cfg, section, "provider", pluginName, useGateway)
	s.Con.PrintSuccess("  " + section + ".provider set to: " + pyrepr.Str(sec["provider"]))
	if err := s.configureGenModelForPlugin(section, pluginName, cfg); err != nil {
		return err
	}
	if pluginName == "xai" {
		return s.configureXAIImagineStorage(section, cfg)
	}
	return nil
}

func (s *ToolsSetup) selectPluginImageGenProvider(pluginName string, cfg config.Tree, useGateway bool) error {
	return s.selectPluginGenProvider("image_gen", pluginName, cfg, useGateway)
}

func (s *ToolsSetup) selectPluginVideoGenProvider(pluginName string, cfg config.Tree, useGateway bool) error {
	return s.selectPluginGenProvider("video_gen", pluginName, cfg, useGateway)
}

// Static STT model list of a provider, false when the catalog has none
func (s *ToolsSetup) sttModelCatalog(provider string) ([]string, bool) {
	fn := ToolsConfigHooks.STTModelCatalog
	requireToolsConfigHook("STTModelCatalog", fn != nil)
	return fn(provider)
}

// STT model prompt after a provider pick, DeepInfra reading its live catalog
func (s *ToolsSetup) configureSTTModel(sttProvider string, cfg config.Tree) error {
	catalog, ok := s.sttModelCatalog(sttProvider)
	if !ok && sttProvider == "deepinfra" {
		fn := ToolsConfigHooks.DeepInfraModelIDs
		requireToolsConfigHook("DeepInfraModelIDs", fn != nil)
		catalog = fn(s.ctx(), s.toolEnv(), "stt")
	}
	if len(catalog) == 0 {
		return nil
	}
	provCfg := cfgSection(config.Tree(cfgSection(cfg, "stt")), sttProvider)
	keyFn := ToolsConfigHooks.STTModelConfigKey
	requireToolsConfigHook("STTModelConfigKey", keyFn != nil)
	modelKey, ok := keyFn(sttProvider)
	if !ok {
		modelKey = "model"
	}
	current := pystr.Strip(pyOrStr(provCfg[modelKey]))
	ordered := slices.Clone(catalog)
	def := slices.Index(ordered, current)
	if def < 0 {
		def = 0
	}
	idx, err := s.promptChoice("  Select STT model:", ordered, def)
	if err != nil {
		return err
	}
	chosen := ordered[idx]
	provCfg[modelKey] = chosen
	s.Con.PrintSuccess("  STT model set to: " + chosen)
	return nil
}

// Row marker key to the config section it selects into, in check order
var providerMarkerSections = [][2]string{
	{"tts_provider", "tts"}, {"stt_provider", "stt"}, {"browser_provider", "browser"}, {"web_backend", "web"},
	{"image_gen_plugin_name", "image_gen"}, {"imagegen_backend", "image_gen"}, {"video_gen_plugin_name", "video_gen"},
}

// Persists the provider and backend keys of a picked row, one provider string per category and no legacy use_gateway
func (s *ToolsSetup) writeProviderConfig(provider *pyjson.Object, cfg config.Tree, managedFeature string) error {
	managed := managedFeature != ""
	for _, pair := range [][2]string{{"tts_provider", "tts"}, {"stt_provider", "stt"}} {
		if v := rowGet(provider, pair[0]); pyTruthy(v) {
			selectInto(cfg, pair[1], "provider", v, managed)
		}
	}
	if rowHas(provider, "browser_provider") {
		bp := rowGet(provider, "browser_provider")
		if pyTruthy(bp) || managed {
			selectInto(cfg, "browser", "cloud_provider", bp, managed)
		} else {
			browser, err := setdefaultSection(cfg, "browser", "pop")
			if err != nil {
				return err
			}
			delete(browser, "use_gateway")
		}
	}
	if engine := rowGet(provider, "browser_engine"); pyTruthy(engine) {
		browser, err := setdefaultSection(cfg, "browser", "setitem")
		if err != nil {
			return err
		}
		browser["engine"] = engine
	}
	if backend := rowGet(provider, "web_backend"); pyTruthy(backend) {
		webCfg := selectInto(cfg, "web", "backend", backend, managed)
		if managed {
			delete(webCfg, "search_backend")
			delete(webCfg, "extract_backend")
		}
		tier := rowGet(provider, "web_tier")
		var tiers any
		if pyTruthy(tier) {
			if _, ok := webCfg["provider_tier"]; !ok {
				webCfg["provider_tier"] = map[string]any{}
			}
			tiers = webCfg["provider_tier"]
		} else {
			tiers = webCfg["provider_tier"]
		}
		if m, ok := pyDictOf(tiers); ok {
			if pyTruthy(tier) {
				m[pyrepr.Str(backend)] = tier
			} else {
				delete(m, pyrepr.Str(backend))
			}
		}
	}
	if cu := rowGet(provider, "computer_use_backend"); pyTruthy(cu) {
		section, err := setdefaultSection(cfg, "computer_use", "setitem")
		if err != nil {
			return err
		}
		section["backend"] = cu
	}
	switch {
	case managed && !slices.Contains([]string{"web", "tts", "stt", "browser"}, managedFeature):
		if _, ok := cfg[managedFeature]; !ok {
			cfg[managedFeature] = map[string]any{}
		}
		if section, ok := pyDictOf(cfg[managedFeature]); ok {
			section["provider"] = NousManagedProvider
			delete(section, "use_gateway")
		}
	case !managed:
		var sections []string
		for _, pair := range providerMarkerSections {
			if rowHas(provider, pair[0]) {
				sections = append(sections, pair[1])
			}
		}
		if len(sections) == 0 {
			for _, catKey := range ToolCategories.Keys {
				if cat := catalogCategory(catKey); cat != nil && providerIn(provider, catProviders(cat)) {
					sections = []string{catKey}
					break
				}
			}
		}
		for _, sectionKey := range sections {
			if section, ok := pyDictOf(cfg[sectionKey]); ok {
				delete(section, "use_gateway")
			}
		}
	}
	return nil
}

// Persists a provider selection for a toolset without prompts, the KeyError of an unknown toolset or provider returned
func (s *ToolsSetup) ApplyProviderSelection(tsKey, providerName string, cfg config.Tree) error {
	catValue, ok := ToolCategories.Get(tsKey)
	cat, _ := catValue.(*pyjson.Object)
	if !ok || cat == nil {
		return &ProviderSelectionError{"Toolset has no configurable category: " + tsKey}
	}
	providers, err := s.VisibleProviders(cat, cfg, true, nil)
	if err != nil {
		return err
	}
	var provider *pyjson.Object
	for _, p := range providers {
		if pyjson.Equal(rowGet(p, "name"), providerName) {
			provider = p
			break
		}
	}
	if provider == nil {
		return &ProviderSelectionError{"Unknown provider " + pyrepr.StrRepr(providerName) + " for toolset " + pyrepr.StrRepr(tsKey)}
	}
	managedFeature := managedFeatureOf(provider)
	if err := s.writeProviderConfig(provider, cfg, managedFeature); err != nil {
		return err
	}
	var falVendor any
	if pyTruthy(rowGet(provider, "imagegen_backend")) && managedFeature == "" {
		falVendor = "fal"
	}
	selections := []struct {
		section string
		vendor  any
	}{
		{"image_gen", rowGet(provider, "image_gen_plugin_name")},
		{"video_gen", rowGet(provider, "video_gen_plugin_name")},
		{"image_gen", falVendor},
	}
	for _, sel := range selections {
		if pyTruthy(sel.vendor) {
			selectInto(cfg, sel.section, "provider", sel.vendor, managedFeature != "")
		}
	}
	return nil
}

// Selection gate of Nous rows, swapped by tests the way hermes monkeypatches the module attribute
var nousProviderGateFn = (*ToolsSetup).nousProviderGate

// False after printing why when a Nous gated row cannot be selected, managed rows running the inline Portal login
func (s *ToolsSetup) nousProviderGate(provider *pyjson.Object, cfg config.Tree, managedFeature string, forceFresh bool) bool {
	if managedFeature != "" {
		ensure := ToolsConfigHooks.EnsureNousPortalAccess
		requireToolsConfigHook("EnsureNousPortalAccess", ensure != nil)
		capability := pyrepr.Str(rowGetOr(provider, "name", "the Nous Tool Gateway"))
		if !ensure(s.ctx(), s.home(), s.Con.Out, capability, managedCoverageCategory(managedFeature)) {
			s.Con.PrintWarning("  Not enabled — Nous Portal access is required for this backend.")
			return false
		}
		return true
	}
	if pyTruthy(rowGet(provider, "requires_nous_auth")) {
		features := s.nousFeatures(cfg, forceFresh)
		entitled := features.Account != nil && isTrue(features.Account.PaidServiceAccess)
		if !features.NousAuthPresent || !entitled {
			format := ToolsConfigHooks.NousEntitlementMessage
			requireToolsConfigHook("NousEntitlementMessage", format != nil)
			message := format(features.Account, pyrepr.Str(rowGetOr(provider, "name", "Nous Subscription")))
			if message == "" {
				message = "Nous Subscription is only available after logging into Nous Portal."
			}
			s.Con.PrintWarning("  " + message)
			return false
		}
	}
	return true
}

// Model pickers that follow a provider pick, plugin image or video gen, the in tree image backend, then STT
func (s *ToolsSetup) finishProviderSelection(provider *pyjson.Object, cfg config.Tree, managedFeature string) error {
	managed := managedFeature != ""
	for _, section := range []string{"image_gen", "video_gen"} {
		if pluginName := rowGet(provider, section+"_plugin_name"); pyTruthy(pluginName) {
			return s.selectPluginGenProvider(section, pyrepr.Str(pluginName), cfg, managed)
		}
	}
	if backend := rowGet(provider, "imagegen_backend"); pyTruthy(backend) {
		if err := s.configureImagegenModel(pyrepr.Str(backend), cfg); err != nil {
			return err
		}
		selectInto(cfg, "image_gen", "provider", "fal", managed)
	}
	if stt := rowGet(provider, "stt_provider"); pyTruthy(stt) && !managed {
		return s.configureSTTModel(pyrepr.Str(stt), cfg)
	}
	return nil
}

// Status lines announcing the backend and provider keys a pick writes
func (s *ToolsSetup) printProviderSelection(provider *pyjson.Object, managedFeature string, reconfigure bool) {
	managed := managedFeature != ""
	if reconfigure && pyTruthy(rowGet(provider, "tts_provider")) {
		s.Con.PrintSuccess("  TTS provider set to: " + rowText(provider, "tts_provider"))
	}
	if pyTruthy(rowGet(provider, "stt_provider")) {
		s.Con.PrintSuccess("  STT provider set to: " + rowText(provider, "stt_provider"))
	}
	if rowHas(provider, "browser_provider") {
		bp := rowGet(provider, "browser_provider")
		switch {
		case reconfigure && managed:
			name := "nous"
			if pyTruthy(bp) {
				name = pyrepr.Str(bp)
			}
			s.Con.PrintSuccess("  Browser cloud provider set to: " + name)
		case pyjson.Equal(bp, "local"):
			s.Con.PrintSuccess("  Browser set to local mode")
		case pyTruthy(bp):
			s.Con.PrintSuccess("  Browser cloud provider set to: " + pyrepr.Str(bp))
		}
	}
	if engine := rowGet(provider, "browser_engine"); pyTruthy(engine) && !pyjson.Equal(engine, "auto") {
		s.Con.PrintSuccess("  Browser engine set to: " + pyrepr.Str(engine))
	}
	if backend := rowGet(provider, "web_backend"); pyTruthy(backend) {
		tier := ""
		if reconfigure && pyTruthy(rowGet(provider, "web_tier")) {
			tier = " (" + rowText(provider, "web_tier") + " tier)"
		}
		name := pyrepr.Str(backend)
		if managed {
			name = NousManagedProvider
		}
		s.Con.PrintSuccess("  Web backend set to: " + name + tier)
	}
	if reconfigure && pyTruthy(rowGet(provider, "computer_use_backend")) {
		s.Con.PrintSuccess("  Computer Use backend set to: " + rowText(provider, "computer_use_backend"))
	}
}

// Whether a keyed row shares its category with a managed sibling while the user lacks Nous auth
func (s *ToolsSetup) showPortalHint(provider *pyjson.Object, cfg config.Tree, managedFeature string, forceFresh bool) (hint bool) {
	if managedFeature != "" || pyTruthy(rowGet(provider, "requires_nous_auth")) {
		return false
	}
	defer func() {
		if recover() != nil {
			hint = false
		}
	}()
	for _, catKey := range ToolCategories.Keys {
		cat := catalogCategory(catKey)
		if cat == nil {
			continue
		}
		rows := catProviders(cat)
		if !providerIn(provider, rows) {
			continue
		}
		for _, sib := range rows {
			if pyTruthy(rowGet(sib, "managed_nous_feature")) {
				return !s.nousFeatures(cfg, forceFresh).NousAuthPresent
			}
		}
	}
	return false
}

// One env var prompt, true unless the new enable flow skipped the key
func (s *ToolsSetup) promptSecret(key, label, url, defaultVal string, reconfigure bool, urlLabel string, strip bool) (bool, error) {
	existing := ""
	if reconfigure {
		var err error
		if existing, err = s.getEnvValue(key); err != nil {
			return false, err
		}
	}
	if existing != "" {
		s.Con.PrintInfo("  " + key + ": configured (" + firstRunes(existing, 8) + "...)")
	}
	if url != "" {
		s.Con.PrintInfo("  " + urlLabel + ": " + url)
	}
	if reconfigure {
		value := pystr.Strip(s.Con.Prompt("    "+label+" (Enter to keep current)", "", defaultVal == ""))
		if value != "" {
			if err := s.saveEnvValue(key, value); err != nil {
				return false, err
			}
			s.Con.PrintSuccess("    Updated")
		} else {
			s.Con.PrintInfo("    Kept current")
		}
		return true, nil
	}
	var value string
	if defaultVal != "" {
		value = s.Con.Prompt("    "+label, defaultVal, false)
	} else {
		value = s.Con.Prompt("    "+label, "", true)
	}
	if strip {
		value = pystr.Strip(value)
	}
	if value != "" {
		if err := s.saveEnvValue(key, value); err != nil {
			return false, err
		}
		s.Con.PrintSuccess("    Saved")
		return true, nil
	}
	s.Con.PrintWarning("    Skipped")
	return false, nil
}

// Row dict of an env_vars entry
func envVarRow(v any) (*pyjson.Object, error) {
	switch x := v.(type) {
	case *pyjson.Object:
		return x, nil
	case map[string]any:
		return pickerValue(x).(*pyjson.Object), nil
	}
	return nil, fmt.Errorf("TypeError: '%s' object is not subscriptable", pyrepr.TypeName(v))
}

// Prompts for a provider's env vars, true when every key ended up configured
func (s *ToolsSetup) promptEnvVars(envVars []any, reconfigure bool) (bool, error) {
	allConfigured := true
	for _, item := range envVars {
		row, err := envVarRow(item)
		if err != nil {
			return false, err
		}
		keyValue, ok := row.Get("key")
		if !ok {
			return false, fmt.Errorf("KeyError: 'key'")
		}
		key := pyrepr.Str(keyValue)
		if !reconfigure {
			existing, err := s.getEnvValue(key)
			if err != nil {
				return false, err
			}
			if existing != "" {
				s.Con.PrintSuccess("  " + key + ": already configured")
				continue
			}
		}
		ok, err = s.promptSecret(key, pyrepr.Str(row.GetOr("prompt", key)), pyrepr.Str(row.GetOr("url", "")),
			pyrepr.Str(row.GetOr("default", "")), reconfigure, "Get yours at", false)
		if err != nil {
			return false, err
		}
		allConfigured = allConfigured && ok
	}
	return allConfigured, nil
}

// Env var list of a row, a non list value read as a list of its own
func envVarList(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out
	case nil:
		return nil
	}
	return []any{v}
}

// Configures one provider, prompting for its keys, writing its config and running its post setup hook
func (s *ToolsSetup) configureProvider(provider *pyjson.Object, cfg config.Tree, forceFresh, reconfigure bool) error {
	envVars := rowGetOr(provider, "env_vars", []any{})
	managedFeature := managedFeatureOf(provider)
	if !nousProviderGateFn(s, provider, cfg, managedFeature, forceFresh) {
		return nil
	}
	s.printProviderSelection(provider, managedFeature, reconfigure)
	if err := s.writeProviderConfig(provider, cfg, managedFeature); err != nil {
		return err
	}
	postSetup := rowGet(provider, "post_setup")
	if !pyTruthy(envVars) {
		if pyTruthy(postSetup) {
			if err := s.runPostSetup(pyrepr.Str(postSetup)); err != nil {
				return err
			}
		}
		s.Con.PrintSuccess("  " + rowText(provider, "name") + " - no configuration needed!")
		if managedFeature != "" {
			s.Con.PrintInfo("  Requests for this tool will be billed to your Nous subscription.")
		}
		return s.finishProviderSelection(provider, cfg, managedFeature)
	}
	if !reconfigure && s.showPortalHint(provider, cfg, managedFeature, forceFresh) {
		s.Con.PrintInfo("  Available through Nous Portal subscription.")
	}
	allConfigured, err := s.promptEnvVars(envVarList(envVars), reconfigure)
	if err != nil {
		return err
	}
	if pyTruthy(postSetup) && allConfigured {
		if err := s.runPostSetup(pyrepr.Str(postSetup)); err != nil {
			return err
		}
	}
	if allConfigured {
		if !reconfigure {
			s.Con.PrintSuccess("  " + rowText(provider, "name") + " configured!")
		}
		return s.finishProviderSelection(provider, cfg, managedFeature)
	}
	return nil
}

// Reconfigures a provider, re-prompting every key
func (s *ToolsSetup) reconfigureProvider(provider *pyjson.Object, cfg config.Tree, forceFresh bool) error {
	return s.configureProvider(provider, cfg, forceFresh, true)
}

// Interactive vision backend choice over auxiliary.vision, auto clearing every pinned key
func (s *ToolsSetup) configureVisionBackend() error {
	s.println("")
	s.println(s.color("  Vision / Image Analysis needs a multimodal model.", cliui.Yellow))
	s.println(s.color("  Pick any provider + model (like /model), or let it auto-detect.", cliui.Dim))
	choices := []string{
		"Auto — use your main model / aggregator fallback (recommended)",
		"Pick a provider and model",
		"Custom OpenAI-compatible endpoint — base URL, API key, model",
		"Skip",
	}
	idx, err := s.promptChoice("  Configure vision backend", choices, 0)
	if err != nil {
		return err
	}
	cfg, err := s.loadConfig()
	if err != nil {
		return err
	}
	visionCfg := cfgSection(config.Tree(cfgSection(cfg, "auxiliary")), "vision")
	switch idx {
	case 0:
		for _, key := range []string{"provider", "model", "base_url", "api_key", "api_mode"} {
			delete(visionCfg, key)
		}
		if err := s.saveConfig(cfg); err != nil {
			return err
		}
		s.Con.PrintSuccess("  Vision set to auto (main model / aggregator fallback)")
	case 1:
		return s.configureVisionProviderModel(cfg, visionCfg)
	case 2:
		baseURL := pystr.Strip(s.Con.Prompt("    Base URL (blank for OpenAI)", "", false))
		if baseURL == "" {
			baseURL = "https://api.openai.com/v1"
		}
		host, err := pyurl.BaseURLHostname(baseURL)
		if err != nil {
			return err
		}
		isNativeOpenAI := host == "api.openai.com"
		keyLabel := "    API key"
		if isNativeOpenAI {
			keyLabel = "    OPENAI_API_KEY"
		}
		apiKey := s.Con.Prompt(keyLabel, "", true)
		if pystr.Strip(apiKey) == "" {
			s.Con.PrintWarning("    Skipped")
			return nil
		}
		defaultModel, question := "", "    Vision model"
		if isNativeOpenAI {
			defaultModel = "gpt-4o-mini"
			question += " (blank for " + defaultModel + ")"
		}
		model := pystr.Strip(s.Con.Prompt(question, "", false))
		if model == "" {
			model = defaultModel
		}
		if err := s.saveEnvValue("OPENAI_API_KEY", pystr.Strip(apiKey)); err != nil {
			return err
		}
		visionCfg["provider"] = "custom"
		visionCfg["base_url"] = baseURL
		if model != "" {
			visionCfg["model"] = model
		} else {
			delete(visionCfg, "model")
		}
		if err := s.saveConfig(cfg); err != nil {
			return err
		}
		suffix := ""
		if model != "" {
			suffix = " (" + model + ")"
		}
		s.Con.PrintSuccess("  Vision set to custom endpoint" + suffix)
	default:
		s.Con.PrintInfo("  Skipped vision configuration")
	}
	return nil
}

// Missing provider inventory, the import failure hermes reports when it cannot load the picker rows
var errInventoryUnwired = errors.New("provider inventory is not installed")

// Aux picker rows of the vision task, the detection failure returned as an error
func (s *ToolsSetup) visionPickerRows(currentProvider, currentModel, currentBaseURL string) (rows []map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			rows, err = nil, fmt.Errorf("%v", r)
		}
	}()
	return ToolsConfigHooks.AuxPickerRows(s.ctx(), currentProvider, currentModel, currentBaseURL, 40)
}

// Provider and model picker for vision over the aux picker rows, persisting provider and model
func (s *ToolsSetup) configureVisionProviderModel(cfg config.Tree, visionCfg map[string]any) error {
	if ToolsConfigHooks.AuxPickerRows == nil || ToolsConfigHooks.AuxPickerLabels == nil {
		s.Con.PrintWarning("  Could not load provider list: " + errInventoryUnwired.Error())
		return nil
	}
	currentProvider := pystr.Strip(pyOrStr(visionCfg["provider"]))
	currentModel := pystr.Strip(pyOrStr(visionCfg["model"]))
	currentBaseURL := pystr.Strip(pyOrStr(visionCfg["base_url"]))
	providers, err := s.visionPickerRows(currentProvider, currentModel, currentBaseURL)
	if err != nil {
		s.Con.PrintWarning("  Could not detect providers: " + pyExcText(err))
		providers = nil
	}
	if len(providers) == 0 {
		s.Con.PrintWarning("  No authenticated providers found. Configure a provider first " +
			"with `icebath model`, then re-run this.")
		return nil
	}
	labels := append(ToolsConfigHooks.AuxPickerLabels(providers, currentProvider, currentBaseURL), "Cancel")
	pidx, err := s.promptChoice("  Choose vision provider:", labels, 0)
	if err != nil {
		return err
	}
	if pidx >= len(providers) {
		s.Con.PrintInfo("  Cancelled")
		return nil
	}
	chosen := providers[pidx]
	slug := chosen["slug"]
	var models []string
	switch list := mapGetOr(chosen, "models", []any{}).(type) {
	case []string:
		models = slices.Clone(list)
	case []any:
		for _, m := range list {
			models = append(models, pyrepr.Str(m))
		}
	}
	label := chosen["name"]
	if !pyTruthy(label) {
		label = slug
	}
	midx, err := s.promptChoice("  Choose vision model for "+pyrepr.Str(label)+":", append(slices.Clone(models), "Type a custom model id…"), 0)
	if err != nil {
		return err
	}
	var model string
	if midx < len(models) {
		model = models[midx]
	} else {
		model = pystr.Strip(s.Con.Prompt("    Model id", "", false))
		if model == "" {
			s.Con.PrintWarning("  No model entered — cancelled")
			return nil
		}
	}
	visionCfg["provider"] = slug
	visionCfg["model"] = model
	delete(visionCfg, "base_url")
	delete(visionCfg, "api_key")
	if err := s.saveConfig(cfg); err != nil {
		return err
	}
	s.Con.PrintSuccess("  Vision set to " + pyrepr.Str(slug) + " / " + model)
	return nil
}

// Key prompts for toolsets that only need env vars, vision running its own provider and model picker
func (s *ToolsSetup) configureSimpleRequirements(tsKey string, reconfigure bool) error {
	if tsKey == "vision" {
		if reconfigure {
			return s.configureVisionBackend()
		}
		has, err := s.toolsetHasKeys("vision", nil, false, nil)
		if err != nil {
			return err
		}
		if !has {
			return s.configureVisionBackend()
		}
		return nil
	}
	var requirements [][2]string
	for _, req := range ToolsetEnvRequirements[tsKey] {
		if !reconfigure {
			v, err := s.getEnvValue(req[0])
			if err != nil {
				return err
			}
			if v != "" {
				continue
			}
		}
		requirements = append(requirements, req)
	}
	if len(requirements) == 0 {
		return nil
	}
	tsLabel := toolsetLabel(s.ctx(), tsKey)
	s.println("")
	if reconfigure {
		s.println(s.color("  "+tsLabel+":", cliui.Cyan))
	} else {
		s.println(s.color("  "+tsLabel+" requires configuration:", cliui.Yellow))
	}
	for _, req := range requirements {
		if _, err := s.promptSecret(req[0], req[0], req[1], "", reconfigure, "Get key at", true); err != nil {
			return err
		}
	}
	return nil
}
