package provider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

const (
	pluginDirStampTTL    = time.Second
	pluginDirMtimeTickNS = int64(2_000_000_000)
	pluginIsolationHost  = "host"
	pluginIsolationInPro = "in_process"
)

// Bundled model provider plugin directories in sorted discovery order with the profiles each registers
var bundledPluginDirs = []struct {
	dir   string
	names []string
}{
	{"actual", []string{"actual"}}, {"ai-gateway", []string{"ai-gateway"}},
	{"alibaba", []string{"alibaba", "alibaba-cn", "alibaba-token-plan", "alibaba-token-plan-cn"}},
	{"alibaba-coding-plan", []string{"alibaba-coding-plan", "alibaba-coding-plan-cn"}},
	{"anthropic", []string{"anthropic"}}, {"arcee", []string{"arcee"}}, {"azure-foundry", []string{"azure-foundry"}},
	{"bedrock", []string{"bedrock"}}, {"commandcode", []string{"commandcode", "commandcode-anthropic"}},
	{"custom", []string{"custom"}}, {"deepinfra", []string{"deepinfra"}}, {"deepseek", []string{"deepseek"}},
	{"fireworks", []string{"fireworks"}}, {"gemini", []string{"gemini"}}, {"gmi", []string{"gmi"}},
	{"huggingface", []string{"huggingface"}}, {"kilocode", []string{"kilocode"}},
	{"kimi-coding", []string{"kimi-coding", "kimi-coding-cn"}}, {"lmstudio", []string{"lmstudio"}},
	{"meta-ai", []string{"meta-ai"}}, {"minimax", []string{"minimax", "minimax-cn", "minimax-oauth"}},
	{"nebius-token-factory", []string{"nebius-token-factory"}}, {"nous", []string{"nous"}}, {"novita", []string{"novita"}},
	{"nvidia", []string{"nvidia"}}, {"ollama-cloud", []string{"ollama-cloud"}}, {"openai-codex", []string{"openai-codex"}},
	{"opencode-zen", []string{"opencode-zen", "opencode-go"}}, {"openrouter", []string{"openrouter"}},
	{"qwen-oauth", []string{"qwen-oauth"}}, {"router", []string{"router"}}, {"solstice", []string{"solstice"}},
	{"stepfun", []string{"stepfun"}}, {"tencent-tokenhub", []string{"tencent-tokenhub"}}, {"upstage", []string{"upstage"}},
	{"vertex", []string{"vertex"}}, {"xai", []string{"xai"}}, {"xiaomi", []string{"xiaomi"}}, {"zai", []string{"zai"}},
}

// The profiles of one home's plugin directories with the scan bookkeeping for that home
type homeLayer struct {
	mu             sync.Mutex
	scanMu         sync.Mutex
	registry       map[string]*Profile
	order          []string
	aliases        map[string]string
	stamps         []int64
	stampCheckedAt time.Time
	misses         map[string]time.Time
	errors         map[string]string
	hosted         map[string]bool
}

func newHomeLayer() *homeLayer {
	return &homeLayer{registry: map[string]*Profile{}, aliases: map[string]string{}, misses: map[string]time.Time{},
		errors: map[string]string{}, hosted: map[string]bool{}}
}

var (
	regMu          sync.RWMutex
	regProfiles    = map[string]*Profile{}
	regOrder       []string
	regAliases     = map[string]string{}
	regSources     = map[string]string{}
	regListCache   []*Profile
	regGeneration  atomic.Int64
	regDiscovered  atomic.Bool
	regDiscovering atomic.Bool
	// Set once the auth registry is built, the mirror skipped before then as the build syncs on first read
	authRegistryReady atomic.Bool
	regScanDepth      atomic.Int32
	discoveryMu       reentrantMutex
	homeLayersMu      sync.Mutex
	homeLayers        = map[string]*homeLayer{}
	hostMemoMu        sync.Mutex
	hostMemo          = map[hostMemoKey]hostMemoEntry{}
	namedCustomMu     sync.Mutex
	namedCustomMem    = map[string]*namedCustomMemo{}
)

type hostMemoKey struct {
	home, host string
	metadata   bool
}

type hostMemoEntry struct {
	token   [3]int64
	profile *Profile
}

type namedCustomMemo struct {
	sig     []any
	answers map[string]bool
}

// Discovery or a home plugin scan is running somewhere in the process
func discoveringNow() bool { return regDiscovering.Load() || regScanDepth.Load() > 0 }

