package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Snapshot of the process wide registry state restored when the test ends
func isolateRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	savedRegs := append([]*ProviderRegistration(nil), registrations...)
	savedSkills := map[string]map[string]string{}
	for k, v := range registeredSkills {
		inner := map[string]string{}
		for q, p := range v {
			inner[q] = p
		}
		savedSkills[k] = inner
	}
	savedCache := map[string]*ProviderConfigSchema{}
	for k, v := range schemaCache {
		savedCache[k] = v
	}
	savedSkillRegistry := skillRegistry
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registrations, registeredSkills, schemaCache, skillRegistry = savedRegs, savedSkills, savedCache, savedSkillRegistry
		registryMu.Unlock()
	})
}

// Minimal external provider whose instances carry a mark set by the registering factory
type regProvider struct {
	BaseProvider
	name   string
	marked bool
}

func (p *regProvider) Name() string                       { return p.name }
func (p *regProvider) Available(ctx context.Context) bool { return true }
func (p *regProvider) Initialize(ctx context.Context, sessionID string, opts InitOptions) error {
	return nil
}
func (p *regProvider) ToolSchemas(ctx context.Context) ([]map[string]any, error) { return nil, nil }

func regFactory(name string) func(context.Context, *tools.Env, LoadOptions) (Provider, error) {
	return func(context.Context, *tools.Env, LoadOptions) (Provider, error) { return &regProvider{name: name}, nil }
}

func writeConfig(t *testing.T, h, body string) {
	t.Helper()
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryUnknownProviderSchemaIsNone(t *testing.T) {
	isolateRegistry(t)
	ctx := context.Background()
	if s := GetProviderConfigSchema(ctx, nil, "builtin"); s != nil {
		t.Fatalf("builtin schema %+v", s)
	}
	if s := GetProviderConfigSchema(ctx, nil, "byterover"); s != nil {
		t.Fatalf("byterover declares no schema, got %+v", s)
	}
}

func TestRegistrySchemaCacheKeysOnSourceNotName(t *testing.T) {
	isolateRegistry(t)
	root := t.TempDir()
	homes := map[string]string{}
	for _, label := range []string{"A", "B"} {
		h := filepath.Join(root, label)
		homes[label] = h
		label := label
		RegisterProvider(ProviderRegistration{
			Name: "custom", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "custom"), New: regFactory("custom"),
			ConfigSchema: func(context.Context) (*ProviderConfigSchema, error) {
				return NewProviderConfigSchema("custom", label), nil
			},
		})
	}
	for _, label := range []string{"A", "B", "A"} {
		ctx := home.WithOverride(context.Background(), homes[label])
		s := GetProviderConfigSchema(ctx, nil, "custom")
		if s == nil || s.Label != label {
			t.Fatalf("profile %s got schema %+v", label, s)
		}
	}
}

func TestRegistryBrokenSchemaIsNotCached(t *testing.T) {
	isolateRegistry(t)
	broken := true
	RegisterProvider(ProviderRegistration{
		Name: "broken", Source: SourceEntryPoint, Dir: filepath.Join(t.TempDir(), "broken"), New: regFactory("broken"),
		ConfigSchema: func(context.Context) (*ProviderConfigSchema, error) {
			if broken {
				return nil, errors.New("invalid syntax")
			}
			return NewProviderConfigSchema("broken", "Broken"), nil
		},
	})
	ctx := context.Background()
	if s := GetProviderConfigSchema(ctx, nil, "broken"); s != nil {
		t.Fatalf("broken schema loaded %+v", s)
	}
	registryMu.RLock()
	for k := range schemaCache {
		if strings.HasSuffix(k, "\x00broken") {
			registryMu.RUnlock()
			t.Fatal("a failed load was cached")
		}
	}
	registryMu.RUnlock()
	broken = false
	if s := GetProviderConfigSchema(ctx, nil, "broken"); s == nil || s.Label != "Broken" {
		t.Fatalf("recovered schema %+v", s)
	}
}

