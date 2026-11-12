package plugins

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyctx"
	"github.com/nickheyer/icebath/pkg/tools"
)

type ledgerPlatformEntry struct {
	Name, Label string
	Factory     any
}

// Scope keyed platform registry double holding the gateway snapshot, restore and deferred resolution contract
type ledgerPlatforms struct {
	mu    sync.Mutex
	slots map[[2]string]PlatformSlot
}

func (r *ledgerPlatforms) CoreShipsPlatform(string) bool { return false }
func (r *ledgerPlatforms) NewEntry(spec PlatformSpec) (any, error) {
	return &ledgerPlatformEntry{Name: spec.Name, Label: spec.Label, Factory: spec.AdapterFactory}, nil
}
func (r *ledgerPlatforms) Snapshot(name, scope string) PlatformSlot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.slots[[2]string{scope, name}]
}
func (r *ledgerPlatforms) Register(entry any, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slots[[2]string{scope, entry.(*ledgerPlatformEntry).Name}] = PlatformSlot{Entry: entry}
}
func (r *ledgerPlatforms) RegisterDeferred(name string, loader *DeferredPlatformLoader, scope string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.slots[[2]string{scope, name}] = PlatformSlot{Loader: loader}
}
func (r *ledgerPlatforms) NoteActivationEnv(string, []string)       {}
func (r *ledgerPlatforms) IsDeferredLoadCancelled(_, _ string) bool { return false }
func (r *ledgerPlatforms) Restore(name string, current, previous PlatformSlot, scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := [2]string{scope, name}
	if r.slots[k] != current {
		return false
	}
	if previous == (PlatformSlot{}) {
		delete(r.slots, k)
	} else {
		r.slots[k] = previous
	}
	return true
}
func (r *ledgerPlatforms) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k := range r.slots {
		if k[1] == name {
			delete(r.slots, k)
		}
	}
}

// Entry of a scope slot, materializing a deferred loader first the way the gateway lookup does
func (r *ledgerPlatforms) get(ctx context.Context, name, scope string) *ledgerPlatformEntry {
	slot := r.Snapshot(name, scope)
	if slot.Entry == nil && slot.Loader != nil {
		slot.Loader.Load(ctx)
		slot = r.Snapshot(name, scope)
	}
	e, _ := slot.Entry.(*ledgerPlatformEntry)
	return e
}

type ledgerProvider struct{ name, marker string }

// Provider registry double with a process global base under scope overlays, the provider registry contract
type ledgerProviders struct {
	mu     sync.Mutex
	local  map[[2]string]any
	global map[string]any
	pause  func(p *ledgerProvider)
}

func (r *ledgerProviders) Accepts(p any) bool { _, ok := p.(*ledgerProvider); return ok }
func (r *ledgerProviders) ProviderName(p any) string {
	if lp, ok := p.(*ledgerProvider); ok {
		return lp.name
	}
	return ""
}
func (r *ledgerProviders) Identity(any) error { return nil }
func (r *ledgerProviders) Snapshot(name, scope string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.local[[2]string{scope, name}]
}
func (r *ledgerProviders) Register(p any, scope string) (bool, error) {
	lp := p.(*ledgerProvider)
	r.mu.Lock()
	r.local[[2]string{scope, strings.TrimSpace(lp.name)}] = p
	pause := r.pause
	r.mu.Unlock()
	if pause != nil {
		pause(lp)
	}
	return true, nil
}
func (r *ledgerProviders) Restore(name string, current, replacement any, scope string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := [2]string{scope, name}
	if r.local[k] != current {
		return false
	}
	if replacement == nil {
		delete(r.local, k)
	} else {
		r.local[k] = replacement
	}
	return true
}
func (r *ledgerProviders) registerGlobal(p *ledgerProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.global[p.name] = p
}
func (r *ledgerProviders) get(name, scope string) any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.local[[2]string{scope, name}]; ok {
		return v
	}
	return r.global[name]
}

// Installs the platform and image provider doubles for one test, restored at cleanup
func installLedgerServices(t *testing.T) (*ledgerPlatforms, *ledgerProviders) {
	t.Helper()
	platforms := &ledgerPlatforms{slots: map[[2]string]PlatformSlot{}}
	providers := &ledgerProviders{local: map[[2]string]any{}, global: map[string]any{}}
	previous := installedServices()
	InstallServices(&Services{Platforms: platforms, ProviderRegistries: map[string]ScopedRegistry{"image_gen_provider": providers},
		BuiltinAuxTaskKeys: func() []string { return []string{"vision", "web_extract", "compression"} }})
	t.Cleanup(func() { InstallServices(previous) })
	return platforms, providers
}