// Fills the auth kind, auth type and wire defaults a registered profile leaves empty
func normalizeRegisteredProfile(p *Profile) {
	if p.Auth == "" {
		p.Auth = AuthKindOf(p.AuthType)
	}
	if p.AuthType == "" {
		p.AuthType = AuthTypeAPIKey
	}
	if p.Mode == "" {
		p.Mode = ModeChatCompletions
	}
}

// RegisterProvider registers a profile process wide by name and aliases, later registrations replacing earlier ones
func RegisterProvider(p Profile) { registerProcessWide(p, "runtime") }

// UnregisterProvider drops a runtime registered profile and its aliases, the restore step of a test registration
func UnregisterProvider(name string) {
	regGeneration.Add(1)
	regMu.Lock()
	defer regMu.Unlock()
	p, ok := regProfiles[name]
	if !ok {
		return
	}
	delete(regProfiles, name)
	delete(regSources, name)
	for _, a := range p.Aliases {
		if regAliases[a] == name {
			delete(regAliases, a)
		}
	}
	regOrder = slices.DeleteFunc(regOrder, func(n string) bool { return n == name })
	regListCache = nil
}

// Registers one bundled plugin profile process wide with the bundled source
func registerBundledProvider(p Profile) { registerProcessWide(p, "bundled") }

func registerProcessWide(p Profile, source string) {
	normalizeRegisteredProfile(&p)
	pp := &p
	regGeneration.Add(1)
	regMu.Lock()
	if _, ok := regProfiles[p.Name]; !ok {
		regOrder = append(regOrder, p.Name)
	}
	regProfiles[p.Name] = pp
	regSources[p.Name] = source
	for _, a := range p.Aliases {
		regAliases[a] = p.Name
	}
	regListCache = nil
	regMu.Unlock()
	if regDiscovered.Load() && !discoveringNow() {
		syncAuthRegistry(context.Background())
	}
}

// Registers a home plugin profile into that home's layer
func registerIntoLayer(layer *homeLayer, p Profile) {
	normalizeRegisteredProfile(&p)
	pp := &p
	regGeneration.Add(1)
	layer.mu.Lock()
	defer layer.mu.Unlock()
	if _, ok := layer.registry[p.Name]; !ok {
		layer.order = append(layer.order, p.Name)
	}
	layer.registry[p.Name] = pp
	for _, a := range p.Aliases {
		layer.aliases[a] = p.Name
	}
}

// Mirrors profiles into the auth registry and picker catalog once built, and the env catalog without config or home scans
func syncAuthRegistry(ctx context.Context) {
	if authRegistryReady.Load() {
		syncPluginProviderRegistry(ctx)
	}
	syncPluginProviderCatalogIfBuilt()
	SyncRegisteredProviders(processWideRegisteredProfiles())
}

// Process wide registered profiles in registration order, bundled sources left out
func processWideRegisteredProfiles() []Profile {
	regMu.Lock()
	defer regMu.Unlock()
	var out []Profile
	for _, name := range regOrder {
		if p := regProfiles[name]; p != nil && regSources[name] != "bundled" {
			out = append(out, *p)
		}
	}
	return out
}

// Admits plugin rows into the picker catalog once its tables exist, building them admits rows itself
func syncPluginProviderCatalogIfBuilt() {
	catalogMu.Lock()
	defer catalogMu.Unlock()
	if catalogBuilt {
		syncPluginProviderCatalogLocked()
	}
}

func profileValues(ps []*Profile) []Profile {
	out := make([]Profile, len(ps))
	for i, p := range ps {
		out[i] = *p
	}
	return out
}

// ProviderSource names where the current registration of a provider came from, empty when unregistered
func ProviderSource(ctx context.Context, name string) string {
	layer := homeLayerFor(ctx, false)
	layer.mu.Lock()
	canonical := layer.aliases[name]
	layer.mu.Unlock()
	regMu.RLock()
	defer regMu.RUnlock()
	if canonical == "" {
		canonical = name
		if c, ok := regAliases[name]; ok {
			canonical = c
		}
	}
	layer.mu.Lock()
	_, user := layer.registry[canonical]
	layer.mu.Unlock()
	if user {
		return "user"
	}
	return regSources[canonical]
}