func TestRegistryDisabledUserProviderIsFoundButNeverLoaded(t *testing.T) {
	isolateRegistry(t)
	root := t.TempDir()
	enabled, disabled := filepath.Join(root, "enabled"), filepath.Join(root, "disabled")
	for _, h := range []string{enabled, disabled} {
		RegisterProvider(ProviderRegistration{
			Name: "fakemem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "fakemem"),
			Spellings: []string{"fakemem-manifest"}, Manifest: "name: fakemem-manifest\nkind: exclusive\n", New: regFactory("fakemem"),
		})
	}
	writeConfig(t, enabled, "memory:\n  provider: fakemem\nplugins:\n  disabled: []\n")
	writeConfig(t, disabled, "memory:\n  provider: fakemem\nplugins:\n  disabled: [fakemem-manifest]\n")
	t.Setenv("ICEBATH_HOME", enabled)
	inEnabled := home.WithOverride(context.Background(), enabled)
	inDisabled := home.WithOverride(context.Background(), disabled)
	if LoadMemoryProvider(inEnabled, nil, "fakemem") == nil {
		t.Fatal("enabled profile did not load its provider")
	}
	if r := FindProvider(inDisabled, nil, "fakemem"); r == nil || r.Dir != filepath.Join(disabled, "plugins", "fakemem") {
		t.Fatalf("disabled provider not discoverable %+v", r)
	}
	if LoadMemoryProvider(inDisabled, nil, "fakemem") != nil {
		t.Fatal("plugins.disabled provider loaded")
	}
	notice, parked := ParkedProviderNotice(inDisabled, nil, "fakemem")
	want := "⚠ Memory provider 'fakemem' is turned off: plugins.disabled lists it, so external memory is off " +
		"(built-in memory still works). Turn it back on with `icebath plugins enable fakemem`."
	if !parked || notice != want {
		t.Fatalf("notice %q parked %v", notice, parked)
	}
	if _, parked := ParkedProviderNotice(inEnabled, nil, "fakemem"); parked {
		t.Fatal("enabled profile reported parked")
	}
	if LoadMemoryProvider(inEnabled, nil, "fakemem") == nil {
		t.Fatal("enabled profile lost its provider")
	}
}

func TestRegistryParkedNoticeNamesTheUpdate(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	RegisterProvider(ProviderRegistration{Name: "fakemem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "fakemem"), New: regFactory("fakemem")})
	writeConfig(t, h, "plugins:\n  disabled: [fakemem]\n  disabled_by_update:\n    fakemem:\n      reason: dependency install failed\n")
	notice, parked := ParkedProviderNotice(home.WithOverride(context.Background(), h), nil, "fakemem")
	if !parked || !strings.Contains(notice, " An update turned it off (dependency install failed) and a later update retries it.") {
		t.Fatalf("notice %q", notice)
	}
}

func TestRegistryBundledProviderIsNeverParked(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	writeConfig(t, h, "plugins:\n  disabled: [byterover, retaindb]\n")
	ctx := home.WithOverride(context.Background(), h)
	if _, parked := ParkedProviderNotice(ctx, nil, "byterover"); parked {
		t.Fatal("bundled provider parked")
	}
	if LoadMemoryProvider(ctx, nil, "retaindb") == nil {
		t.Fatal("bundled provider refused by plugins.disabled")
	}
}

func TestRegistryProjectProviderIsDiscoveredWhenRegistered(t *testing.T) {
	isolateRegistry(t)
	ctx := context.Background()
	if containsString(ListMemoryProviderNames(ctx, nil), "projectmem") || FindProvider(ctx, nil, "projectmem") != nil {
		t.Fatal("unregistered project provider visible")
	}
	dir := filepath.Join(t.TempDir(), ".icebath", "plugins", "projectmem")
	RegisterProvider(ProviderRegistration{Name: "projectmem", Source: SourceProject, Dir: dir, New: regFactory("projectmem")})
	if !containsString(ListMemoryProviderNames(ctx, nil), "projectmem") {
		t.Fatal("project provider not listed")
	}
	if r := FindProvider(ctx, nil, "projectmem"); r == nil || r.Dir != dir {
		t.Fatalf("project provider %+v", r)
	}
}

func TestRegistryBundledStillWinsOverProjectAndUser(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	RegisterProvider(ProviderRegistration{Name: "byterover", Source: SourceProject, Dir: filepath.Join(t.TempDir(), "byterover"), New: regFactory("byterover")})
	RegisterProvider(ProviderRegistration{Name: "byterover", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "byterover"), New: regFactory("byterover")})
	if r := FindProvider(context.Background(), nil, "byterover"); r == nil || r.Source != SourceBundled {
		t.Fatalf("byterover resolved to %+v", r)
	}
	RegisterProvider(ProviderRegistration{Name: "shared", Source: SourceEntryPoint, New: regFactory("shared")})
	RegisterProvider(ProviderRegistration{Name: "shared", Source: SourceProject, Dir: "/p/shared", New: regFactory("shared")})
	RegisterProvider(ProviderRegistration{Name: "shared", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "shared"), New: regFactory("shared")})
	if r := FindProvider(context.Background(), nil, "shared"); r == nil || r.Source != SourceUser {
		t.Fatalf("user source should win over project and entry point, got %+v", r)
	}
	names := ListMemoryProviderNames(context.Background(), nil)
	count := 0
	for _, n := range names {
		if n == "shared" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("names %v", names)
	}
}

