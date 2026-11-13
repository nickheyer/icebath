package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Provider kinds a guest plugin directory declares in provides, mapped to the registrar its provider load captures
var guestProviderRegistrars = map[string]string{
	"memory": "register_memory_provider", "context_engine": "register_context_engine", "cron_scheduler": "register_cron_scheduler",
	"computer_use": "register_computer_use_provider", "model_provider": "register_model_provider", "gateway_hook": "register_gateway_hook",
}

// Provider kinds a category discovery owns, a plugin supplying one of them loading only through that discovery
var exclusiveProviderKinds = map[string]bool{"memory": true, "cron_scheduler": true, "computer_use": true}

// Whether a runtime names one of the guest forms
func isGuestRuntime(runtime string) bool { return runtime == RuntimeWasm || runtime == RuntimeProgram }

// Manifest of a plugin directory whose plugin.yaml declares a WebAssembly or program runtime, nil otherwise
func GuestManifest(dir string) *Manifest {
	for _, name := range []string{"plugin.yaml", "plugin.yml"} {
		if file := filepath.Join(dir, name); isFile(file) {
			return guestManifestFile(file, dir)
		}
	}
	return nil
}

// Guest manifest a YAML file of a directory declares, nil when it names no guest runtime
func guestManifestFile(file, dir string) *Manifest {
	mf := ParseManifestFile(file, dir, "user", "")
	if mf == nil || !isGuestRuntime(manifestRuntime(mf)) {
		return nil
	}
	return mf
}

// Whether the manifest supplies a provider kind, by its provides list or by the model provider kind
func (m *Manifest) Supplies(kind string) bool {
	if kind == "model_provider" && m.Kind == "model-provider" {
		return true
	}
	for _, p := range m.Provides {
		if p == kind {
			return true
		}
	}
	return false
}