// Whether the home's config defines a named custom provider, memoized on the config cache version
func hasNamedCustomProvider(ctx context.Context, name, h, hkey string) bool {
	sig, err := config.ConfigCacheVersionOrNone(ctx, home.ConfigPath(h))
	if err != nil {
		return namedCustomProviderConfiguredFn(ctx, name, h)
	}
	namedCustomMu.Lock()
	memo := namedCustomMem[hkey]
	if memo == nil || !reflect.DeepEqual(memo.sig, sig) {
		memo = &namedCustomMemo{sig: sig, answers: map[string]bool{}}
		namedCustomMem[hkey] = memo
	}
	answer, ok := memo.answers[name]
	namedCustomMu.Unlock()
	if ok {
		return answer
	}
	answer = namedCustomProviderConfiguredFn(ctx, name, h)
	namedCustomMu.Lock()
	memo.answers[name] = answer
	namedCustomMu.Unlock()
	return answer
}

// Config probe behind the named custom memo, swapped by tests counting probes
var namedCustomProviderConfiguredFn func(ctx context.Context, name, h string) bool

// Binds the probe at init since the probe reaches the registry that reads it
func init() { namedCustomProviderConfiguredFn = namedCustomProviderConfigured }

// True when config defines a providers or custom_providers entry matching the request
func namedCustomProviderConfigured(ctx context.Context, name, h string) bool {
	cfg := LoadConfigSafe(h)
	if cfg == nil {
		cfg = map[string]any{}
	}
	return newResolver(ctx, config.Tree(cfg), config.NewSecrets(h), h, nil).getNamedCustomProvider(name, cfg, true) != nil
}

// Reports whether the home's config defines a providers or custom_providers entry for the name
func HasNamedCustomProvider(ctx context.Context, h, name string) bool {
	ctx = ctxForHome(ctx, h)
	return namedCustomProviderConfigured(ctx, name, home.Dir(ctx))
}

// Registered profile for a name or alias in the bound home, layer first, no discovery or rescan
func (layer *homeLayer) lookup(n string) *Profile {
	layer.mu.Lock()
	canonical := layer.aliases[n]
	layer.mu.Unlock()
	regMu.RLock()
	if canonical == "" {
		canonical = n
		if c, ok := regAliases[n]; ok {
			canonical = c
		}
	}
	shared := regProfiles[canonical]
	regMu.RUnlock()
	layer.mu.Lock()
	user := layer.registry[canonical]
	layer.mu.Unlock()
	if user != nil {
		return user
	}
	return shared
}

// Profile for a provider name or alias with lazy discovery, miss rechecks and the custom fallback
func getProviderProfile(ctx context.Context, name string) *Profile {
	if config.SafeWorkerEnabled() {
		return nil
	}
	discoverProviders(ctx)
	layer, h, key := boundHomeLayer(ctx)
	checked := refreshHomeLayer(ctx, layer, h, key, false)
	profile := layer.lookup(name)
	isCustomRoute := strings.HasPrefix(strings.ToLower(name), "custom:")
	if profile == nil && !isCustomRoute && !checked && missRecheckDue(layer, name) {
		if refreshHomeLayer(ctx, layer, h, key, true) {
			profile = layer.lookup(name)
		}
	}
	if profile == nil && isCustomRoute {
		profile = layer.lookup("custom")
	}
	if profile == nil && !isCustomRoute && hasNamedCustomProvider(ctx, name, h, key) {
		profile = layer.lookup("custom")
	}
	return profile
}

// GetProviderProfile looks a provider profile up by name or alias in the bound home
func GetProviderProfile(ctx context.Context, name string) (Profile, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if p := getProviderProfile(ctx, name); p != nil {
		return *p, true
	}
	return Profile{}, false
}

// Whether a miss may force a plugin dir check now, once per name per stamp TTL
func missRecheckDue(layer *homeLayer, name string) bool {
	now := registryMonotonic()
	layer.mu.Lock()
	defer layer.mu.Unlock()
	if last, ok := layer.misses[name]; ok && now.Sub(last) < pluginDirStampTTL {
		return false
	}
	if len(layer.misses) >= 512 {
		layer.misses = map[string]time.Time{}
	}
	layer.misses[name] = now
	return true
}