func TestRegistryUserSourceIsScopedToItsHome(t *testing.T) {
	isolateRegistry(t)
	a, b := t.TempDir(), t.TempDir()
	RegisterProvider(ProviderRegistration{Name: "onlya", Source: SourceUser, Home: a, Dir: filepath.Join(a, "plugins", "onlya"), New: regFactory("onlya")})
	if FindProvider(home.WithOverride(context.Background(), a), nil, "onlya") == nil {
		t.Fatal("provider invisible in its own home")
	}
	if FindProvider(home.WithOverride(context.Background(), b), nil, "onlya") != nil {
		t.Fatal("another profile's user provider answered")
	}
	if FindProvider(context.Background(), &tools.Env{Home: a}, "onlya") == nil {
		t.Fatal("session environment home not honoured")
	}
}

func TestRegistryEntryPointProviderIsListedWithoutLoading(t *testing.T) {
	isolateRegistry(t)
	loads := 0
	pkgDir := filepath.Join(t.TempDir(), "pipmem_pkg")
	RegisterProvider(ProviderRegistration{Name: "pipmem", Source: SourceEntryPoint, Dir: pkgDir,
		New: func(context.Context, *tools.Env, LoadOptions) (Provider, error) {
			loads++
			return &regProvider{name: "pipmem"}, nil
		}})
	RegisterProvider(ProviderRegistration{Name: "flatmem", Source: SourceEntryPoint, New: regFactory("flatmem")})
	ctx := context.Background()
	if !containsString(ListMemoryProviderNames(ctx, nil), "pipmem") {
		t.Fatal("entry point provider not listed")
	}
	if r := FindProvider(ctx, nil, "pipmem"); r == nil || r.Dir != pkgDir {
		t.Fatalf("package entry point dir %+v", r)
	}
	if loads != 0 {
		t.Fatal("resolving an entry point constructed it")
	}
	if r := FindProvider(ctx, nil, "flatmem"); r == nil || r.Dir != "" || hasProviderDir(r) {
		t.Fatalf("bare module entry point has a directory %+v", r)
	}
	for _, d := range DiscoverMemoryProviders(ctx, nil) {
		if d.Name == "pipmem" && (d.Description != "" || !d.Available) {
			t.Fatalf("entry point discovery %+v", d)
		}
	}
}

func TestRegistrySecondaryRegistrationCannotCostTheProvider(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	RegisterProvider(ProviderRegistration{Name: "auxmem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "auxmem"),
		New: func(context.Context, *tools.Env, LoadOptions) (Provider, error) {
			return &regProvider{name: "auxmem", marked: true}, errors.New("register_auxiliary_task failed")
		}})
	loaded := LoadMemoryProvider(context.Background(), nil, "auxmem")
	p, ok := loaded.(*regProvider)
	if !ok || p.Name() != "auxmem" || !p.marked {
		t.Fatalf("loaded %#v", loaded)
	}
}

