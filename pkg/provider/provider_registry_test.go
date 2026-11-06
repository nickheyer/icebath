package provider

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
)

type prSample struct{ name string }

func (p *prSample) Name() string { return p.name }

type OrbitProvider interface {
	NamedProvider
	Orbit() string
}

type prOrbit struct{ name string }

func (p *prOrbit) Name() string  { return p.name }
func (p *prOrbit) Orbit() string { return "orbit" }

type prRecords struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *prRecords) Enabled(context.Context, slog.Level) bool { return true }
func (h *prRecords) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *prRecords) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *prRecords) WithGroup(string) slog.Handler      { return h }

// Level and message lines of every record
func (h *prRecords) lines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.recs {
		out = append(out, r.Level.String()+" "+r.Message)
	}
	return out
}

// Value of an attribute on the record at index i
func (h *prRecords) attr(i int, key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var got string
	h.recs[i].Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			got = a.Value.String()
		}
		return true
	})
	return got
}

// Registry of prSample providers logging into a record collector
func newPrRegistry(opts ProviderRegistryOptions) (*ProviderRegistry[*prSample], *prRecords) {
	recs := &prRecords{}
	if opts.Label == "" {
		opts.Label = "sample"
	}
	opts.Logger = slog.New(recs)
	return NewProviderRegistry[*prSample](opts), recs
}

// Fresh existing directory under the test temp dir
func prDir(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A raw dotted home path and its canonical key address one slot and one generation
func TestProviderScopeKeysScopedReadsAndRestoreShareCanonicalKey(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{Label: "sample"})
	ctx := context.Background()
	homeDir := prDir(t, "Profile")
	sep := string(filepath.Separator)
	raw := homeDir + sep + ".." + sep + filepath.Base(homeDir)
	key := home.Key(homeDir)
	t.Setenv("ICEBATH_HOME", homeDir)
	global, scoped, replacement := &prSample{"sample"}, &prSample{"sample"}, &prSample{"sample"}
	if err := registry.Register(global, ""); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(scoped, raw); err != nil {
		t.Fatal(err)
	}
	if registry.GetProvider(ctx, "sample", "") != scoped || registry.GetProvider(ctx, "sample", key) != scoped {
		t.Fatal("scoped read missed the raw path registration")
	}
	if registry.SnapshotRegistration("sample", "") != global || registry.SnapshotRegistration("sample", key) != scoped {
		t.Fatal("snapshot slots differ")
	}
	generation := registry.RegistryGeneration(ctx, key)
	if err := registry.Register(replacement, key); err != nil {
		t.Fatal(err)
	}
	if registry.RegistryGeneration(ctx, raw) == generation {
		t.Fatal("raw path generation did not move")
	}
	if !registry.RestoreRegistration("sample", replacement, nil, raw) {
		t.Fatal("restore through the raw path refused")
	}
	if registry.GetProvider(ctx, "sample", key) != global {
		t.Fatal("restore did not fall back to the global provider")
	}
}

// Wrong provider types and nil raise the TypeError text with the class article
func TestProviderRegistryRejectsWrongType(t *testing.T) {
	orbits := NewProviderRegistry[OrbitProvider](ProviderRegistryOptions{Label: "Orbit", Logger: slog.New(&prRecords{})})
	_, err := orbits.Scoped().Register(&prSample{"x"}, "")
	var typeErr *ProviderTypeError
	if !errors.As(err, &typeErr) || err.Error() != "register_provider() expects an OrbitProvider instance, got prSample" {
		t.Fatalf("orbit rejection %v", err)
	}
	err = orbits.Register(nil, "")
	if !errors.As(err, &typeErr) || err.Error() != "register_provider() expects an OrbitProvider instance, got NoneType" {
		t.Fatalf("nil rejection %v", err)
	}
	samples, _ := newPrRegistry(ProviderRegistryOptions{})
	if err := samples.Register(nil, ""); err == nil || err.Error() != "register_provider() expects a prSample instance, got NoneType" {
		t.Fatalf("typed nil rejection %v", err)
	}
	if _, err := samples.Scoped().Register(&prOrbit{"x"}, ""); err == nil || err.Error() != "register_provider() expects a prSample instance, got prOrbit" {
		t.Fatalf("sample rejection %v", err)
	}
	if len(samples.ListProviders(context.Background(), "")) != 0 || orbits.RegistryGeneration(context.Background(), "").Global != 0 {
		t.Fatal("rejected provider changed the registry")
	}
}