// Whether the route or the aggregator targeted model's profile rejects image tool parts
func RoutedModelRejectsVisionToolMessages(ctx context.Context, provider, model string) bool {
	providerName := strings.ToLower(strings.TrimSpace(provider))
	if p := getProviderProfile(ctx, providerName); p != nil && !p.VisionToolMessages {
		return true
	}
	if !IsRoutingAggregator(providerName) {
		return false
	}
	target, _, found := strings.Cut(strings.TrimSpace(model), "/")
	if !found || target == "" {
		return false
	}
	tp := getProviderProfile(ctx, strings.ToLower(strings.TrimSpace(target)))
	return tp != nil && !tp.VisionToolMessages
}

// RegistryGeneration counts provider registrations so memos over profile answers can key on it
func RegistryGeneration() int64 { return regGeneration.Load() }

// BoundRegistryKey returns the bound home key and registry generation after the periodic plugin check
func BoundRegistryKey(ctx context.Context) (string, int64) {
	discoverProviders(ctx)
	layer, h, key := boundHomeLayer(ctx)
	refreshHomeLayer(ctx, layer, h, key, false)
	return key, regGeneration.Load()
}

// Host equals the claimed host or is a subdomain of it
func hostClaims(host, claimed string) bool {
	claimed = strings.TrimRight(strings.ToLower(strings.TrimSpace(claimed)), ".")
	return claimed != "" && (host == claimed || strings.HasSuffix(host, "."+claimed))
}

// ProfileForHost returns the registered profile whose hostnames claim the url host, metadata adding metadata hostnames
func ProfileForHost(ctx context.Context, rawURL string, metadata bool) (Profile, bool) {
	if p := profileForHostCtx(ctx, rawURL, metadata); p != nil {
		return *p, true
	}
	return Profile{}, false
}

func profileForHostCtx(ctx context.Context, rawURL string, metadata bool) *Profile {
	if ctx == nil {
		ctx = context.Background()
	}
	host := ""
	if rawURL != "" {
		host = Hostname(rawURL)
	}
	if host == "" || config.SafeWorkerEnabled() {
		return nil
	}
	discoverProviders(ctx)
	layer, h, key := boundHomeLayer(ctx)
	refreshHomeLayer(ctx, layer, h, key, false)
	regMu.RLock()
	shared := int64(len(regProfiles))
	regMu.RUnlock()
	layer.mu.Lock()
	user := int64(len(layer.registry))
	layer.mu.Unlock()
	token := [3]int64{regGeneration.Load(), shared, user}
	mk := hostMemoKey{key, host, metadata}
	hostMemoMu.Lock()
	cached, ok := hostMemo[mk]
	hostMemoMu.Unlock()
	if ok && cached.token == token {
		return cached.profile
	}
	var found *Profile
	for _, p := range listProviders(ctx) {
		hosts := p.Hostnames
		if metadata {
			hosts = append(append([]string(nil), p.Hostnames...), p.MetadataHostnames...)
		}
		for _, claimed := range hosts {
			if hostClaims(host, claimed) {
				found = p
				break
			}
		}
		if found != nil {
			break
		}
	}
	hostMemoMu.Lock()
	hostMemo[mk] = hostMemoEntry{token, found}
	hostMemoMu.Unlock()
	return found
}

// ListProviders returns every registered profile once, the bound home's plugins shadowing process wide ones
func ListProviders(ctx context.Context) []Profile {
	if ctx == nil {
		ctx = context.Background()
	}
	return profileValues(listProviders(ctx))
}

func listProviders(ctx context.Context) []*Profile {
	if config.SafeWorkerEnabled() {
		return nil
	}
	discoverProviders(ctx)
	layer := homeLayerFor(ctx, false)
	regMu.Lock()
	if regListCache == nil {
		seen := map[*Profile]bool{}
		cache := []*Profile{}
		for _, name := range regOrder {
			if p := regProfiles[name]; p != nil && !seen[p] {
				seen[p] = true
				cache = append(cache, p)
			}
		}
		regListCache = cache
	}
	cache := regListCache
	regMu.Unlock()
	layer.mu.Lock()
	defer layer.mu.Unlock()
	result := []*Profile{}
	for _, p := range cache {
		if _, shadowed := layer.registry[p.Name]; !shadowed {
			result = append(result, p)
		}
	}
	seen := map[*Profile]bool{}
	for _, name := range layer.order {
		if p := layer.registry[name]; p != nil && !seen[p] {
			seen[p] = true
			result = append(result, p)
		}
	}
	return result
}