func TestRegistryActivationIsNotGatedOnPluginsEnabled(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeConfig(t, h, "plugins:\n  enabled: []\n")
	RegisterProvider(ProviderRegistration{Name: "gatedmem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "gatedmem"), New: regFactory("gatedmem")})
	if LoadMemoryProvider(context.Background(), nil, "gatedmem") == nil {
		t.Fatal("provider gated on plugins.enabled")
	}
}

func TestRegistryFailingProviderDoesNotAbortDiscovery(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	RegisterProvider(ProviderRegistration{Name: "goodmem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "goodmem"), New: regFactory("goodmem")})
	RegisterProvider(ProviderRegistration{Name: "denied", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "denied"),
		New: func(context.Context, *tools.Env, LoadOptions) (Provider, error) { panic("permission denied") }})
	RegisterProvider(ProviderRegistration{Name: "nothing", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "nothing"),
		New: func(context.Context, *tools.Env, LoadOptions) (Provider, error) { return nil, nil }})
	ctx := context.Background()
	found := map[string]DiscoveredProvider{}
	for _, d := range DiscoverMemoryProviders(ctx, nil) {
		found[d.Name] = d
	}
	if !found["goodmem"].Available || !containsString(ListMemoryProviderNames(ctx, nil), "byterover") {
		t.Fatalf("discovery lost siblings %+v", found)
	}
	if found["denied"].Available || found["nothing"].Available {
		t.Fatalf("broken providers reported available %+v", found)
	}
	if LoadMemoryProvider(ctx, nil, "denied") != nil || LoadMemoryProvider(ctx, nil, "nothing") != nil {
		t.Fatal("broken providers loaded")
	}
	if LoadMemoryProvider(ctx, nil, "missing") != nil {
		t.Fatal("unknown provider loaded")
	}
}

// Plugin skill registry fake recording removals
type regSkills struct {
	mu      sync.Mutex
	skills  map[string]string
	removed []string
}

func (r *regSkills) FindPluginSkill(q string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.skills[q]
	return p, ok
}

func (r *regSkills) RemovePluginSkill(q string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.skills, q)
	r.removed = append(r.removed, q)
}

func TestRegistrySkillsRegisterOnlyForTheActiveProviderAndArePruned(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	skillsReg := &regSkills{skills: map[string]string{}}
	SetPluginSkillRegistry(skillsReg)
	var seen []bool
	RegisterProvider(ProviderRegistration{Name: "skillmem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "skillmem"),
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) {
			seen = append(seen, opts.RegisterSkills)
			if opts.RegisterSkills {
				skillsReg.mu.Lock()
				skillsReg.skills["skillmem:tips"] = "/skills/tips/SKILL.md"
				skillsReg.mu.Unlock()
				opts.TrackSkill("skillmem:tips")
			}
			return &regProvider{name: "skillmem"}, nil
		}})
	ctx := context.Background()
	writeConfig(t, h, "memory:\n  provider: other\n")
	if LoadMemoryProvider(ctx, nil, "skillmem") == nil {
		t.Fatal("inactive provider failed to load")
	}
	DiscoverMemoryProviders(ctx, nil)
	writeConfig(t, h, "memory:\n  provider: skillmem\n")
	if LoadMemoryProvider(ctx, nil, "skillmem") == nil {
		t.Fatal("active provider failed to load")
	}
	if len(seen) != 3 || seen[0] || seen[1] || !seen[2] {
		t.Fatalf("register skills flags %v", seen)
	}
	PruneInactiveMemoryProviderSkills(ctx, nil, "skillmem")
	if len(skillsReg.removed) != 0 {
		t.Fatalf("active provider skills pruned %v", skillsReg.removed)
	}
	PruneInactiveMemoryProviderSkills(home.WithOverride(ctx, t.TempDir()), nil, "")
	if len(skillsReg.removed) != 0 {
		t.Fatal("another profile's pruning retracted this profile's skill")
	}
	PruneInactiveMemoryProviderSkills(ctx, nil, ActiveMemoryProvider(ctx, nil)+"-switched")
	if len(skillsReg.removed) != 1 || skillsReg.removed[0] != "skillmem:tips" {
		t.Fatalf("removed %v", skillsReg.removed)
	}
	registryMu.RLock()
	left := len(registeredSkills[homeKey(h)])
	registryMu.RUnlock()
	if left != 0 {
		t.Fatal("tracked skill kept after pruning")
	}
}