// A blank name raises the label ValueError text
func TestProviderRegistryRejectsEmptyName(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{Label: "Image gen"})
	for _, name := range []string{"", "  ", "\t　"} {
		err := registry.Register(&prSample{name}, "")
		var valueErr *ValueError
		if !errors.As(err, &valueErr) || err.Error() != "Image gen provider .name must be a non-empty string" {
			t.Fatalf("%q: %v", name, err)
		}
	}
}

// Builtin names call the collision hook and skip registration or return its error
func TestProviderRegistryBuiltinCollision(t *testing.T) {
	var seen []string
	warned, _ := newPrRegistry(ProviderRegistryOptions{Label: "TTS", Normalize: LowerKey, BuiltinNames: map[string]bool{"edge": true},
		OnBuiltinCollision: func(key string) error { seen = append(seen, key); return nil }})
	ctx := context.Background()
	if err := warned.Register(&prSample{" EDGE "}, ""); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(seen, []string{"edge"}) || warned.GetProvider(ctx, "edge", "") != nil || len(warned.ListProviders(ctx, "")) != 0 {
		t.Fatalf("collision registered or hook missed %v", seen)
	}
	if warned.RegistryGeneration(ctx, "") != (ProviderRegistryGeneration{}) {
		t.Fatal("collision bumped the generation")
	}
	rejectErr := errors.New("Terminal environment provider 'local' collides with a builtin backend")
	rejecting, _ := newPrRegistry(ProviderRegistryOptions{Label: "Terminal environment", Normalize: LowerKey,
		BuiltinNames: map[string]bool{"local": true}, OnBuiltinCollision: func(string) error { return rejectErr }})
	if err := rejecting.Register(&prSample{"Local"}, ""); err != rejectErr {
		t.Fatalf("collision error %v", err)
	}
	silent, recs := newPrRegistry(ProviderRegistryOptions{BuiltinNames: map[string]bool{"local": true}})
	if err := silent.Register(&prSample{"local"}, ""); err != nil || silent.GetProvider(ctx, "local", "") != nil || len(recs.lines()) != 0 {
		t.Fatalf("hookless collision %v %v", err, recs.lines())
	}
}

// Re-registration overwrites in place and logs the displaced type, the log label lowercased unless all caps
func TestProviderRegistryReregistrationOverwritesAndLogs(t *testing.T) {
	ctx := context.Background()
	registry, recs := newPrRegistry(ProviderRegistryOptions{Label: "Image gen"})
	first, second, other := &prSample{"fal"}, &prSample{" fal "}, &prSample{"openai"}
	for _, p := range []*prSample{first, other, second} {
		if err := registry.Register(p, ""); err != nil {
			t.Fatal(err)
		}
	}
	if registry.GetProvider(ctx, "fal", "") != second {
		t.Fatal("re-registration did not overwrite")
	}
	if keys := registry.Merged(ctx, "").Keys(); !slices.Equal(keys, []string{"fal", "openai"}) {
		t.Fatalf("overwrite moved the slot %v", keys)
	}
	want := []string{"DEBUG Registered image gen provider 'fal' (prSample)", "DEBUG Registered image gen provider 'openai' (prSample)",
		"DEBUG Image gen provider 'fal' re-registered (was 'prSample')"}
	if got := recs.lines(); !slices.Equal(got, want) {
		t.Fatalf("logs %q", got)
	}
	tts, ttsRecs := newPrRegistry(ProviderRegistryOptions{Label: "TTS"})
	_ = tts.Register(&prSample{"x"}, "")
	if got := ttsRecs.lines(); !slices.Equal(got, []string{"DEBUG Registered TTS provider 'x' (prSample)"}) {
		t.Fatalf("all caps label %q", got)
	}
}