// UnlistedProviderNames returns registered profiles discovery surfaces must not offer, a failing gate hiding its profile
func UnlistedProviderNames(ctx context.Context) map[string]bool {
	if ctx == nil {
		ctx = context.Background()
	}
	hidden := map[string]bool{}
	for _, p := range listProviders(ctx) {
		listed, err := p.Listed(ctx)
		if err != nil {
			slog.Debug(fmt.Sprintf("provider %s listed() raised; hiding it: %v", p.Name, err))
			hidden[p.Name] = true
			continue
		}
		if !listed {
			hidden[p.Name] = true
		}
	}
	return hidden
}

// Resolves a name to the bound home's plugin profile and the process wide profile
type profileResolve func(name string) (user, shared *Profile)

// BoundProfileResolver resolves names against the home bound now, only the periodic plugin check runs
func boundProfileResolver(ctx context.Context) profileResolve {
	var layer *homeLayer
	if discoveringNow() {
		layer, _, _ = boundHomeLayer(ctx)
	} else {
		layer = homeLayerFor(ctx, false)
	}
	return func(name string) (*Profile, *Profile) {
		regMu.RLock()
		shared := name
		if c, ok := regAliases[name]; ok {
			shared = c
		}
		sharedProfile := regProfiles[shared]
		regMu.RUnlock()
		layer.mu.Lock()
		defer layer.mu.Unlock()
		target := layer.aliases[name]
		if target == "" {
			target = shared
		}
		return layer.registry[target], sharedProfile
	}
}

// The bound home's layer after the plugin dir check
func homeLayerFor(ctx context.Context, force bool) *homeLayer {
	layer, h, key := boundHomeLayer(ctx)
	refreshHomeLayer(ctx, layer, h, key, force)
	return layer
}

// RefreshBoundHome rescans the bound home's plugin dirs now and forgets its remembered misses
func RefreshBoundHome(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	layer := homeLayerFor(ctx, true)
	layer.mu.Lock()
	layer.misses = map[string]time.Time{}
	layer.mu.Unlock()
}

func boundHomeLayer(ctx context.Context) (*homeLayer, string, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	h := home.Dir(ctx)
	key := home.Key(h)
	homeLayersMu.Lock()
	defer homeLayersMu.Unlock()
	layer := homeLayers[key]
	if layer == nil {
		layer = newHomeLayer()
		homeLayers[key] = layer
	}
	return layer, h, key
}

// Rescans the home's plugin dirs when their stamps moved, true when the check ran
func refreshHomeLayer(ctx context.Context, layer *homeLayer, h, key string, force bool) bool {
	now := registryMonotonic()
	layer.mu.Lock()
	due := force || layer.stampCheckedAt.IsZero() || now.Sub(layer.stampCheckedAt) >= pluginDirStampTTL
	layer.mu.Unlock()
	if h == "" || config.SafeWorkerEnabled() || !due {
		return false
	}
	if !layer.scanMu.TryLock() {
		return false
	}
	stamps := pluginDirStampsFn(h)
	layer.mu.Lock()
	changed := !reflect.DeepEqual(stamps, layer.stamps)
	layer.mu.Unlock()
	if changed {
		settled := pluginDirStampsSettled(stamps)
		scanHomeLayerFn(ctx, layer, h, key)
		layer.mu.Lock()
		if settled {
			layer.stamps = stamps
		} else {
			layer.stamps = nil
		}
		layer.stampCheckedAt = now
		layer.mu.Unlock()
		layer.scanMu.Unlock()
		if regDiscovered.Load() && !discoveringNow() {
			syncAuthRegistry(ctx)
		}
	} else {
		layer.scanMu.Unlock()
	}
	layer.mu.Lock()
	layer.stampCheckedAt = now
	layer.mu.Unlock()
	return true
}

// Modification times of the plugins dir and its model providers dir, minus one for a missing dir
func pluginDirStamps(h string) []int64 {
	stamp := func(path string) int64 {
		st, err := os.Stat(path)
		if err != nil {
			return -1
		}
		return st.ModTime().UnixNano()
	}
	return []int64{stamp(home.PluginsDir(h)), stamp(filepath.Join(home.PluginsDir(h), "model-providers"))}
}