func ledgerSchema(name string) map[string]any {
	return map[string]any{"name": name, "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
}

func ledgerContext(name string, m *Manager) *Context {
	return newContext(&Manifest{Name: name, Key: name}, m)
}

// Replaces the bundled plugin table with the given plugins for one test
func ledgerSwapBundled(t *testing.T, plugins ...Plugin) {
	t.Helper()
	bundledMu.Lock()
	savedByKey, savedOrder := bundledByKey, bundledOrder
	bundledByKey, bundledOrder = map[string]Plugin{}, nil
	bundledMu.Unlock()
	t.Cleanup(func() {
		bundledMu.Lock()
		bundledByKey, bundledOrder = savedByKey, savedOrder
		bundledMu.Unlock()
	})
	for _, p := range plugins {
		RegisterBundled(p)
	}
}

type ledgerBundled struct {
	m        Manifest
	register func(ctx *Context) error
}

func (f ledgerBundled) Manifest() Manifest          { return f.m }
func (f ledgerBundled) Register(ctx *Context) error { return f.register(ctx) }

// Compiled-in plugin driving every registrar kind of the ledger, the Go twin of the hermes ledger_probe
func ledgerProbePlugin(unloaded *[]string, mu *sync.Mutex) Plugin {
	return ledgerBundled{m: Manifest{Name: "ledger_probe", Version: "0.1.0", Description: "ownership ledger probe"}, register: func(ctx *Context) error {
		bg := context.Background()
		if _, err := ctx.RegisterTool(bg, ToolSpec{Name: "ledger_probe_tool", Toolset: "plugin_ledger_probe", Schema: ledgerSchema("ledger_probe_tool"),
			Handler: func(context.Context, *tools.Call) string { return "ledger" }}); err != nil {
			return err
		}
		if _, err := ctx.RegisterPlatform(PlatformSpec{Name: "ledger_probe_platform", Label: "Ledger probe",
			AdapterFactory: func(any) any { return struct{}{} }, Check: func() bool { return true }}); err != nil {
			return err
		}
		ctx.RegisterCLICommand("ledger-probe-cli", "Ledger CLI", func(any) {}, func(any) error { return nil }, "")
		ctx.RegisterCommand("ledger-probe-command", func(_ context.Context, args string) (any, error) { return args, nil }, "Ledger command", "", "")
		ctx.RegisterHook("pre_tool_call", func(context.Context, map[string]any) (any, error) { return map[string]any{"hook": "ledger"}, nil }, false)
		ctx.RegisterMiddleware("tool_request", func(map[string]any) (any, error) { return map[string]any{"middleware": "ledger"}, nil })
		if _, err := ctx.RegisterAuxiliaryTask("ledger_probe_task", AuxTaskOptions{DisplayName: "Ledger probe task", Description: "Ledger task"}); err != nil {
			return err
		}
		skill := filepath.Join(ctx.manager.home, "ledger_probe_SKILL.md")
		if err := os.WriteFile(skill, []byte("# Ledger probe\n"), 0o644); err != nil {
			return err
		}
		if _, err := ctx.RegisterSkill("ledger-probe", skill, "Ledger skill", nil); err != nil {
			return err
		}
		if _, err := ctx.RegisterSystemPromptSection("ledger-probe-section", "ledger probe section content"); err != nil {
			return err
		}
		if err := ctx.RegisterApprovalTransport(bg, "ledger_probe_transport", func(context.Context, tools.ApprovalRequest) (tools.ApprovalTransportDecision, error) {
			return tools.ApprovalTransportDecision{Choice: "deny"}, nil
		}); err != nil {
			return err
		}
		_, err := ctx.OnUnload(func() error { mu.Lock(); *unloaded = append(*unloaded, "ledger_probe"); mu.Unlock(); return nil })
		return err
	}}
}

// Port of test_load_force_reload_and_unload_remove_every_manager_registration
func TestLoadForceReloadAndUnloadRemoveEveryManagerRegistration(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	platforms, _ := installLedgerServices(t)
	var unloaded []string
	var mu sync.Mutex
	ledgerSwapBundled(t, ledgerProbePlugin(&unloaded, &mu))
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [ledger_probe]\n")
	ctx := homeCtx(h)
	m := NewManager(h)
	m.DiscoverAndLoad(ctx, false)
	if e := loadedError(m, "ledger_probe"); e != "" {
		t.Fatalf("load error %s", e)
	}
	firstTool := tools.Default.Lookup(ctx, h, "ledger_probe_tool")
	firstPlatform := platforms.get(ctx, "ledger_probe_platform", h)
	m.mu.RLock()
	firstHook := m.hooks["pre_tool_call"][0]
	firstMiddleware := m.middleware["tool_request"][0]
	firstCommand := m.pluginCommands["ledger-probe-command"]
	firstCLI := m.cliCommands["ledger-probe-cli"]
	firstSkill := m.pluginSkills["ledger_probe:ledger-probe"]
	moduleName := m.plugins["ledger_probe"].Module
	kinds := map[string]bool{}
	for _, r := range m.ownershipLedger["ledger_probe"] {
		kinds[r.Kind] = true
	}
	_, hasSection := m.systemPromptSections["ledger-probe-section"]
	_, hasTransport := m.approvalTransports["ledger_probe_transport"]
	m.mu.RUnlock()
	if firstTool == nil || firstPlatform == nil {
		t.Fatalf("tool %v platform %v", firstTool, firstPlatform)
	}
	want := []string{"approval_transport", "auxiliary_task", "cli_command", "command", "hook", "middleware", "on_unload", "platform", "skill", "system_prompt_section", "tool", "tool_override_policy"}
	if got := sortedKeys(kinds); !reflect.DeepEqual(got, want) {
		t.Fatalf("ledger kinds %v", got)
	}
	if !hasSection || !hasTransport {
		t.Fatalf("section %v transport %v", hasSection, hasTransport)
	}

	m.DiscoverAndLoad(ctx, true)
	secondTool := tools.Default.Lookup(ctx, h, "ledger_probe_tool")
	secondPlatform := platforms.get(ctx, "ledger_probe_platform", h)
	if secondTool == nil || secondTool == firstTool || secondPlatform == nil || secondPlatform == firstPlatform {
		t.Fatalf("reload kept the first registrations: tool %p %p platform %p %p", firstTool, secondTool, firstPlatform, secondPlatform)
	}
	m.mu.RLock()
	if len(m.hooks["pre_tool_call"]) != 1 || m.hooks["pre_tool_call"][0] == firstHook {
		t.Fatalf("hooks %v", m.hooks["pre_tool_call"])
	}
	if len(m.middleware["tool_request"]) != 1 || m.middleware["tool_request"][0] == firstMiddleware {
		t.Fatalf("middleware %v", m.middleware["tool_request"])
	}
	if m.pluginCommands["ledger-probe-command"] == firstCommand || m.cliCommands["ledger-probe-cli"] == firstCLI || m.pluginSkills["ledger_probe:ledger-probe"] == firstSkill {
		t.Fatal("command, cli command or skill kept its first registration")
	}
	if len(m.auxTasks) != 1 {
		t.Fatalf("aux tasks %v", m.auxTasks)
	}
	m.mu.RUnlock()

	if !m.Unload("ledger_probe") {
		t.Fatal("unload found nothing")
	}
	if tools.Default.Lookup(ctx, h, "ledger_probe_tool") != nil || platforms.Snapshot("ledger_probe_platform", h) != (PlatformSlot{}) {
		t.Fatal("tool or platform survived unload")
	}
	m.mu.RLock()
	_, hooked := m.hooks["pre_tool_call"]
	_, mid := m.middleware["tool_request"]
	_, cmd := m.pluginCommands["ledger-probe-command"]
	_, cli := m.cliCommands["ledger-probe-cli"]
	_, sk := m.pluginSkills["ledger_probe:ledger-probe"]
	_, sec := m.systemPromptSections["ledger-probe-section"]
	_, tr := m.approvalTransports["ledger_probe_transport"]
	_, pl := m.plugins["ledger_probe"]
	leftovers := len(m.auxTasks) + len(m.ownershipLedger)
	m.mu.RUnlock()
	if hooked || mid || cmd || cli || sk || sec || tr || pl || leftovers != 0 {
		t.Fatalf("leftovers hook %v middleware %v command %v cli %v skill %v section %v transport %v plugin %v aux+ledger %d", hooked, mid, cmd, cli, sk, sec, tr, pl, leftovers)
	}
	mu.Lock()
	if !reflect.DeepEqual(unloaded, []string{"ledger_probe", "ledger_probe"}) {
		t.Fatalf("on_unload callbacks %v", unloaded)
	}
	mu.Unlock()
	if tools.Default.SnapshotPluginOverridePolicy(moduleName, h) != nil {
		t.Fatal("tool override policy survived unload")
	}
	if scope, err := tools.Default.PluginScopeForModule(h, moduleName); err != nil || scope != tools.ScopeKey(h) {
		t.Fatalf("durable module scope %q %v", scope, err)
	}
}

// Registers one platform entry through a fresh context of a fresh manager
func ledgerPlatform(t *testing.T, scope, plugin, name, label string) (*Manager, *Registration) {
	t.Helper()
	m := NewManager(scope)
	handle, err := ledgerContext(plugin, m).RegisterPlatform(PlatformSpec{Name: name, Label: label, AdapterFactory: func(any) any { return label }, Check: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	return m, handle
}

// Port of test_reverse_unload_restores_an_overridden_platform_registration
func TestReverseUnloadRestoresAnOverriddenPlatformRegistration(t *testing.T) {
	platforms, _ := installLedgerServices(t)
	scope, name := t.TempDir(), "ledger_override_platform"
	ctx := context.Background()
	_, handleA := ledgerPlatform(t, scope, "ledger_owner_a", name, "Ledger A")
	entryA := platforms.get(ctx, name, scope)
	_, handleB := ledgerPlatform(t, scope, "ledger_owner_b", name, "Ledger B")
	entryB := platforms.get(ctx, name, scope)
	if handleA == nil || handleB == nil || entryA == nil || entryB == nil || entryA == entryB {
		t.Fatalf("handles %v %v entries %v %v", handleA, handleB, entryA, entryB)
	}
	if err := handleB.Dispose(); err != nil {
		t.Fatal(err)
	}
	if platforms.get(ctx, name, scope) != entryA {
		t.Fatal("disposing B did not reveal A")
	}
	if err := handleA.Dispose(); err != nil {
		t.Fatal(err)
	}
	if platforms.Snapshot(name, scope) != (PlatformSlot{}) {
		t.Fatal("disposing A did not restore the empty slot")
	}
}

// Port of test_targeted_unload_does_not_resurrect_an_older_override
func TestTargetedUnloadDoesNotResurrectAnOlderOverride(t *testing.T) {
	platforms, _ := installLedgerServices(t)
	scope, name := t.TempDir(), "ledger_out_of_order_platform"
	ctx := context.Background()
	ma, _ := ledgerPlatform(t, scope, "ledger_old", name, "old")
	oldEntry := platforms.get(ctx, name, scope)
	mb, _ := ledgerPlatform(t, scope, "ledger_new", name, "new")
	newEntry := platforms.get(ctx, name, scope)
	if oldEntry == nil || newEntry == nil {
		t.Fatal("platform not registered")
	}
	if !ma.Unload("ledger_old") || platforms.get(ctx, name, scope) != newEntry || ma.pluginPlatformNames[name] {
		t.Fatal("targeted unload of the older owner disturbed the newer one")
	}
	if !mb.Unload("ledger_new") {
		t.Fatal("unload of the newer owner found nothing")
	}
	if got := platforms.get(ctx, name, scope); got == oldEntry || platforms.Snapshot(name, scope) != (PlatformSlot{}) {
		t.Fatalf("older override resurrected: %v", got)
	}
}

// Registers one ledger tool through a context, failing the test on an error
func ledgerTool(t *testing.T, c *Context, name, toolset string, handler tools.Handler) *Registration {
	t.Helper()
	h, err := c.RegisterTool(context.Background(), ToolSpec{Name: name, Toolset: toolset, Schema: ledgerSchema(name), Handler: handler})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Port of test_targeted_unload_does_not_resurrect_an_older_tool_override
func TestTargetedUnloadDoesNotResurrectAnOlderToolOverride(t *testing.T) {
	scope, name := t.TempDir(), "ledger_out_of_order_tool"
	ctx := context.Background()
	previous := tools.Default.SnapshotRegistration(scope, name)
	ma, mb := NewManager(scope), NewManager(scope)
	marker := func(s string) tools.Handler { return func(context.Context, *tools.Call) string { return s } }
	if ledgerTool(t, ledgerContext("tool_a", ma), name, "ledger_test", marker("a")) == nil {
		t.Fatal("A registration refused")
	}
	oldEntry := tools.Default.Lookup(ctx, scope, name)
	if ledgerTool(t, ledgerContext("tool_b", mb), name, "ledger_test", marker("b")) == nil {
		t.Fatal("B registration refused")
	}
	newEntry := tools.Default.Lookup(ctx, scope, name)
	if oldEntry == nil || newEntry == oldEntry {
		t.Fatal("B did not replace A")
	}
	ma.Unload("tool_a")
	if tools.Default.Lookup(ctx, scope, name) != newEntry || ma.pluginToolNames[name] {
		t.Fatal("targeted unload of A disturbed B")
	}
	mb.Unload("tool_b")
	if tools.Default.Lookup(ctx, scope, name) == oldEntry || tools.Default.SnapshotRegistration(scope, name) != previous {
		t.Fatal("A resurrected after B unloaded")
	}
}

func ledgerSharedHandler(context.Context, *tools.Call) string { return "base" }

// Port of test_rejected_tool_registration_does_not_claim_global_fallback
func TestRejectedToolRegistrationDoesNotClaimGlobalFallback(t *testing.T) {
	name := "ledger_rejected_tool"
	previous := tools.Default.SnapshotRegistration("", name)
	tools.Default.Register(tools.Def{Name: name, Toolset: "ledger_base", Schema: ledgerSchema(name), Handler: ledgerSharedHandler})
	base := tools.Default.SnapshotRegistration("", name)
	t.Cleanup(func() { tools.Default.RestoreRegistration(name, base, previous, "") })
	m := NewManager(t.TempDir())
	if ledgerTool(t, ledgerContext("rejected_owner", m), name, "different_toolset", ledgerSharedHandler) != nil {
		t.Fatal("rejected registration returned a handle")
	}
	if m.pluginToolNames[name] || len(m.ownershipLedger) != 0 || tools.Default.SnapshotRegistration("", name) != base {
		t.Fatalf("rejection claimed the global fallback: names %v ledger %v", m.pluginToolNames, m.ownershipLedger)
	}
}

// Port of test_plugin_context_cannot_shadow_same_toolset_global_with_core_callable
func TestPluginContextCannotShadowSameToolsetGlobalWithCoreCallable(t *testing.T) {
	name := "ledger_same_toolset_global"
	previous := tools.Default.SnapshotRegistration("", name)
	tools.Default.Register(tools.Def{Name: name, Toolset: "ledger_same_toolset", Schema: ledgerSchema(name),
		Handler: func(context.Context, *tools.Call) string { return "base" }})
	base := tools.Default.SnapshotRegistration("", name)
	t.Cleanup(func() { tools.Default.RestoreRegistration(name, base, previous, "") })
	m := NewManager(t.TempDir())
	if ledgerTool(t, ledgerContext("core_callable", m), name, "ledger_same_toolset", ledgerSharedHandler) != nil {
		t.Fatal("core callable shadowed the global tool")
	}
	if tools.Default.Lookup(context.Background(), m.home, name) != base || tools.Default.SnapshotRegistration(m.home, name) != nil {
		t.Fatal("scope overlay holds a laundered core handler")
	}
}

// Port of test_rejected_tool_registration_does_not_claim_local_predecessor
func TestRejectedToolRegistrationDoesNotClaimLocalPredecessor(t *testing.T) {
	scope, name := t.TempDir(), "ledger_rejected_local_tool"
	previous := tools.Default.SnapshotRegistration(scope, name)
	shared := func(context.Context, *tools.Call) string { return "shared" }
	ma, mb := NewManager(scope), NewManager(scope)
	if ledgerTool(t, ledgerContext("local_owner", ma), name, "owner_toolset", shared) == nil {
		t.Fatal("owner registration refused")
	}
	owner := tools.Default.SnapshotRegistration(scope, name)
	if ledgerTool(t, ledgerContext("false_owner", mb), name, "different_toolset", shared) != nil {
		t.Fatal("same handler rejection manufactured a lease")
	}
	if len(mb.ownershipLedger) != 0 {
		t.Fatalf("false owner ledger %v", mb.ownershipLedger)
	}
	ma.Unload("local_owner")
	if tools.Default.SnapshotRegistration(scope, name) != previous || owner == nil {
		t.Fatal("owner unload did not restore the predecessor")
	}
}

// Port of test_scoped_plugin_cannot_deregister_a_process_global_tool
func TestScopedPluginCannotDeregisterAProcessGlobalTool(t *testing.T) {
	r := tools.NewRegistry()
	name, module := "ledger_global_base", "icebath_plugins.scoped_cleanup"
	r.Register(tools.Def{Name: name, Toolset: "ledger_base", Schema: ledgerSchema(name), Handler: ledgerSharedHandler})
	r.RegisterPluginOverridePolicy(module, true, "/profiles/isolated")
	scope, err := r.PluginScopeForModule("", module)
	if err != nil || scope != tools.ScopeKey("/profiles/isolated") {
		t.Fatalf("module scope %q %v", scope, err)
	}
	if err := r.Deregister(name, scope, module); err == nil || !strings.Contains(err.Error(), "process-global") {
		t.Fatalf("deregister error %v", err)
	}
	if r.SnapshotRegistration("", name) == nil {
		t.Fatal("scoped plugin removed the process global tool")
	}
}

// Port of test_manager_local_override_does_not_resurrect_after_targeted_unload
func TestManagerLocalOverrideDoesNotResurrectAfterTargetedUnload(t *testing.T) {
	m := NewManager(t.TempDir())
	ledgerContext("local_a", m).RegisterCLICommand("shared-local", "A", func(any) {}, nil, "")
	ledgerContext("local_b", m).RegisterCLICommand("shared-local", "B", func(any) {}, nil, "")
	if !m.Unload("local_a") || m.cliCommands["shared-local"] == nil || m.cliCommands["shared-local"].Plugin != "local_b" {
		t.Fatalf("after unloading A %+v", m.cliCommands["shared-local"])
	}
	if !m.Unload("local_b") {
		t.Fatal("unload of B found nothing")
	}
	if _, ok := m.cliCommands["shared-local"]; ok {
		t.Fatal("A resurrected after B unloaded")
	}
}

// Registers an image provider through a context, failing the test on an error
func ledgerImage(t *testing.T, c *Context, p *ledgerProvider) *Registration {
	t.Helper()
	h, err := c.RegisterImageGenProvider(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Port of test_provider_overlay_switches_profiles_and_reveals_fresh_global_fallback
func TestProviderOverlaySwitchesProfilesAndRevealsFreshGlobalFallback(t *testing.T) {
	_, providers := installLedgerServices(t)
	name := "ledger_profile_provider"
	globalA, globalB := &ledgerProvider{name, "global-a"}, &ledgerProvider{name, "global-b"}
	providerA, providerB := &ledgerProvider{name, "profile-a"}, &ledgerProvider{name, "profile-b"}
	homeA, homeB := t.TempDir(), t.TempDir()
	ma, mb := NewManager(homeA), NewManager(homeB)
	providers.registerGlobal(globalA)
	if ledgerImage(t, ledgerContext("provider_a", ma), providerA) == nil || ledgerImage(t, ledgerContext("provider_b", mb), providerB) == nil {
		t.Fatal("profile provider refused")
	}
	for _, step := range []struct {
		h    string
		want *ledgerProvider
	}{{homeA, providerA}, {homeB, providerB}, {homeA, providerA}} {
		if providers.get(name, step.h) != step.want {
			t.Fatalf("active for %s is not %s", step.h, step.want.marker)
		}
	}
	if !ma.Unload("provider_a") || providers.get(name, homeA) != globalA {
		t.Fatal("unload of A did not reveal the global provider")
	}
	providers.registerGlobal(globalB)
	if providers.get(name, homeA) != globalB || providers.get(name, homeB) != providerB {
		t.Fatal("fresh global fallback not visible to A or B lost its overlay")
	}
	if !mb.Unload("provider_b") || providers.get(name, homeB) != globalB {
		t.Fatal("unload of B pinned a stale base")
	}
}

// Port of test_reused_provider_singleton_keeps_registration_generations_distinct
func TestReusedProviderSingletonKeepsRegistrationGenerationsDistinct(t *testing.T) {
	_, providers := installLedgerServices(t)
	scope, name := t.TempDir(), "ledger_generation_provider"
	managers := make([]*Manager, 4)
	contexts := make([]*Context, 4)
	for i := range managers {
		managers[i] = NewManager(scope)
		contexts[i] = ledgerContext("provider_"+string(rune('0'+i)), managers[i])
	}
	a, b, c := &ledgerProvider{name, "a"}, &ledgerProvider{name, "b"}, &ledgerProvider{name, "c"}
	ledgerImage(t, contexts[0], a)
	ledgerImage(t, contexts[1], b)
	managers[0].Unload("provider_0")
	if providers.get(name, scope) != b {
		t.Fatal("B lost the slot when A's first generation retired")
	}
	ledgerImage(t, contexts[2], a)
	ledgerImage(t, contexts[3], c)
	managers[3].Unload("provider_3")
	if providers.get(name, scope) != a {
		t.Fatal("A's second generation was treated as retired")
	}
	managers[2].Unload("provider_2")
	if providers.get(name, scope) != b {
		t.Fatal("retiring A's second generation did not reveal B")
	}
	managers[1].Unload("provider_1")
	if providers.get(name, scope) != nil {
		t.Fatal("baseline not restored")
	}
}

// Port of test_same_provider_singleton_can_have_two_live_owners
func TestSameProviderSingletonCanHaveTwoLiveOwners(t *testing.T) {
	_, providers := installLedgerServices(t)
	scope := t.TempDir()
	p := &ledgerProvider{"ledger_shared_singleton", ""}
	ma, mb := NewManager(scope), NewManager(scope)
	if ledgerImage(t, ledgerContext("shared_a", ma), p) == nil || ledgerImage(t, ledgerContext("shared_b", mb), p) == nil {
		t.Fatal("shared provider refused")
	}
	ma.Unload("shared_a")
	if providers.get(p.name, scope) != p {
		t.Fatal("retiring the older identical lease removed the newer owner")
	}
	mb.Unload("shared_b")
	if providers.get(p.name, scope) != nil {
		t.Fatal("baseline not restored")
	}
}

// Port of test_provider_cleanup_uses_the_captured_normalized_name
func TestProviderCleanupUsesTheCapturedNormalizedName(t *testing.T) {
	_, providers := installLedgerServices(t)
	scope, key := t.TempDir(), "ledger_mutable_provider"
	p := &ledgerProvider{"  ledger_mutable_provider  ", ""}
	m := NewManager(scope)
	if ledgerImage(t, ledgerContext("mutable_provider", m), p) == nil || providers.get(key, scope) != p {
		t.Fatal("provider not registered under its stripped name")
	}
	p.name = "renamed_after_registration"
	m.Unload("mutable_provider")
	if providers.get(key, scope) != nil {
		t.Fatal("renamed provider orphaned its entry")
	}
}

// Port of test_registration_transaction_excludes_concurrent_disposal
func TestRegistrationTransactionExcludesConcurrentDisposal(t *testing.T) {
	_, providers := installLedgerServices(t)
	scope, name := t.TempDir(), "ledger_transaction_provider"
	a, b := &ledgerProvider{name, "a"}, &ledgerProvider{name, "b"}
	ma, mb := NewManager(scope), NewManager(scope)
	ledgerImage(t, ledgerContext("tx_a", ma), a)
	wrote, release := make(chan struct{}), make(chan struct{})
	providers.mu.Lock()
	providers.pause = func(p *ledgerProvider) {
		if p == b {
			close(wrote)
			<-release
		}
	}
	providers.mu.Unlock()
	registered := make(chan *Registration, 1)
	go func() {
		h, _ := ledgerContext("tx_b", mb).RegisterImageGenProvider(b)
		registered <- h
	}()
	select {
	case <-wrote:
	case <-time.After(time.Second):
		t.Fatal("registration never wrote")
	}
	disposed := make(chan bool, 1)
	go func() { disposed <- ma.Unload("tx_a") }()
	select {
	case <-disposed:
		t.Fatal("dispose interleaved with registration transaction")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if h := <-registered; h == nil {
		t.Fatal("registration refused")
	}
	if !<-disposed {
		t.Fatal("unload found nothing")
	}
	if providers.get(name, scope) != b {
		t.Fatal("B lost the slot to the concurrent disposal")
	}
	mb.Unload("tx_b")
	if providers.get(name, scope) != nil {
		t.Fatal("baseline not restored")
	}
}

// Port of test_resolved_deferred_platform_restores_its_displaced_loader
func TestResolvedDeferredPlatformRestoresItsDisplacedLoader(t *testing.T) {
	platforms, _ := installLedgerServices(t)
	scope, name := t.TempDir(), "ledger_transfer"
	oldLoader := &DeferredPlatformLoader{Load: func(context.Context) {}}
	platforms.RegisterDeferred(name, oldLoader, scope)
	displaced := platforms.Snapshot(name, scope)
	ledgerSwapBundled(t, ledgerBundled{m: Manifest{Name: name + "-platform", Kind: "platform"}, register: func(ctx *Context) error {
		_, err := ctx.RegisterPlatform(PlatformSpec{Name: name, Label: "Transferred", AdapterFactory: func(any) any { return struct{}{} }, Check: func() bool { return true }})
		return err
	}})
	var mf *Manifest
	for _, b := range bundledManifests() {
		mf = b
	}
	m := NewManager(scope)
	ctx := homeCtx(scope)
	if err := m.registerDeferredPlatform(ctx, mf); err != nil {
		t.Fatal(err)
	}
	if entry := platforms.get(ctx, name, scope); entry == nil || entry.Label != "Transferred" {
		t.Fatalf("resolved entry %+v (load error %q)", entry, loadedError(m, mf.ID()))
	}
	m.Unload(mf)
	if platforms.Snapshot(name, scope) != displaced {
		t.Fatalf("slot %+v is not the displaced loader", platforms.Snapshot(name, scope))
	}
}

// Compiled-in profile probe reading its marker from the home it loads in, the Go twin of the hermes profile_probe
func profileProbePlugin() Plugin {
	return ledgerBundled{m: Manifest{Name: "profile_probe", Version: "0.1.0", Description: "profile probe"}, register: func(ctx *Context) error {
		raw, err := os.ReadFile(filepath.Join(ctx.manager.home, "profile_probe_marker"))
		if err != nil {
			return err
		}
		marker := string(raw)
		if _, err := ctx.RegisterTool(context.Background(), ToolSpec{Name: "shared_profile_tool", Toolset: "profile_probe", Schema: ledgerSchema("shared_profile_tool"),
			Handler: func(context.Context, *tools.Call) string { return marker }}); err != nil {
			return err
		}
		_, err = ctx.RegisterPlatform(PlatformSpec{Name: "shared_profile_platform", Label: marker, AdapterFactory: func(any) any { return marker }, Check: func() bool { return true }})
		return err
	}}
}

// Writes the profile probe marker and config enabling it under a home
func writeProfileProbe(t *testing.T, h, marker string) {
	t.Helper()
	writeFile(t, filepath.Join(h, "profile_probe_marker"), marker)
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  enabled: [profile_probe]\n")
}

// Port of test_same_name_tool_and_platform_are_isolated_by_hermes_home
func TestSameNameToolAndPlatformAreIsolatedByHermesHome(t *testing.T) {
	platforms, _ := installLedgerServices(t)
	ledgerSwapBundled(t, profileProbePlugin())
	homeA, homeB := t.TempDir(), t.TempDir()
	writeProfileProbe(t, homeA, "profile-a")
	writeProfileProbe(t, homeB, "profile-b")
	load := func(h string) (*Manager, *tools.Def, *ledgerPlatformEntry) {
		ctx := homeCtx(h)
		m := GetPluginManager(ctx)
		m.DiscoverAndLoad(ctx, false)
		return m, tools.Default.Lookup(ctx, h, "shared_profile_tool"), platforms.get(ctx, "shared_profile_platform", h)
	}
	ma, toolA, platformA := load(homeA)
	mb, toolB, platformB := load(homeB)
	t.Cleanup(func() { ma.Unload("profile_probe"); mb.Unload("profile_probe") })
	if ma == mb || toolA == nil || toolB == nil || toolA == toolB || platformA == nil || platformB == nil || platformA == platformB {
		t.Fatalf("profiles share registrations: managers %p %p tools %v %v platforms %v %v", ma, mb, toolA, toolB, platformA, platformB)
	}
	for _, step := range []struct {
		h, marker string
		platform  *ledgerPlatformEntry
	}{{homeA, "profile-a", platformA}, {homeB, "profile-b", platformB}, {homeA, "profile-a", platformA}} {
		ctx := homeCtx(step.h)
		got := tools.Default.Lookup(ctx, step.h, "shared_profile_tool").Handler(ctx, &tools.Call{Name: "shared_profile_tool", Args: map[string]any{}})
		if got != step.marker || platforms.get(ctx, "shared_profile_platform", step.h) != step.platform {
			t.Fatalf("dispatch under %s gave %q", step.marker, got)
		}
	}
}

// Port of test_manager_discovery_uses_its_home_not_the_ambient_profile
func TestManagerDiscoveryUsesItsHomeNotTheAmbientProfile(t *testing.T) {
	platforms, _ := installLedgerServices(t)
	ledgerSwapBundled(t, profileProbePlugin())
	homeA, homeB := t.TempDir(), t.TempDir()
	writeProfileProbe(t, homeA, "retained-a")
	writeProfileProbe(t, homeB, "ambient-b")
	m := NewManager(homeA)
	t.Cleanup(func() { m.Unload("profile_probe") })
	m.DiscoverAndLoad(homeCtx(homeB), false)
	tool := tools.Default.Lookup(homeCtx(homeA), homeA, "shared_profile_tool")
	if tool == nil || tool.Handler(context.Background(), &tools.Call{Args: map[string]any{}}) != "retained-a" {
		t.Fatalf("tool of the retained home %v", tool)
	}
	if e, _ := platforms.Snapshot("shared_profile_platform", homeA).Entry.(*ledgerPlatformEntry); e == nil || e.Label != "retained-a" {
		t.Fatalf("platform of the retained home %+v", e)
	}
	if tools.Default.SnapshotRegistration(homeB, "shared_profile_tool") != nil || platforms.Snapshot("shared_profile_platform", homeB) != (PlatformSlot{}) {
		t.Fatal("discovery wrote into the ambient profile")
	}
}

// Port of test_same_slug_profiles_allocate_distinct_modules_concurrently
func TestSameSlugProfilesAllocateDistinctModulesConcurrently(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	python := liveInterpreters(t)["system"]
	installHostEnvHooks()
	installLedgerServices(t)
	ledgerSwapBundled(t)
	t.Setenv("PYTHONPATH", "")
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	var managers []*Manager
	for _, marker := range []string{"concurrent-a", "concurrent-b"} {
		h := t.TempDir()
		dir := filepath.Join(h, "plugins", "profile_probe")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: profile_probe\nversion: 0.1.0\ndescription: profile probe "+marker+"\n")
		writeFile(t, filepath.Join(dir, "__init__.py"), "def register(ctx):\n    ctx.register_tool(\n        name='shared_profile_tool',\n        toolset='profile_probe',\n"+
			"        schema={'name': 'shared_profile_tool', 'parameters': {'type': 'object', 'properties': {}}},\n        handler=lambda args, **kwargs: '"+marker+"',\n    )\n")
		writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: host\n  enabled: [profile_probe]\n")
		m := NewManager(h)
		t.Cleanup(func() {
			m.Unload("profile_probe")
			if m.pluginHostInstance != nil {
				m.pluginHostInstance.Shutdown()
			}
		})
		managers = append(managers, m)
	}
	var wg sync.WaitGroup
	for _, m := range managers {
		wg.Add(1)
		go func(m *Manager) { defer wg.Done(); m.DiscoverAndLoad(homeCtx(m.home), false) }(m)
	}
	wg.Wait()
	modules := make([]string, 0, 2)
	for _, m := range managers {
		m.mu.RLock()
		p := m.plugins["profile_probe"]
		m.mu.RUnlock()
		if p == nil || p.Error != "" {
			t.Fatalf("profile probe not loaded: %+v", p)
		}
		modules = append(modules, p.Module)
	}
	if modules[0] == "" || modules[0] == modules[1] {
		t.Fatalf("same slug profiles share module %v", modules)
	}
}

// Port of test_spawned_supervised_task_is_cancelled_on_unload
func TestSpawnedSupervisedTaskIsCancelledOnUnload(t *testing.T) {
	m := NewManager(t.TempDir())
	c := newContext(&Manifest{Name: "task_probe", Version: "0.1", Source: "user"}, m)
	cancelled := make(chan bool, 1)
	started := make(chan struct{})
	task, err := c.SpawnTask(pyctx.WithRunningLoop(context.Background()), func(ctx context.Context) error {
		close(started)
		select {
		case <-ctx.Done():
			cancelled <- true
			return ctx.Err()
		case <-time.After(time.Hour):
			return nil
		}
	}, "probe-task")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	m.mu.RLock()
	kinds := map[string]bool{}
	for _, r := range m.ownershipLedger["task_probe"] {
		kinds[r.Kind] = true
	}
	m.mu.RUnlock()
	if !kinds["background_task"] {
		t.Fatalf("ledger kinds %v", kinds)
	}
	if !m.Unload("task_probe") {
		t.Fatal("unload found nothing")
	}
	<-task.done
	if !errors.Is(task.err, context.Canceled) {
		t.Fatalf("task error %v", task.err)
	}
	m.mu.RLock()
	_, left := m.ownershipLedger["task_probe"]
	m.mu.RUnlock()
	if left || !<-cancelled {
		t.Fatal("task ledger entry survived or the task never saw cancellation")
	}
}

// Routes one registration of a plugin module into the scope its override policies attribute it to
func registerFromModule(r *tools.Registry, h, module, name string) error {
	scope, err := r.PluginScopeForModule(h, module)
	if err != nil {
		return err
	}
	return r.RegisterScoped(scope, tools.Def{Name: name, Toolset: "entrypoint_shared", Schema: ledgerSchema(name), Handler: ledgerSharedHandler, Plugin: module})
}

// Port of test_shared_entrypoint_module_uses_the_active_profile_scope
func TestSharedEntrypointModuleUsesTheActiveProfileScope(t *testing.T) {
	r := tools.NewRegistry()
	module := "third_party.shared_icebath_plugin"
	homeA, homeB, homeC := t.TempDir(), t.TempDir(), t.TempDir()
	policyA := r.RegisterPluginOverridePolicy(module, false, homeA)
	policyB := r.RegisterPluginOverridePolicy(module, false, homeB)
	for _, step := range [][2]string{{homeA, "shared_entrypoint_a"}, {homeB, "shared_entrypoint_b"}} {
		if err := registerFromModule(r, step[0], module, step[1]); err != nil {
			t.Fatal(err)
		}
	}
	if r.SnapshotRegistration(homeA, "shared_entrypoint_a") == nil || r.SnapshotRegistration(homeB, "shared_entrypoint_a") != nil ||
		r.SnapshotRegistration(homeB, "shared_entrypoint_b") == nil || r.SnapshotRegistration("", "shared_entrypoint_b") != nil {
		t.Fatal("entrypoint registration leaked across profiles")
	}
	scope, err := r.PluginScopeForModule(homeA, module)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Deregister("shared_entrypoint_a", scope, module); err != nil {
		t.Fatal(err)
	}
	if r.SnapshotRegistration(homeA, "shared_entrypoint_a") != nil || r.SnapshotRegistration(homeB, "shared_entrypoint_b") == nil {
		t.Fatal("deregister crossed profiles")
	}
	r.RestorePluginOverridePolicy(module, policyA, nil, homeA)
	r.RestorePluginOverridePolicy(module, policyB, nil, homeB)
	if err := registerFromModule(r, homeA, module, "shared_entrypoint_stale"); err != nil {
		t.Fatal(err)
	}
	if r.SnapshotRegistration(homeA, "shared_entrypoint_stale") == nil || r.SnapshotRegistration("", "shared_entrypoint_stale") != nil {
		t.Fatal("stale callback escaped its durable scope")
	}
	if err := registerFromModule(r, homeC, module, "shared_entrypoint_ambiguous"); err == nil || !strings.Contains(err.Error(), "multiple profiles") {
		t.Fatalf("ambiguous registration error %v", err)
	}
	if r.SnapshotRegistration("", "shared_entrypoint_ambiguous") != nil {
		t.Fatal("ambiguous registration reached the global layer")
	}
}

// Port of test_decorated_plugin_callable_keeps_its_defining_module_scope
func TestDecoratedPluginCallableKeepsItsDefiningModuleScope(t *testing.T) {
	r := tools.NewRegistry()
	module, scope := "third_party.decorated_plugin", t.TempDir()
	r.RegisterPluginOverridePolicy(module, false, scope)
	inner := func(context.Context, *tools.Call) string { return "decorated" }
	wrapped := func(ctx context.Context, call *tools.Call) string { return inner(ctx, call) }
	resolved, err := r.PluginScopeForModule("", module)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterScoped(resolved, tools.Def{Name: "decorated_plugin_tool", Toolset: "decorated_plugin", Schema: ledgerSchema("decorated_plugin_tool"), Handler: wrapped, Plugin: module}); err != nil {
		t.Fatal(err)
	}
	if r.SnapshotRegistration(scope, "decorated_plugin_tool") == nil || r.SnapshotRegistration("", "decorated_plugin_tool") != nil {
		t.Fatal("wrapped handler lost its defining module scope")
	}
}

// Port of test_entrypoint_policy_uses_the_most_specific_module_prefix
func TestEntrypointPolicyUsesTheMostSpecificModulePrefix(t *testing.T) {
	r := tools.NewRegistry()
	broad, narrow := t.TempDir(), t.TempDir()
	r.RegisterPluginOverridePolicy("vendor", true, broad)
	r.RegisterPluginOverridePolicy("vendor.plugin", false, narrow)
	if err := registerFromModule(r, narrow, "vendor.plugin.handlers", "specific_entrypoint_tool"); err != nil {
		t.Fatal(err)
	}
	if r.SnapshotRegistration(narrow, "specific_entrypoint_tool") == nil || r.SnapshotRegistration(broad, "specific_entrypoint_tool") != nil {
		t.Fatal("broad prefix policy captured the narrower module")
	}
}