// Empty labels fail construction as the label index does in hermes
func TestProviderRegistryEmptyLabelPanics(t *testing.T) {
	defer func() {
		if rec := recover(); rec != "string index out of range" {
			t.Fatalf("recovered %v", rec)
		}
	}()
	NewProviderRegistry[*prSample](ProviderRegistryOptions{})
}

// Scoped reads find the active profile registration first and fall back to the global layer
func TestProviderRegistryScopedGetFallsBackToGlobal(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{Normalize: LowerKey})
	a, b := prDir(t, "profile-a"), prDir(t, "profile-b")
	global, scoped := &prSample{"TestBox"}, &prSample{"testbox"}
	_ = registry.Register(global, "")
	_ = registry.Register(scoped, a)
	ctx := context.Background()
	if registry.GetProvider(ctx, " TESTBOX ", a) != scoped || registry.GetProvider(ctx, "testbox", b) != global {
		t.Fatal("explicit scope lookups")
	}
	t.Setenv("ICEBATH_HOME", b)
	if registry.GetProvider(ctx, "testbox", "") != global {
		t.Fatal("active home without registration missed the global provider")
	}
	if registry.GetProvider(home.WithOverride(ctx, a), "testbox", "") != scoped {
		t.Fatal("context home override did not select the scoped provider")
	}
	t.Setenv("ICEBATH_HOME", a)
	if registry.GetProvider(ctx, "testbox", "") != scoped || registry.GetProvider(ctx, "missing", "") != nil {
		t.Fatal("active home lookups")
	}
}

// Merged keeps global order with scoped overrides in place and scoped additions last, list sorts by name
func TestProviderRegistryMergedAndListOrder(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{})
	scope := prDir(t, "scope")
	ctx := context.Background()
	c, a, b, a2, d := &prSample{"c"}, &prSample{"a"}, &prSample{"b"}, &prSample{"a"}, &prSample{"d"}
	for _, p := range []*prSample{c, a, b} {
		_ = registry.Register(p, "")
	}
	_ = registry.Register(d, scope)
	_ = registry.Register(a2, scope)
	merged := registry.Merged(ctx, scope)
	if !slices.Equal(merged.Keys(), []string{"c", "a", "b", "d"}) || !slices.Equal(merged.Values(), []*prSample{c, a2, b, d}) {
		t.Fatalf("merged %v", merged.Keys())
	}
	merged.Delete("c")
	if registry.GetProvider(ctx, "c", "") != c {
		t.Fatal("merged is not a copy")
	}
	if got := registry.ListProviders(ctx, scope); !slices.Equal(got, []*prSample{a2, b, c, d}) {
		t.Fatalf("list %v", got)
	}
	t.Setenv("ICEBATH_HOME", prDir(t, "other"))
	if got := registry.ListProviders(ctx, ""); !slices.Equal(got, []*prSample{a, b, c}) {
		t.Fatalf("global list %v", got)
	}
}