// Stamps older than the widest mtime tick, so a later change in the same tick cannot hide behind them
func pluginDirStampsSettled(stamps []int64) bool {
	now := registryWallNanos()
	for _, s := range stamps {
		if s == -1 {
			continue
		}
		d := now - s
		if d < 0 {
			d = -d
		}
		if d < pluginDirMtimeTickNS {
			return false
		}
	}
	return true
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// Whether a general plugin dir's manifest declares kind model-provider
func declaresModelProviderKind(pluginDir string) bool {
	for _, filename := range []string{"plugin.yaml", "plugin.yml"} {
		manifest := filepath.Join(pluginDir, filename)
		st, err := os.Stat(manifest)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(manifest)
		if err != nil {
			return false
		}
		text := strings.ToValidUTF8(string(raw), "�")
		if data, err := config.FastSafeLoad([]byte(text)); err == nil {
			if m, ok := data.(map[string]any); ok {
				kind, present := m["kind"]
				if !present {
					kind = ""
				}
				return strings.TrimSpace(pyScalarStr(kind)) == "model-provider"
			}
		}
		for _, line := range strings.Split(strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text), "\n") {
			stripped := strings.TrimSpace(line)
			if strings.HasPrefix(stripped, "#") || !strings.Contains(stripped, ":") {
				continue
			}
			k, v, _ := strings.Cut(stripped, ":")
			if strings.TrimSpace(k) == "kind" {
				return strings.Trim(strings.TrimSpace(v), "\"'") == "model-provider"
			}
		}
		return false
	}
	return false
}

// Whether a plugin manifest declares the model provider kind, the exported form plugins reach through Services
func DeclaresModelProviderKind(pluginDir string) bool { return declaresModelProviderKind(pluginDir) }

// Python str of a YAML scalar for the manifest and isolation comparisons
func pyScalarStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(v)
}

// Imports every plugin dir of the home into its layer, user providers first then installed model-provider kinds
func scanHomeLayer(ctx context.Context, layer *homeLayer, h, key string) {
	regScanDepth.Add(1)
	defer regScanDepth.Add(-1)
	userDir := filepath.Join(home.PluginsDir(h), "model-providers")
	if isDir(userDir) {
		for _, child := range sortedChildDirs(userDir) {
			name := filepath.Base(child)
			if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
				continue
			}
			importPluginDir(ctx, layer, child, "user", key)
		}
	}
	installedDir := home.PluginsDir(h)
	if isDir(installedDir) {
		for _, child := range sortedChildDirs(installedDir) {
			name := filepath.Base(child)
			if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") || name == "model-providers" {
				continue
			}
			if declaresModelProviderKind(child) {
				importPluginDir(ctx, layer, child, "user", key)
			}
		}
	}
}

// Child directories of dir sorted by path, symlinked directories included
func sortedChildDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if isDir(path) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// Module name a home plugin imports under, unique per home
func userModuleName(pluginDir, homeKey string) string {
	sum := sha1.Sum([]byte(homeKey))
	return "_icebath_user_provider_" + hex.EncodeToString(sum[:])[:10] + "_" + strings.ReplaceAll(filepath.Base(pluginDir), "-", "_")
}

// Loads one home plugin dir through the plugin host into the layer, once per module
func importPluginDir(ctx context.Context, layer *homeLayer, pluginDir, source, homeKey string) {
	if _, err := os.Stat(filepath.Join(pluginDir, "__init__.py")); err != nil {
		return
	}
	moduleName := userModuleName(pluginDir, homeKey)
	layer.mu.Lock()
	done := layer.hosted[moduleName]
	layer.mu.Unlock()
	if done {
		return
	}
	hosted := isolationMode(LoadConfigSafe(home.Dir(ctx))) == pluginIsolationHost
	profiles, err := loadExternalProviderPluginFn(ctx, pluginDir, source, homeKey)
	for _, p := range profiles {
		registerIntoLayer(layer, p)
	}
	if err != nil {
		if hosted {
			slog.Warn(fmt.Sprintf("Failed to load user provider plugin %s in the plugin host: %s", filepath.Base(pluginDir), err))
		} else {
			slog.Warn(fmt.Sprintf("Failed to load %s provider plugin %s: %s", source, filepath.Base(pluginDir), err))
		}
		noteLoadResult(layer, pluginDir, err)
		return
	}
	if !hosted {
		noteLoadResult(layer, pluginDir, nil)
	}
	layer.mu.Lock()
	layer.hosted[moduleName] = true
	layer.mu.Unlock()
}

// Records or clears a home plugin's import error on the layer importing it
func noteLoadResult(layer *homeLayer, pluginDir string, err error) {
	layer.mu.Lock()
	defer layer.mu.Unlock()
	if err == nil {
		delete(layer.errors, filepath.Base(pluginDir))
		return
	}
	layer.errors[filepath.Base(pluginDir)] = goErrorTypeName(err) + ": " + err.Error()
}