// OAuth flow fake reporting fixed states
type regFlow struct{}

func (regFlow) StartLoopbackFlowBackground(ctx context.Context) (any, error) {
	return map[string]any{"state": "pending", "config": filepath.Join(home.Dir(ctx), "honcho.json")}, nil
}
func (regFlow) FlowStatus(ctx context.Context) (any, error) {
	return map[string]any{"state": "idle"}, nil
}

func TestRegistryUserDirProviderServesItsDeclaredSchemaAndFlow(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	field := NewProviderField("workspace", "Workspace")
	field.Inline = true
	schema := NewProviderConfigSchema("honcho", "Honcho (user dir)", field)
	schema.Storage = StorageHonchoHostBlock
	RegisterProvider(ProviderRegistration{Name: "honcho", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "honcho"),
		New: regFactory("honcho"), OAuth: regFlow{},
		ConfigSchema: func(context.Context) (*ProviderConfigSchema, error) { return schema, nil }})
	ctx := context.Background()
	got := GetProviderConfigSchema(ctx, nil, "honcho")
	if got == nil || got.Storage != StorageHonchoHostBlock || len(got.InlineFields()) != 1 || got.InlineFields()[0].Key != "workspace" {
		t.Fatalf("schema %+v", got)
	}
	flow, err := ProviderOAuthFlow(ctx, nil, "honcho")
	if err != nil {
		t.Fatal(err)
	}
	started, _ := flow.StartLoopbackFlowBackground(ctx)
	if started.(map[string]any)["state"] != "pending" {
		t.Fatalf("start %v", started)
	}
	if _, err := ProviderOAuthFlow(ctx, nil, "byterover"); !errors.Is(err, errNoOAuthFlow) {
		t.Fatalf("byterover flow error %v", err)
	}
	if _, err := ProviderOAuthFlow(ctx, nil, "absent"); !errors.Is(err, errProviderNotInstalled) {
		t.Fatalf("absent flow error %v", err)
	}
}