// Register, restore and reset advance the generations of the layer they touch
func TestProviderRegistryGenerationBumps(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{})
	ctx := context.Background()
	scope := prDir(t, "scope")
	t.Setenv("ICEBATH_HOME", scope)
	gen := func(s string) ProviderRegistryGeneration { return registry.RegistryGeneration(ctx, s) }
	p, q := &prSample{"x"}, &prSample{"y"}
	_ = registry.Register(p, "")
	if gen("") != (ProviderRegistryGeneration{1, 0}) {
		t.Fatalf("global register %v", gen(""))
	}
	_ = registry.Register(q, scope)
	if gen(scope) != (ProviderRegistryGeneration{1, 1}) || gen("") != (ProviderRegistryGeneration{1, 1}) {
		t.Fatalf("scoped register %v %v", gen(scope), gen(""))
	}
	if registry.RestoreRegistration("x", q, nil, "") || gen(scope) != (ProviderRegistryGeneration{1, 1}) {
		t.Fatal("refused restore bumped")
	}
	if !registry.RestoreRegistration("y", q, nil, scope) || gen(scope) != (ProviderRegistryGeneration{1, 2}) {
		t.Fatalf("scoped restore %v", gen(scope))
	}
	if !registry.RestoreRegistration("x", p, nil, "") || gen(scope) != (ProviderRegistryGeneration{2, 2}) {
		t.Fatalf("global restore %v", gen(scope))
	}
	registry.ResetForTests()
	if gen(scope) != (ProviderRegistryGeneration{3, 0}) {
		t.Fatalf("reset %v", gen(scope))
	}
}

// Snapshots read the exact slot with no global fallback
func TestProviderRegistrySnapshotWithoutFallback(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{Normalize: LowerKey})
	scope := prDir(t, "scope")
	global := &prSample{"X"}
	_ = registry.Register(global, "")
	if registry.SnapshotRegistration(" x ", "") != global || registry.SnapshotRegistration("x", scope) != nil {
		t.Fatal("snapshot fell back or missed")
	}
	if registry.Export().ScopedProviders.Len() != 0 {
		t.Fatal("snapshot created a scope")
	}
}

// Restore refuses a replaced provider, restores the previous one and removes a slot for a nil previous
func TestProviderRegistryRestoreRefusesReplacedProvider(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{})
	ctx := context.Background()
	p1, p2 := &prSample{"testbox"}, &prSample{"testbox"}
	_ = registry.Register(p1, "")
	_ = registry.Register(p2, "")
	if registry.RestoreRegistration("testbox", p1, nil, "") || registry.GetProvider(ctx, "testbox", "") != p2 {
		t.Fatal("restore over a replaced provider")
	}
	if !registry.RestoreRegistration("testbox", p2, p1, "") || registry.GetProvider(ctx, "testbox", "") != p1 {
		t.Fatal("restore of the previous provider")
	}
	if !registry.RestoreRegistration("testbox", p1, nil, "") || registry.GetProvider(ctx, "testbox", "") != nil {
		t.Fatal("restore to an empty slot")
	}
	before := registry.RegistryGeneration(ctx, "")
	if !registry.RestoreRegistration("testbox", nil, nil, "") || registry.RegistryGeneration(ctx, "") == before {
		t.Fatal("nil current over an empty slot is restorable and bumps as in hermes")
	}
	scope := prDir(t, "scope")
	if registry.RestoreRegistration("testbox", p1, nil, scope) {
		t.Fatal("restore into an empty scope")
	}
	if slot, ok := registry.Export().ScopedProviders.Get(home.Key(scope)); !ok || slot.Len() != 0 {
		t.Fatal("refused scoped restore leaves the created empty scope as hermes does")
	}
}

// Restoring the last scoped provider drops the emptied scope
func TestProviderRegistryRestoreDropsEmptiedScope(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{})
	scope := prDir(t, "scope")
	p := &prSample{"x"}
	_ = registry.Register(p, scope)
	scoped := registry.Export().ScopedProviders
	if !scoped.Has(home.Key(scope)) {
		t.Fatal("scoped register did not create the scope")
	}
	if !registry.RestoreRegistration("x", p, nil, scope) || scoped.Len() != 0 {
		t.Fatalf("emptied scope kept %v", scoped.Keys())
	}
}