// PluginLoadError returns the import error a home plugin dir raised, empty when it loaded
func PluginLoadError(ctx context.Context, plugin string) string {
	if ctx == nil {
		ctx = context.Background()
	}
	layer := homeLayerFor(ctx, false)
	layer.mu.Lock()
	defer layer.mu.Unlock()
	return layer.errors[plugin]
}

// Plugin isolation mode from config, in process unless plugins.isolation names the host
func isolationMode(cfg map[string]any) string {
	if os.Getenv("ICEBATH_PLUGIN_HOST_PROCESS") == "1" {
		return pluginIsolationInPro
	}
	plugins, _ := cfg["plugins"].(map[string]any)
	raw, ok := plugins["isolation"]
	if !ok || raw == nil || raw == "" || raw == false {
		return pluginIsolationInPro
	}
	mode := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(pyScalarStr(raw))), "-", "_")
	if mode != pluginIsolationHost && mode != pluginIsolationInPro {
		slog.Warn(fmt.Sprintf("plugins.isolation=%s is not one of host, in_process; loading plugins in-process", pyRepr(raw)))
		return pluginIsolationInPro
	}
	return mode
}

// Reason an in process import of third party code must not happen, empty when allowed
func inProcessImportRefusal(cfg map[string]any, what string) string {
	if isolationMode(cfg) != pluginIsolationHost {
		return ""
	}
	return what + " runs only in-process, and plugins.isolation is 'host' (third-party plugin code " +
		"never runs inside the icebath process); set plugins.isolation: in_process to load it"
}

// ProviderEntryPoint is one icebath_agent.plugins entry point of the PM managed site-packages
type ProviderEntryPoint struct {
	Name  string
	Value string
	Load  func(ctx context.Context) (ProviderEntryPointTarget, error)
}

// ProviderEntryPointTarget is what loading an entry point produced in the plugin host
type ProviderEntryPointTarget struct {
	Profiles          []Profile
	Callable          bool
	RequiresArguments bool
	Invoke            func(ctx context.Context) ([]Profile, error)
}

// Names of a plugins config list as a set, nil when the key is not a list
func pluginsConfigSet(cfg map[string]any, key string) map[string]bool {
	plugins, _ := cfg["plugins"].(map[string]any)
	list, ok := plugins[key].([]any)
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for _, v := range list {
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out
}

// Loads enabled pip entry point provider plugins, logging and skipping each one that fails
func discoverEntryPointProviders(ctx context.Context) {
	cfg := LoadConfigSafe(home.Dir(ctx))
	enabled := pluginsConfigSet(cfg, "enabled")
	disabled := pluginsConfigSet(cfg, "disabled")
	if len(enabled) == 0 {
		return
	}
	eps, err := installedProviderEntryPointsFn(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("entry-point provider scan skipped: %s", err))
		return
	}
	for _, ep := range eps {
		if !enabled[ep.Name] || disabled[ep.Name] {
			slog.Debug(fmt.Sprintf("entry-point provider %s skipped: not enabled in config", pyRepr(ep.Name)))
			continue
		}
		if refusal := inProcessImportRefusal(cfg, "pip-installed model-provider plugin "+pyRepr(ep.Name)); refusal != "" {
			slog.Warn(refusal)
			continue
		}
		target, err := ep.Load(ctx)
		for _, p := range target.Profiles {
			RegisterProvider(p)
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Failed to load entry-point provider plugin %s: %s", pyRepr(ep.Name), err))
			continue
		}
		if !target.Callable {
			continue
		}
		if target.RequiresArguments {
			slog.Debug(fmt.Sprintf("entry-point %s skipped by provider scan: target requires arguments (general plugin owned by PluginManager)", pyRepr(ep.Name)))
			continue
		}
		profiles, err := target.Invoke(ctx)
		for _, p := range profiles {
			RegisterProvider(p)
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("Entry-point provider plugin %s raised on invocation: %s", pyRepr(ep.Name), err))
		}
	}
}

// Every bundled plugin profile in discovery order, the source both discovery and the catalog read
func bundledSource() []Profile {
	var out []Profile
	for _, d := range bundledPluginDirs {
		for _, p := range bundledPluginPort(d.dir)() {
			normalizeRegisteredProfile(&p)
			out = append(out, p)
		}
	}
	return out
}

