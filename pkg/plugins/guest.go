package plugins

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Forms a third party plugin takes
const (
	RuntimeWasm    = "wasm"
	RuntimeProgram = "program"
)

// Sentence naming every form icebath loads a plugin in
const PluginFormsText = "icebath loads plugins built in, as WebAssembly modules (runtime: wasm) or as external programs (runtime: program)"

const (
	guestDefaultMemoryMB    = 256
	guestDefaultCallSeconds = 120.0
)

// Form, entry, grants and limits one third party plugin declares in its manifest
type guestSpec struct {
	runtime     string
	entry       []string
	dir         string
	name        string
	memoryPages uint32
	callLimit   time.Duration
	grants      guestGrants
}

// Entry argv a failed start names
func (g *guestSpec) argv() []string { return append([]string{g.runtime + ":"}, g.entry...) }

// Runtime a manifest selects, empty when it declares no third party form
func manifestRuntime(mf *Manifest) string {
	declared := strings.ToLower(strings.TrimSpace(scalarString(mf.Raw["runtime"])))
	if len(mf.Entry) == 0 {
		return ""
	}
	if declared == "" && strings.HasSuffix(strings.ToLower(mf.Entry[0]), ".wasm") {
		return RuntimeWasm
	}
	return declared
}

// Guest description of a manifest, an error naming the field that does not fit
func guestSpecOf(mf *Manifest, profileHome string) (*guestSpec, error) {
	runtime := manifestRuntime(mf)
	if runtime != RuntimeWasm && runtime != RuntimeProgram {
		return nil, fmt.Errorf("plugin '%s' declares runtime %q; %s", mf.Name, runtime, PluginFormsText)
	}
	if runtime == RuntimeWasm && len(mf.Entry) != 1 {
		return nil, fmt.Errorf("plugin '%s' runtime wasm takes one entry, the module file inside the plugin directory", mf.Name)
	}
	limits := asMap(mf.Raw["limits"])
	memoryMB := guestNumber(limits["memory_mb"], guestDefaultMemoryMB)
	if memoryMB < 16 || memoryMB > 4096 {
		return nil, fmt.Errorf("plugin '%s' limits.memory_mb must be between 16 and 4096", mf.Name)
	}
	seconds := guestNumber(limits["call_seconds"], guestDefaultCallSeconds)
	if seconds <= 0 {
		return nil, fmt.Errorf("plugin '%s' limits.call_seconds must be positive", mf.Name)
	}
	spec := &guestSpec{runtime: runtime, entry: mf.Entry, dir: mf.Path, name: mf.Name,
		memoryPages: uint32(memoryMB * 16), callLimit: time.Duration(seconds * float64(time.Second))}
	data := mf.DataNamespace
	if data == "" {
		data = mf.Name
	}
	spec.grants = parseGuestGrants(asMap(mf.Raw["grants"]), guestPathVars{home: profileHome, pluginDir: mf.Path,
		pluginData: filepath.Join(profileHome, "plugin-data", data)})
	return spec, nil
}

// Number a manifest gives for a limit, the default when absent
func guestNumber(v any, def float64) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return def
}

// Every registrar the parent serves for a guest, which reaches each one through its wire callbacks
func guestServedCtxMethods() map[string]bool {
	served := map[string]bool{}
	for name := range HostInProcessObjectBases {
		served[name] = true
	}
	for name := range hostInProcessServed {
		served[name] = true
	}
	return served
}

// Host of one third party plugin, kept for the life of the manager so its restart history survives reloads
func (m *Manager) guestHost(mf *Manifest) (*PluginHost, error) {
	spec, err := guestSpecOf(mf, m.home)
	if err != nil {
		return nil, err
	}
	pluginHostMu.Lock()
	defer pluginHostMu.Unlock()
	key := mf.ID()
	host := m.guestHosts[key]
	if host == nil {
		host, err = newPluginHost(m)
		if err != nil {
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

// Hosts of the third party plugins of a manager, sorted by plugin key
func (m *Manager) GuestHosts() []*PluginHost {
	pluginHostMu.Lock()
	defer pluginHostMu.Unlock()
	keys := make([]string, 0, len(m.guestHosts))
	for key := range m.guestHosts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hosts := make([]*PluginHost, 0, len(keys))
	for _, key := range keys {
		hosts = append(hosts, m.guestHosts[key])
	}
	return hosts
}

// Starts the guest of a manifest, runs its register function onto pctx and stops the guest when the plugin unloads
func (m *Manager) loadGuest(ctx context.Context, mf *Manifest, pctx *Context) (string, error) {
	host, err := m.guestHost(mf)
	if err != nil {
		return "", err
	}
	if _, err := host.Load(ctx, mf, pctx, "", false, false); err != nil {
		host.Shutdown()
		return "", err
	}
	_, err = pctx.OnUnload(func() error {
		host.Shutdown()
		return nil
	})
	return filepath.Join(mf.Path, mf.Entry[0]), err
}

// Starts the guest in the form its manifest selects
func (h *PluginHost) launchGuest(ctx context.Context, s *hostStart) (*Channel, *hostProcess, error) {
	h.mu.Lock()
	spec := h.guest
	h.mu.Unlock()
	if spec.runtime == RuntimeWasm {
		return h.launchWasm(ctx, s, spec)
	}
	return h.launchProgram(s, spec)
}

// Installs a started guest as the current child unless a shutdown abandoned the start
func (h *PluginHost) installGuest(s *hostStart, child *hostProcess, reader io.ReadCloser, writer io.WriteCloser, name string) (*Channel, error) {
	h.mu.Lock()
	if h.starting != s {
		h.mu.Unlock()
		_ = reader.Close()
		_ = writer.Close()
		return nil, fmt.Errorf("shutdown")
	}
	h.generation++
	generation := h.generation
	ch := newChannel(reader, writer, h.handle, name, func(reason string) { h.onChannelClose(generation, reason) }, h.contextForOrigin)
	ch.notifyCancel = true
	h.proc, h.channel = child, ch
	h.releases = nil
	h.mu.Unlock()
	ch.Start()
	return ch, nil
}

// Path of a guest entry inside its plugin directory, refused when it leaves the directory
func guestEntryPath(spec *guestSpec) (string, error) {
	entry := filepath.Join(spec.dir, filepath.FromSlash(spec.entry[0]))
	rel, err := filepath.Rel(spec.dir, entry)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(spec.entry[0]) {
		return "", fmt.Errorf("plugin '%s' entry %q must be a file inside the plugin directory", spec.name, spec.entry[0])
	}
	return entry, nil
}

// Directory the compiled code of WebAssembly plugins is cached under
func guestCacheDir() string {
	return filepath.Join(home.CacheDir(home.Root()), "plugin-wasm")
}