// Reset clears every layer in place so exported hooks observe it
func TestProviderRegistryResetForTests(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{})
	scope := prDir(t, "scope")
	api := registry.Export()
	_ = registry.Register(&prSample{"x"}, "")
	_ = registry.Register(&prSample{"y"}, scope)
	registry.ResetForTests()
	if api.Providers.Len() != 0 || api.ScopedProviders.Len() != 0 || registry.RegistryGeneration(context.Background(), scope).Scoped != 0 {
		t.Fatal("reset left registrations")
	}
	_ = registry.Register(&prSample{"z"}, "")
	if !api.Providers.Has("z") {
		t.Fatal("hook lost identity after reset")
	}
}

// Export binds the module API and the live providers, scoped providers and lock hooks
func TestProviderRegistryExport(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{})
	api := registry.Export()
	ctx := context.Background()
	scope := prDir(t, "scope")
	p, q := &prSample{"x"}, &prSample{"y"}
	if err := api.RegisterProvider(p, ""); err != nil {
		t.Fatal(err)
	}
	_ = api.RegisterProvider(q, scope)
	if got, _ := api.Providers.Get("x"); got != p {
		t.Fatal("providers hook")
	}
	if slot, _ := api.ScopedProviders.Get(home.Key(scope)); slot == nil || !slot.Has("y") {
		t.Fatal("scoped providers hook")
	}
	if api.GetProvider(ctx, "y", scope) != q || !slices.Equal(api.ListProviders(ctx, scope), []*prSample{p, q}) ||
		api.SnapshotRegistration("y", scope) != q || api.RegistryGeneration(ctx, scope) != (ProviderRegistryGeneration{1, 1}) {
		t.Fatal("bound reads")
	}
	if !api.RestoreRegistration("y", q, nil, scope) {
		t.Fatal("bound restore")
	}
	api.Lock.Lock()
	done := make(chan struct{})
	go func() {
		registry.GetProvider(ctx, "x", "")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("lock hook is not the registry lock")
	case <-time.After(50 * time.Millisecond):
	}
	api.Lock.Unlock()
	<-done
	api.ResetForTests()
	if api.Providers.Len() != 0 {
		t.Fatal("bound reset")
	}
}

// The scoped view drives register snapshot and restore the way the plugin context does
func TestProviderRegistryScopedRegistryAdapter(t *testing.T) {
	registry, _ := newPrRegistry(ProviderRegistryOptions{Normalize: LowerKey, BuiltinNames: map[string]bool{"edge": true}})
	var view plugins.ScopedRegistry = registry.Scoped()
	scope := prDir(t, "scope")
	p, plugin := &prSample{"Fal"}, &prSample{"fal"}
	_ = registry.Register(p, scope)
	if !view.Accepts(plugin) || view.Accepts(&prOrbit{"x"}) || view.Accepts(nil) || view.Accepts((*prSample)(nil)) {
		t.Fatal("accepts")
	}
	if view.ProviderName(plugin) != "fal" || view.ProviderName(&prOrbit{"x"}) != "" || view.Identity(plugin) != nil {
		t.Fatal("name or identity")
	}
	if view.Snapshot("missing", scope) != nil {
		t.Fatal("empty snapshot is not an untyped nil")
	}
	previous := view.Snapshot("fal", scope)
	if previous != p {
		t.Fatalf("snapshot %v", previous)
	}
	if ok, err := view.Register(plugin, scope); !ok || err != nil || view.Snapshot("fal", scope) != plugin {
		t.Fatalf("register %v %v", ok, err)
	}
	if ok, err := view.Register(&prSample{"EDGE"}, scope); ok || err != nil {
		t.Fatalf("builtin collision %v %v", ok, err)
	}
	if ok, err := view.Register(&prOrbit{"x"}, scope); ok || err == nil {
		t.Fatalf("foreign provider %v %v", ok, err)
	}
	if view.Restore("fal", plugin, &prOrbit{"fal"}, scope) || view.Restore("fal", &prOrbit{"fal"}, nil, scope) {
		t.Fatal("restore accepted a foreign value")
	}
	if !view.Restore("fal", plugin, previous, scope) || registry.SnapshotRegistration("fal", scope) != p {
		t.Fatal("restore of the snapshot")
	}
	if !view.Restore("fal", p, nil, scope) || registry.Export().ScopedProviders.Len() != 0 {
		t.Fatal("restore to empty")
	}
}