// BundledProfiles returns every bundled profile once, the last registration of a name winning, without config or discovery
func BundledProfiles() []Profile {
	if config.SafeWorkerEnabled() {
		return nil
	}
	index := map[string]int{}
	var out []Profile
	for _, p := range bundledSource() {
		if i, ok := index[p.Name]; ok {
			out[i] = p
			continue
		}
		index[p.Name] = len(out)
		out = append(out, p)
	}
	return out
}

// Populates the process wide registry once, entry points first and bundled plugins winning collisions
func discoverProviders(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	if config.SafeWorkerEnabled() {
		return
	}
	if regDiscovered.Load() && !regDiscovering.Load() {
		return
	}
	discoveryMu.Lock()
	if regDiscovered.Load() {
		discoveryMu.Unlock()
		return
	}
	regDiscovered.Store(true)
	regDiscovering.Store(true)
	runDiscoveryStepsFn(ctx)
	regDiscovering.Store(false)
	discoveryMu.Unlock()
	syncAuthRegistry(ctx)
}

// The discovery passes in precedence order
func runDiscoverySteps(ctx context.Context) {
	discoverEntryPointProviders(ctx)
	for _, p := range bundledSource() {
		registerBundledProvider(p)
	}
}

// Plugin host loader of a home plugin dir, replaced in tests
var loadExternalProviderPluginFn = loadExternalProviderPlugin

// Installed distribution entry point scan, replaced in tests
var installedProviderEntryPointsFn = installedProviderEntryPoints

// Plugin dir stamp reader, replaced in tests to count directory checks
var pluginDirStampsFn = pluginDirStamps

// Clocks and the home scan behind the plugin dir TTL, swapped by tests driving time by hand
var (
	registryMonotonic = time.Now
	registryWallNanos = func() int64 { return time.Now().UnixNano() }
	scanHomeLayerFn   func(ctx context.Context, layer *homeLayer, h, key string)
)

// Discovery passes, replaced in tests running a cold discovery with their own registrations
var runDiscoveryStepsFn func(ctx context.Context)

// Binds the scan at init since the scan imports plugins that reach the registry
func init() {
	scanHomeLayerFn = scanHomeLayer
	runDiscoveryStepsFn = runDiscoverySteps
}

// Captures the process wide provider registry and returns the closure that restores it and drops the caches built over it
func SnapshotProviderRegistryForTests() func() {
	discoverProviders(context.Background())
	regMu.Lock()
	profiles := map[string]*Profile{}
	for k, v := range regProfiles {
		profiles[k] = v
	}
	order := append([]string(nil), regOrder...)
	aliases := map[string]string{}
	for k, v := range regAliases {
		aliases[k] = v
	}
	sources := map[string]string{}
	for k, v := range regSources {
		sources[k] = v
	}
	regMu.Unlock()
	reg := providerRegistry()
	reg.mu.Lock()
	keys := append([]string(nil), reg.keys...)
	rows := map[string]*Profile{}
	for k, v := range reg.rows {
		rows[k] = v
	}
	pending := reg.syncPending
	reg.mu.Unlock()
	pluginMirroredMu.Lock()
	mirrored := map[string]bool{}
	for k, v := range pluginMirroredProviders {
		mirrored[k] = v
	}
	pluginMirroredMu.Unlock()
	return func() {
		regMu.Lock()
		regProfiles, regOrder, regAliases, regSources, regListCache = profiles, order, aliases, sources, nil
		regMu.Unlock()
		regGeneration.Add(1)
		reg.mu.Lock()
		reg.keys, reg.rows, reg.syncPending = keys, rows, pending
		reg.mu.Unlock()
		pluginMirroredMu.Lock()
		pluginMirroredProviders = mirrored
		pluginMirroredMu.Unlock()
		homeLayersMu.Lock()
		homeLayers = map[string]*homeLayer{}
		homeLayersMu.Unlock()
		homeRowsMu.Lock()
		homeRows = map[[2]*Profile]*Profile{}
		homeRowsMu.Unlock()
		hostMemoMu.Lock()
		hostMemo = map[hostMemoKey]hostMemoEntry{}
		hostMemoMu.Unlock()
		namedCustomMu.Lock()
		namedCustomMem = map[string]*namedCustomMemo{}
		namedCustomMu.Unlock()
		resetCatalogTables()
	}
}