func TestRegistrySchemaFromRecord(t *testing.T) {
	s, err := ProviderConfigSchemaFromRecord(map[string]any{
		"name": "honcho", "label": "Honcho", "storage": StorageHonchoHostBlock,
		"fields": []any{map[string]any{
			"key": "mode", "label": "Mode", "kind": KindSelect, "inline": true, "env_key": nil,
			"options":       []any{map[string]any{"value": "a", "label": "A"}, map[string]any{"value": "b", "label": "B", "description": "bee"}},
			"aliases":       []any{"legacy_mode"},
			"env_fallbacks": []any{"HONCHO_MODE"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	f := s.Fields[0]
	if s.Storage != StorageHonchoHostBlock || f.Kind != KindSelect || f.Scope != "host" || !f.Inline || f.EnvKey != "" ||
		len(f.Options) != 2 || f.Options[1].Description != "bee" || f.Aliases[0] != "legacy_mode" || !f.AllowedValues()["b"] {
		t.Fatalf("rebuilt %+v", s)
	}
	if f.IsSecret() {
		t.Fatal("select field reported secret")
	}
	if s, err := ProviderConfigSchemaFromRecord(nil); s != nil || err != nil {
		t.Fatalf("empty record %v %v", s, err)
	}
	if _, err := ProviderConfigSchemaFromRecord(map[string]any{"name": "x", "label": "X", "bogus": 1}); err == nil {
		t.Fatal("unknown schema key accepted")
	}
	if _, err := ProviderConfigSchemaFromRecord(map[string]any{"name": "x", "label": "X", "fields": []any{map[string]any{"key": "k"}}}); err == nil {
		t.Fatal("field without label accepted")
	}
	if d := NewProviderConfigSchema("x", "X"); d.Storage != StorageFlatJSON {
		t.Fatalf("default storage %q", d.Storage)
	}
}

func TestRegistryPluginCLICommandsForTheActiveProviderOnly(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	setup := func(cmd *cobra.Command) { cmd.Flags().Bool("dry", false, "") }
	RegisterProvider(ProviderRegistration{Name: "climem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "climem"),
		New: regFactory("climem"), CLI: &ProviderCLI{Setup: setup}})
	ctx := context.Background()
	writeConfig(t, h, "memory:\n  provider: byterover\n")
	if cmds := DiscoverPluginCLICommands(ctx, nil); len(cmds) != 0 {
		t.Fatalf("inactive provider commands %+v", cmds)
	}
	writeConfig(t, h, "memory:\n  provider: climem\n")
	cmds := DiscoverPluginCLICommands(ctx, nil)
	if len(cmds) != 1 || cmds[0].Name != "climem" || cmds[0].Help != "Manage climem memory plugin" || cmds[0].Description != "" ||
		cmds[0].Plugin != "climem" || cmds[0].Setup == nil || cmds[0].Handler != nil {
		t.Fatalf("commands %+v", cmds)
	}
}

func TestRegistryWarmAndDiscoverBundled(t *testing.T) {
	isolateRegistry(t)
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	if WarmMemoryProvider(ctx, nil, "") {
		t.Fatal("warmed with no provider configured")
	}
	writeConfig(t, h, "memory:\n  provider: retaindb\n")
	if !WarmMemoryProvider(ctx, nil, "") || WarmMemoryProvider(ctx, nil, "nope") {
		t.Fatal("warm resolution wrong")
	}
	RegisterProvider(ProviderRegistration{Name: "usermem", Source: SourceUser, Home: h, Dir: filepath.Join(h, "plugins", "usermem"), New: regFactory("usermem")})
	if WarmMemoryProvider(ctx, nil, "usermem") {
		t.Fatal("user provider warmed in process")
	}
	var names []string
	for _, d := range DiscoverMemoryProviders(ctx, nil) {
		names = append(names, d.Name)
		if d.Name == "retaindb" && d.Description != "RetainDB — cloud memory API with hybrid search and 7 memory types." {
			t.Fatalf("retaindb description %q", d.Description)
		}
	}
	if strings.Join(names, ",") != "byterover,retaindb,usermem" {
		t.Fatalf("discovery order %v", names)
	}
}

func TestRegistryUnregisterProvidersBySourceAndHome(t *testing.T) {
	isolateRegistry(t)
	a, b := t.TempDir(), t.TempDir()
	RegisterProvider(ProviderRegistration{Name: "m", Source: SourceUser, Home: a, Dir: filepath.Join(a, "plugins", "m"), New: regFactory("m")})
	RegisterProvider(ProviderRegistration{Name: "m", Source: SourceUser, Home: a, Dir: filepath.Join(a, "plugins", "m"), New: regFactory("m")})
	RegisterProvider(ProviderRegistration{Name: "m", Source: SourceUser, Home: b, Dir: filepath.Join(b, "plugins", "m"), New: regFactory("m")})
	count := func() int {
		registryMu.RLock()
		defer registryMu.RUnlock()
		n := 0
		for _, r := range registrations {
			if r.Name == "m" {
				n++
			}
		}
		return n
	}
	if count() != 2 {
		t.Fatalf("duplicate registration kept, %d entries", count())
	}
	UnregisterProviders(SourceUser, a)
	if count() != 1 || FindProvider(home.WithOverride(context.Background(), b), nil, "m") == nil {
		t.Fatal("unregister removed the wrong home")
	}
}