// Names are stripped then lowercased with Python case mapping
func TestLowerKey(t *testing.T) {
	for in, want := range map[string]string{"  OpenAI \t": "openai", "ElevenLabs": "elevenlabs", "　ÉDGE\u0085": "édge", "": ""} {
		if got := LowerKey(in); got != want {
			t.Errorf("LowerKey(%q) = %q want %q", in, got, want)
		}
	}
}

type prProbe struct {
	name  string
	avail bool
	boom  any
}

func (p *prProbe) Name() string { return p.name }
func (p *prProbe) IsAvailable() bool {
	if p.boom != nil {
		panic(p.boom)
	}
	return p.avail
}

// A panicking availability probe reads unavailable and logs at the given level with its traceback on request
func TestIsAvailableSafe(t *testing.T) {
	recs := &prRecords{}
	logger := slog.New(recs)
	if !IsAvailableSafe(&prProbe{name: "fal", avail: true}, logger, "image_gen provider %s.is_available() raised %s", slog.LevelDebug, false) {
		t.Fatal("available provider")
	}
	if IsAvailableSafe(&prProbe{name: "fal"}, logger, "image_gen provider %s.is_available() raised %s", slog.LevelDebug, false) {
		t.Fatal("unavailable provider")
	}
	if len(recs.lines()) != 0 {
		t.Fatalf("healthy probes logged %q", recs.lines())
	}
	if IsAvailableSafe(&prProbe{name: "bad", boom: errors.New("boom")}, logger, "image_gen provider %s.is_available() raised %s", slog.LevelDebug, false) {
		t.Fatal("raising provider available")
	}
	if IsAvailableSafe(&prProbe{name: "cloud", boom: "kaput"}, logger, "Browser provider %s.is_available() raised %s — treating as unavailable", slog.LevelWarn, true) {
		t.Fatal("raising browser provider available")
	}
	want := []string{"DEBUG image_gen provider bad.is_available() raised boom", "WARN Browser provider cloud.is_available() raised kaput — treating as unavailable"}
	if got := recs.lines(); !slices.Equal(got, want) {
		t.Fatalf("logs %q", got)
	}
	if recs.attr(0, "exc_info") != "" {
		t.Fatal("traceback without exc_info")
	}
	if tb := recs.attr(1, "exc_info"); !strings.HasPrefix(tb, "kaput\n") || !strings.Contains(tb, "IsAvailable") {
		t.Fatalf("traceback %q", tb)
	}
}

// Configured provider names come stripped from the read only config with nous read as fal and failures as none
func TestConfiguredProviderName(t *testing.T) {
	ctx := context.Background()
	write := func(body string) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_HOME", dir)
	}
	logger := slog.New(&prRecords{})
	for body, want := range map[string]string{
		"image_gen:\n  provider: '  openai  '\n": "openai",
		"image_gen:\n  provider: '   '\n":        "",
		"image_gen:\n  provider: Nous\n":         "fal",
		"image_gen:\n  provider: nous\n":         "fal",
		"image_gen:\n  model: x\n":               "",
		"image_gen: openai\n":                    "",
		"image_gen:\n  provider: 7\n":            "",
	} {
		write(body)
		if got := ConfiguredProviderName(ctx, "image_gen", logger); got != want {
			t.Errorf("%q: %q want %q", body, got, want)
		}
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", filepath.Join(blocker, "home"))
	recs := &prRecords{}
	if got := ConfiguredProviderName(ctx, "video_gen", slog.New(recs)); got != "" {
		t.Fatalf("unreadable config %q", got)
	}
	if lines := recs.lines(); len(lines) != 1 || !strings.HasPrefix(lines[0], "DEBUG Could not read video_gen.provider from config: ") {
		t.Fatalf("logs %q", lines)
	}
}