// Sorted child directories of root holding a guest plugin that supplies kind, dot and underscore names skipped
func GuestProviderDirs(root, kind string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		dir := filepath.Join(root, name)
		if !isDir(dir) {
			continue
		}
		if mf := GuestManifest(dir); mf != nil && mf.Supplies(kind) {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out
}

// Guest of a category plugin directory, one per directory for the life of the manager
func (m *Manager) categoryGuestHost(mf *Manifest) (*PluginHost, error) {
	spec, err := guestSpecOf(mf, m.home)
	if err != nil {
		return nil, err
	}
	pluginHostMu.Lock()
	defer pluginHostMu.Unlock()
	key := "dir:" + mf.Path
	host := m.guestHosts[key]
	if host == nil {
		if host, err = newPluginHost(m); err != nil {
			return nil, err
		}
		if m.guestHosts == nil {
			m.guestHosts = map[string]*PluginHost{}
		}
		m.guestHosts[key] = host
	}
	host.mu.Lock()
	host.guest = spec
	host.mu.Unlock()
	return host, nil
}

// One provider load of a guest plugin directory, loaded again into a fresh guest after the old one exits
type guestInstance struct {
	host    *PluginHost
	mf      *Manifest
	capture string
	forward bool
	pctx    *Context
	before  func(context.Context)
	loadMu  sync.Mutex
	mu      sync.Mutex
	gen     int
	items   []map[string]any
}

// Provider load of a guest manifest that captures the registrar of kind, pctx serving every other context call
func (m *Manager) guestProviderLoad(mf *Manifest, kind string, pctx *Context, forward bool, before func(context.Context)) (*guestInstance, error) {
	capture, ok := guestProviderRegistrars[kind]
	if !ok {
		return nil, fmt.Errorf("unknown guest provider kind %q", kind)
	}
	if reason := RequiresVersionError(mf.RequiresIcebath); reason != "" {
		return nil, fmt.Errorf("plugin '%s' %s", mf.Name, reason)
	}
	host, err := m.categoryGuestHost(mf)
	if err != nil {
		return nil, err
	}
	if pctx == nil {
		pctx = newContext(mf, m)
	}
	return &guestInstance{host: host, mf: mf, capture: capture, forward: forward, pctx: pctx, before: before}, nil
}

// Plugin key the provider load registers its context under in the guest
func (g *guestInstance) key() string { return g.capture + ":" + g.mf.Path }

// Runs the provider load in the guest, every captured object ref stamped with the guest generation
func (g *guestInstance) load(ctx context.Context) ([]map[string]any, int, error) {
	h := g.host
	ch, err := h.ensureStarted(ctx)
	if err != nil {
		return nil, 0, err
	}
	key := g.key()
	h.stateMu.Lock()
	h.contexts[key] = g.pctx
	g.pctx.hostInProcess = guestServedCtxMethods()
	h.loading = key
	h.stateMu.Unlock()
	params := map[string]any{"plugin_key": key, "plugin_id": g.pctx.PluginID(), "name": g.mf.Name, "path": g.mf.Path,
		"profile_name": g.pctx.ProfileName(h.baseCtx), "manifest": hostManifestInfo(g.mf), "capture": g.capture, "forward": g.forward}
	raw, err := ch.Call(ctx, "load_instance", params, 0)
	h.stateMu.Lock()
	h.loading = ""
	if err != nil {
		delete(h.contexts, key)
	}
	h.stateMu.Unlock()
	if err != nil {
		return nil, 0, h.unavailableAsExit(err)
	}
	h.mu.Lock()
	gen := h.generation
	h.mu.Unlock()
	reply, _ := raw.(map[string]any)
	items := []map[string]any{}
	for _, entry := range listOrEmpty(reply["captured"]) {
		item, _ := entry.(map[string]any)
		if item == nil {
			continue
		}
		if ref, ok := item["value"].(map[string]any); ok {
			if _, isObject := ref["__object__"]; isObject {
				ref["generation"] = int64(gen)
			}
		}
		items = append(items, item)
	}
	g.mu.Lock()
	g.items, g.gen = items, gen
	g.mu.Unlock()
	return items, gen, nil
}

// Captured items of the running guest, loading the plugin again when the guest that captured them exited
func (g *guestInstance) current(ctx context.Context) ([]map[string]any, int, error) {
	g.loadMu.Lock()
	defer g.loadMu.Unlock()
	g.mu.Lock()
	items, gen := g.items, g.gen
	g.mu.Unlock()
	h := g.host
	h.mu.Lock()
	live := items != nil && gen == h.generation && h.aliveLocked()
	h.mu.Unlock()
	if live {
		return items, gen, nil
	}
	if items != nil {
		if h.restartRefused(fmt.Sprintf("%s plugin %s", g.capture, g.mf.Name)) {
			return nil, 0, &PluginHostUnavailable{Reason: h.exitReason()}
		}
		if g.before != nil {
			g.before(ctx)
		}
	}
	return g.load(ctx)
}

// Wire value of the first captured item of a ref kind, nil when the plugin registered none
func firstCaptured(items []map[string]any, refKey string) map[string]any {
	for _, item := range items {
		if ref, ok := item["value"].(map[string]any); ok {
			if _, has := ref[refKey]; has {
				return ref
			}
		}
	}
	return nil
}

// First provider object the load captured, a proxy that reloads with the instance, nil when the plugin registered none
func (g *guestInstance) object(ctx context.Context, base string) (*HostedObject, error) {
	items, _, err := g.current(ctx)
	if err != nil {
		return nil, err
	}
	ref := firstCaptured(items, "__object__")
	if ref == nil {
		return nil, nil
	}
	reload := func(ctx context.Context) (map[string]any, error) {
		items, _, err := g.current(ctx)
		if err != nil {
			return nil, err
		}
		if fresh := firstCaptured(items, "__object__"); fresh != nil {
			return fresh, nil
		}
		return nil, &PluginHostUnavailable{Reason: fmt.Sprintf("plugin '%s' no longer registers a provider through %s", g.mf.Name, g.capture)}
	}
	return g.host.objectProxy(ref, base, reload), nil
}

// First callback the load captured in the running guest, nil when the plugin registered none
func (g *guestInstance) callable(ctx context.Context) (*HostedCallable, error) {
	items, _, err := g.current(ctx)
	if err != nil {
		return nil, err
	}
	ref := firstCaptured(items, "__callable__")
	if ref == nil {
		return nil, nil
	}
	return g.host.callableProxy(ref), nil
}

// Loads the provider of kind a guest plugin directory registers, nil when it registers none
func LoadGuestProvider(ctx context.Context, dir, kind, base string, pctx *Context, forward bool, before func(context.Context)) (*HostedObject, error) {
	mf, err := requireGuestManifest(dir)
	if err != nil {
		return nil, err
	}
	inst, err := GetPluginManager(ctx).guestProviderLoad(mf, kind, pctx, forward, before)
	if err != nil {
		return nil, err
	}
	return inst.object(ctx, base)
}

// Guest manifest of a plugin directory, an error naming the forms icebath loads when it declares none
func requireGuestManifest(dir string) (*Manifest, error) {
	if mf := GuestManifest(dir); mf != nil {
		return mf, nil
	}
	return nil, fmt.Errorf("%s declares no runtime: wasm or runtime: program plugin; %s", dir, PluginFormsText)
}

// Starts the guest of a plugin directory ahead of its first provider load
func StartGuestPlugin(ctx context.Context, dir string) error {
	mf, err := requireGuestManifest(dir)
	if err != nil {
		return err
	}
	host, err := GetPluginManager(ctx).categoryGuestHost(mf)
	if err != nil {
		return err
	}
	_, err = host.ensureStarted(ctx)
	return err
}

const cronSchedulerBase = "cron.scheduler_provider:CronScheduler"

// Loads the cron scheduler a guest plugin directory registers, nil when it registers none
func LoadGuestCronScheduler(ctx context.Context, dir string) (*HostedObject, error) {
	return LoadGuestProvider(ctx, dir, "cron_scheduler", cronSchedulerBase, nil, false, nil)
}

// Forwards a dashboard request to the guest of the plugin installed at pluginDir, loading the profile plugins first
func ServeGuestDashboard(ctx context.Context, pluginDir string, req DashboardRequest, timeout time.Duration) (*DashboardResponse, error) {
	DiscoverPlugins(ctx, false)
	m := GetPluginManager(ctx)
	want := resolveExisting(pluginDir)
	m.mu.RLock()
	var key string
	for k, loaded := range m.plugins {
		if loaded.Enabled && loaded.Manifest.Path != "" && resolveExisting(loaded.Manifest.Path) == want {
			key = k
			break
		}
	}
	m.mu.RUnlock()
	if key == "" {
		return nil, fmt.Errorf("the plugin at %s is not loaded in this profile", pluginDir)
	}
	pluginHostMu.Lock()
	host := m.guestHosts[key]
	pluginHostMu.Unlock()
	if host == nil {
		return nil, fmt.Errorf("the plugin at %s runs no guest", pluginDir)
	}
	return host.ServeDashboard(ctx, key, req, timeout)
}
