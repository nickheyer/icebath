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
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/setupflow"
)

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

// Rows of a category, a panic for a provider entry that is not a mapping
func catProviders(cat *pyjson.Object) []*pyjson.Object {
	raw, _ := cat.GetOr("providers", []any{}).([]any)
	out := make([]*pyjson.Object, 0, len(raw))
	for _, item := range raw {
		row, ok := item.(*pyjson.Object)
		if !ok {
			panic(fmt.Sprintf("a tool provider entry is %s, expected a mapping", errchain.Kind(item)))
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

// Membership of a value among members, an error for a list or mapping value
func pySetContains(v any, members ...any) (bool, error) {
	switch v.(type) {
	case []any, []string, map[string]any, config.Tree, *pyjson.Object:
		return false, fmt.Errorf("expected a single name, got %s", errchain.Kind(v))
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
func setdefaultSection(cfg map[string]any, key string) (map[string]any, error) {
	v, ok := cfg[key]
	if !ok {
		m := map[string]any{}
		cfg[key] = m
		return m, nil
	}
	if m, ok := pyDictOf(v); ok {
		return m, nil
	}
	return nil, fmt.Errorf("%s in config is %s, expected a mapping", key, errchain.Kind(v))
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
		panic(fmt.Sprintf("expected a list of mappings, got %s", errchain.Kind(v)))
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

// Installed picker registry of a plugin category, nil when no package provides it
func pickerRegistryFor(category string) *PickerRegistry {
	switch category {
	case "image_gen":
		return ToolsConfigHooks.ImageGenRegistry
	case "video_gen":
		return ToolsConfigHooks.VideoGenRegistry
	case "web":
		return webPickerRegistry
	case "browser":
		return ToolsConfigHooks.BrowserRegistry
	case "tts":
		return ToolsConfigHooks.TTSRegistry
	}
	return nil
}

// Plugin registry of a category after plugin discovery, nil on any failure
func (s *ToolsSetup) pluginRegistry(category string) (reg *PickerRegistry) {
	defer func() {
		if recover() != nil {
			reg = nil
		}
	}()
	reg = pickerRegistryFor(category)
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
func (s *ToolsSetup) pluginProviderRows(category string, markerKeys []string, requireName, skipBuiltin, flattenVariants bool) []*pyjson.Object {
	reg := s.pluginRegistry(category)
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

// Marker keys and row options of one plugin backed category
type pluginRowSpec struct {
	markers                                   []string
	requireName, skipBuiltin, flattenVariants bool
}

var pluginProviderRowSpecs = map[string]pluginRowSpec{
	"image_gen": {markers: []string{"image_gen_plugin_name"}},
	"video_gen": {markers: []string{"video_gen_plugin_name"}},
	"web":       {markers: []string{"web_backend", "web_search_plugin_name"}, requireName: true, flattenVariants: true},
	"browser":   {markers: []string{"browser_provider", "browser_plugin_name"}, requireName: true},
	"tts":       {markers: []string{"tts_provider", "tts_plugin_name"}, requireName: true, skipBuiltin: true},
}

// Picker rows of the plugin registered providers of one category
func (s *ToolsSetup) pluginRowsFor(category string) []*pyjson.Object {
	spec, ok := pluginProviderRowSpecs[category]
	if !ok {
		panic("unknown provider category " + category)
	}
	return s.pluginProviderRows(category, spec.markers, spec.requireName, spec.skipBuiltin, spec.flattenVariants)
}

func (s *ToolsSetup) pluginImageGenProviders() []*pyjson.Object  { return s.pluginRowsFor("image_gen") }
func (s *ToolsSetup) pluginVideoGenProviders() []*pyjson.Object  { return s.pluginRowsFor("video_gen") }
func (s *ToolsSetup) pluginWebSearchProviders() []*pyjson.Object { return s.pluginRowsFor("web") }
func (s *ToolsSetup) pluginBrowserProviders() []*pyjson.Object   { return s.pluginRowsFor("browser") }
func (s *ToolsSetup) pluginTTSProviders() []*pyjson.Object       { return s.pluginRowsFor("tts") }
func pluginRowsBuilder(fn func(*ToolsSetup) []*pyjson.Object) func(*ToolsSetup) ([]*pyjson.Object, error) {
	return func(s *ToolsSetup) ([]*pyjson.Object, error) { return fn(s), nil }
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

// Provider rows of a category, plugin rows after the catalog rows
func (s *ToolsSetup) VisibleProviders(cat *pyjson.Object) ([]*pyjson.Object, error) {
	visible := append([]*pyjson.Object{}, catProviders(cat)...)
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

// Whether enabling a toolset should open provider setup
func (s *ToolsSetup) toolsetNeedsConfigurationPrompt(tsKey string, cfg config.Tree) (bool, error) {
	cat := catalogCategory(tsKey)
	if cat == nil || cat.Len() == 0 {
		has, err := s.toolsetHasKeys(tsKey, cfg)
		return !has, err
	}
	providers, err := s.VisibleProviders(cat)
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
		return !FalKeyIsConfigured(s.toolEnv()) && !s.anyPluginProviderAvailable("image_gen"), nil
	}
	if tsKey == "video_gen" {
		return !s.anyPluginProviderAvailable("video_gen"), nil
	}
	has, err := s.toolsetHasKeys(tsKey, cfg)
	return !has, err
}

// Whether any provider of a plugin registry reports itself available
func (s *ToolsSetup) anyPluginProviderAvailable(category string) (found bool) {
	reg := s.pluginRegistry(category)
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
func (s *ToolsSetup) configureToolCategory(tsKey string, cat *pyjson.Object, cfg config.Tree, reconfigure bool) error {
	name, ok := cat.Get("name")
	if !ok {
		return errors.New("a tool category has no name")
	}
	providers, err := s.VisibleProviders(cat)
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
		return s.configureProvider(providers[0], cfg, reconfigure)
	}
	s.println("")
	choices, options := []string{}, []setupflow.Option{}
	for _, p := range providers {
		badge, tag, configured := "", "", ""
		if pyTruthy(rowGet(p, "badge")) {
			badge = " [" + rowText(p, "badge") + "]"
		}
		if pyTruthy(rowGet(p, "tag")) {
			tag = rowText(p, "tag")
		}
		ready, err := providerEnvReady(s.ctx(), s.home(), p)
		if err != nil {
			return err
		}
		if ready {
			active, err := s.IsProviderActive(p, cfg)
			if err != nil {
				return err
			}
			if active {
				configured = " [active]"
			} else if pyTruthy(rowGetOr(p, "env_vars", []any{})) {
				configured = " [configured]"
			}
		}
		label := rowText(p, "name") + badge
		if tag != "" {
			label += " — " + tag
		}
		label += configured
		choices = append(choices, label)
		options = append(options, setupflow.Option{Title: rowText(p, "name") + badge + configured, Detail: strings.TrimSpace(tag)})
	}
	if !reconfigure {
		choices = append(choices, "Skip — keep defaults / configure later")
		options = append(options, setupflow.Option{Title: "Skip — keep defaults / configure later"})
	}
	defaultIdx, err := s.detectActiveProviderIndex(providers, cfg)
	if err != nil {
		return err
	}
	question := "  " + title + ":"
	if reconfigure {
		question = "  Select provider:"
	}
	idx, err := s.chooseOption(question, options, choices, defaultIdx)
	if err != nil {
		return err
	}
	if idx >= len(providers) {
		s.Con.PrintInfo("  Skipped " + pyrepr.Str(name))
		return nil
	}
	return s.configureProvider(providers[idx], cfg, reconfigure)
}

// Provider env lookup of the tier auto detection, swapped by tests
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
		panic("a web provider row has no web_backend")
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
	shared := name("backend")
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

// Browser provider markers test membership since a local row carries an empty one, every other marker truthiness
func hasMarker(provider *pyjson.Object, marker string) bool {
	if marker == "browser_provider" {
		return rowHas(provider, marker)
	}
	return pyTruthy(rowGet(provider, marker))
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
	return allowed && err == nil, err
}

// Active checks in order, the first marker a row carries decides
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
func (s *ToolsSetup) IsProviderActive(provider *pyjson.Object, cfg config.Tree) (bool, error) {
	for _, section := range []string{"image_gen", "video_gen"} {
		if pluginName := rowGet(provider, section+"_plugin_name"); pyTruthy(pluginName) {
			genCfg, ok := cfgDictOr(cfg, section)
			return ok && pyjson.Equal(genCfg["provider"], pluginName), nil
		}
	}
	for _, c := range activeChecks {
		if hasMarker(provider, c.marker) {
			return c.check(s, provider, cfg)
		}
	}
	return false, nil
}

// Index of the active provider row, else the first keyed row whose keys are present, else 0
func (s *ToolsSetup) detectActiveProviderIndex(providers []*pyjson.Object, cfg config.Tree) (int, error) {
	for i, p := range providers {
		active, err := s.IsProviderActive(p, cfg)
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

// One in tree image backend, its display name, config section and catalog
type imagegenBackend struct {
	display   string
	configKey string
	catalogFn func(s *ToolsSetup, cfg config.Tree) (*modelCatalog, string)
}

var imagegenBackends = map[string]imagegenBackend{
	"fal": {display: "FAL.ai", configKey: "image_gen", catalogFn: (*ToolsSetup).falModelCatalog},
}

// Catalog and default model of a plugin provider, empty when unregistered or without models
func (s *ToolsSetup) pluginModelCatalog(category, pluginName string) (cat *modelCatalog, def string) {
	reg := s.pluginRegistry(category)
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
	return s.pluginModelCatalog("image_gen", pluginName)
}

func (s *ToolsSetup) pluginVideoGenCatalog(pluginName string) (*modelCatalog, string) {
	return s.pluginModelCatalog("video_gen", pluginName)
}

// Text of a metadata field the widths measure, an error for a value that is no text
func metaText(meta map[string]any, key string) (string, error) {
	v, ok := meta[key]
	if !ok {
		return "", nil
	}
	text, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("model metadata %s is %s, expected text", key, errchain.Kind(v))
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
	if s.Ask != nil {
		return s.pickModelOption(cat, ordered, currentModel, cfgKey, display, cfg)
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

// Model choice through the asker, each row naming its speed, strengths, price and whether it is in use
func (s *ToolsSetup) pickModelOption(cat *modelCatalog, ordered []string, currentModel, cfgKey, display string, cfg config.Tree) error {
	options := make([]setupflow.Option, 0, len(ordered))
	for _, mid := range ordered {
		meta := cat.meta[mid]
		parts := []string{}
		for _, field := range []string{"speed", "strengths"} {
			text, err := metaText(meta, field)
			if err != nil {
				return err
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
		if price := pyrepr.Str(mapGetOr(meta, "price", "")); price != "" {
			parts = append(parts, price)
		}
		if mid == currentModel {
			parts = append(parts, "currently in use")
		}
		options = append(options, setupflow.Option{Title: mid, Detail: strings.Join(parts, " · ")})
	}
	idx, err := s.Ask.Choose("  Choose "+display+" model:", options, 0)
	if err != nil {
		return err
	}
	chosen := ordered[idx]
	cfgSection(cfg, cfgKey)["model"] = chosen
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

// Writes the vendor into cfg[section][key]
func selectInto(cfg config.Tree, section, key string, vendor any) map[string]any {
	sec := cfgSection(cfg, section)
	sec[key] = vendor
	return sec
}

// Persists a plugin image or video provider pick then runs its model picker
func (s *ToolsSetup) selectPluginGenProvider(section, pluginName string, cfg config.Tree) error {
	sec := selectInto(cfg, section, "provider", pluginName)
	s.Con.PrintSuccess("  " + section + ".provider set to: " + pyrepr.Str(sec["provider"]))
	if err := s.configureGenModelForPlugin(section, pluginName, cfg); err != nil {
		return err
	}
	if pluginName == "xai" {
		return s.configureXAIImagineStorage(section, cfg)
	}
	return nil
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

// Persists the provider and backend keys of a picked row, one provider string per category
func (s *ToolsSetup) writeProviderConfig(provider *pyjson.Object, cfg config.Tree) error {
	for _, pair := range [][2]string{{"tts_provider", "tts"}, {"stt_provider", "stt"}} {
		if v := rowGet(provider, pair[0]); pyTruthy(v) {
			selectInto(cfg, pair[1], "provider", v)
		}
	}
	if bp := rowGet(provider, "browser_provider"); pyTruthy(bp) {
		selectInto(cfg, "browser", "cloud_provider", bp)
	}
	if engine := rowGet(provider, "browser_engine"); pyTruthy(engine) {
		browser, err := setdefaultSection(cfg, "browser")
		if err != nil {
			return err
		}
		browser["engine"] = engine
	}
	if backend := rowGet(provider, "web_backend"); pyTruthy(backend) {
		webCfg := selectInto(cfg, "web", "backend", backend)
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
		section, err := setdefaultSection(cfg, "computer_use")
		if err != nil {
			return err
		}
		section["backend"] = cu
	}
	return nil
}

// Model pickers that follow a provider pick, plugin image or video gen, the in tree image backend, then STT
func (s *ToolsSetup) finishProviderSelection(provider *pyjson.Object, cfg config.Tree) error {
	for _, section := range []string{"image_gen", "video_gen"} {
		if pluginName := rowGet(provider, section+"_plugin_name"); pyTruthy(pluginName) {
			return s.selectPluginGenProvider(section, pyrepr.Str(pluginName), cfg)
		}
	}
	if backend := rowGet(provider, "imagegen_backend"); pyTruthy(backend) {
		if err := s.configureImagegenModel(pyrepr.Str(backend), cfg); err != nil {
			return err
		}
		selectInto(cfg, "image_gen", "provider", "fal")
	}
	if stt := rowGet(provider, "stt_provider"); pyTruthy(stt) {
		return s.configureSTTModel(pyrepr.Str(stt), cfg)
	}
	if pyrepr.Str(rowGet(provider, "tts_provider")) == "piper" {
		return s.configurePiperVoice(cfg)
	}
	return nil
}

// Voice prompt after Piper is picked, over the voices the engine can fetch, the configured or default voice preselected
func (s *ToolsSetup) configurePiperVoice(cfg config.Tree) error {
	fn := ToolsConfigHooks.PiperVoices
	requireToolsConfigHook("PiperVoices", fn != nil)
	voices, defaultVoice := fn()
	if len(voices) == 0 {
		return nil
	}
	piper := cfgSection(config.Tree(cfgSection(cfg, "tts")), "piper")
	current := pystr.Strip(pyOrStr(piper["voice"]))
	if current == "" {
		current = defaultVoice
	}
	def := max(slices.Index(voices, current), 0)
	idx, err := s.promptChoice("  Select Piper voice (language_REGION-name-quality, downloaded on first use):", voices, def)
	if err != nil {
		return err
	}
	piper["voice"] = voices[idx]
	s.Con.PrintSuccess("  Piper voice set to: " + voices[idx])
	return nil
}

// Status lines announcing the backend and provider keys a pick writes
func (s *ToolsSetup) printProviderSelection(provider *pyjson.Object, reconfigure bool) {
	if reconfigure && pyTruthy(rowGet(provider, "tts_provider")) {
		s.Con.PrintSuccess("  TTS provider set to: " + rowText(provider, "tts_provider"))
	}
	if pyTruthy(rowGet(provider, "stt_provider")) {
		s.Con.PrintSuccess("  STT provider set to: " + rowText(provider, "stt_provider"))
	}
	if rowHas(provider, "browser_provider") {
		bp := rowGet(provider, "browser_provider")
		switch {
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
		s.Con.PrintSuccess("  Web backend set to: " + pyrepr.Str(backend) + tier)
	}
	if reconfigure && pyTruthy(rowGet(provider, "computer_use_backend")) {
		s.Con.PrintSuccess("  Computer Use backend set to: " + rowText(provider, "computer_use_backend"))
	}
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
		answer, err := s.promptText("    "+label+" (Enter to keep current)", "", defaultVal == "")
		if err != nil {
			return false, err
		}
		if value := pystr.Strip(answer); value != "" {
			if err := s.saveEnvValue(key, value); err != nil {
				return false, err
			}
			s.Con.PrintSuccess("    Updated")
		} else {
			s.Con.PrintInfo("    Kept current")
		}
		return true, nil
	}
	value, err := s.promptText("    "+label, defaultVal, defaultVal == "")
	if err != nil {
		return false, err
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
	return nil, fmt.Errorf("an env_vars entry is %s, expected a mapping", errchain.Kind(v))
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
			return false, errors.New("an env_vars entry has no key")
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
func (s *ToolsSetup) configureProvider(provider *pyjson.Object, cfg config.Tree, reconfigure bool) error {
	envVars := rowGetOr(provider, "env_vars", []any{})
	s.printProviderSelection(provider, reconfigure)
	if err := s.writeProviderConfig(provider, cfg); err != nil {
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
		return s.finishProviderSelection(provider, cfg)
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
		return s.finishProviderSelection(provider, cfg)
	}
	return nil
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
		answer, err := s.promptText("    Base URL (blank for OpenAI)", "", false)
		if err != nil {
			return err
		}
		baseURL := pystr.Strip(answer)
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
		apiKey, err := s.promptText(keyLabel, "", true)
		if err != nil {
			return err
		}
		if pystr.Strip(apiKey) == "" {
			s.Con.PrintWarning("    Skipped")
			return nil
		}
		defaultModel, question := "", "    Vision model"
		if isNativeOpenAI {
			defaultModel = "gpt-4o-mini"
			question += " (blank for " + defaultModel + ")"
		}
		answer, err = s.promptText(question, "", false)
		if err != nil {
			return err
		}
		model := pystr.Strip(answer)
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

// Missing provider inventory, the import failure reported when the picker rows cannot load
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
		answer, err := s.promptText("    Model id", "", false)
		if err != nil {
			return err
		}
		if model = pystr.Strip(answer); model == "" {
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
		has, err := s.toolsetHasKeys("vision", nil)
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
