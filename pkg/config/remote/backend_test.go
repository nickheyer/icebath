package remote

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func captureStderr(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	prev := stderr
	stderr = &buf
	t.Cleanup(func() { stderr = prev })
	return &buf
}

func cmdSources() config.ConfigCommandSources {
	return config.ConfigCommandSources{
		IsEnvSettingKey: func(key string) bool {
			return !strings.Contains(key, ".") && key == strings.ToUpper(key) && !config.IsEnvConfigKey(key)
		},
		SaveEnvSetting: func(_ context.Context, h, key, value string) error {
			return config.SaveEnvValue(h, strings.ToUpper(key), value)
		},
		ReadEnvSetting: func(h, key string) (string, bool) { v, ok := config.LoadEnv(h)[strings.ToUpper(key)]; return v, ok },
		RemoveEnvSetting: func(_ context.Context, h, key string) (bool, error) {
			return config.RemoveHomeEnvValue(context.Background(), h, key)
		},
		DropStaleModelRoute: func(map[string]any, string, map[string]any) (map[string]any, bool) { return nil, false },
		RuntimeKeyOwners: config.RuntimeKeyOwners{
			IsPresetFieldPath:   func([]string) bool { return false },
			SelectionConfigKeys: func() map[string]bool { return nil },
			ActiveManifests:     func() []config.PluginConfigKeys { return nil },
		},
	}
}

func loadConfig(t *testing.T, p *stubPlane) config.Tree {
	t.Helper()
	tree, err := config.LoadContext(context.Background(), p.home, os.LookupEnv)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return tree
}

func writeKey(p *stubPlane, key string, value any) error {
	return config.WriteConfigKey(context.Background(), filepath.Join(p.home, "config.yaml"), key, value)
}

func setValue(p *stubPlane, key, value string) error {
	return config.SetConfigValue(context.Background(), p.home, key, value, false, cmdSources())
}

func mustEqualJSON(t *testing.T, label string, got, want any) {
	t.Helper()
	if !JSONEqual(got, want) {
		t.Fatalf("%s: got %#v want %#v", label, got, want)
	}
}

func TestSelectedFromEnvAndFileToolingOff(t *testing.T) {
	setupPlane(t)
	b, err := config.GetConfigBackend(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range b.ProtectedEnvNames() {
		found = found || n == "GATEWAY_RELAY_IDP_CLIENT_SECRET"
	}
	if b.Name() != "remote" || b.SupportsFileTooling() || b.HonorsManagedConfig() || !found {
		t.Fatalf("backend shape %s", b.Name())
	}
}

func TestReadsComeFromPlaneNotLocalFile(t *testing.T) {
	p := setupPlane(t)
	p.upper = map[string]any{"model": map[string]any{"default": "hermes-4", "provider": "nous"}, "terminal": map[string]any{"timeout": 180}}
	local := filepath.Join(p.home, "config.yaml")
	os.WriteFile(local, []byte(`{"model": {"default": "LOCAL"}, "terminal": {"timeout": 1}}`), 0o644)
	cfg := loadConfig(t, p)
	if cfg.Get("model.default") != "hermes-4" || cfg.Get("terminal.timeout") != 180 || cfg.Get("display") == nil {
		t.Fatalf("cfg %v %v", cfg.Get("model.default"), cfg.Get("terminal.timeout"))
	}
	if raw, _ := os.ReadFile(local); !strings.Contains(string(raw), "LOCAL") {
		t.Fatal("local file touched")
	}
	gets := p.gets()
	if len(gets) == 0 || gets[0].profile != "default" {
		t.Fatalf("gets %v", gets)
	}
	for _, g := range gets {
		if g.auth != "Bearer "+stubToken || g.instance != stubInstance {
			t.Fatalf("request %+v", g)
		}
	}
}

func TestBootRetriesThenExitsOnOutage(t *testing.T) {
	p := setupPlane(t)
	p.failStatus = 503
	err := testBackend().Boot(context.Background(), p.home)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(unavailable.SystemExitCode(), "does not start") {
		t.Fatalf("err %v", err)
	}
	if n := len(p.gets()); n != 3 {
		t.Fatalf("gets %d", n)
	}
}

func TestBootDoesNotRetryARefusal(t *testing.T) {
	p := setupPlane(t)
	t.Setenv(InstanceEnv, "someone-else")
	err := testBackend().Boot(context.Background(), p.home)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "config_agent_unknown") || len(p.gets()) != 1 {
		t.Fatalf("err %v gets %d", err, len(p.gets()))
	}
}

func TestBootRequiresInstanceID(t *testing.T) {
	p := setupPlane(t)
	os.Unsetenv(InstanceEnv)
	_, err := config.LoadContext(context.Background(), p.home, os.LookupEnv)
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), InstanceEnv) || len(p.requests) != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestBadIDPCredentialsFailClosed(t *testing.T) {
	p := setupPlane(t)
	t.Setenv(idpEnvPrefix+"CLIENT_SECRET", "wrong")
	_, err := config.LoadContext(context.Background(), p.home, os.LookupEnv)
	if err == nil || !strings.Contains(err.Error(), "IdP token request") || len(p.requests) != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestPartialIDPConfigIsNotRetried(t *testing.T) {
	p := setupPlane(t)
	os.Unsetenv(idpEnvPrefix + "CLIENT_SECRET")
	_, err := config.LoadContext(context.Background(), p.home, os.LookupEnv)
	if err == nil || !strings.Contains(err.Error(), "CLIENT_SECRET") || p.tokenRequests != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestConfigSetSendsOnlyTheChangedKey(t *testing.T) {
	p := setupPlane(t)
	p.upper = map[string]any{"model": map[string]any{"default": "hermes-4", "provider": "nous"}, "terminal": map[string]any{"timeout": 180}}
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %d", len(patches))
	}
	body := patches[0].body
	mustEqualJSON(t, "set", body["set"], map[string]any{"display": map[string]any{"personality": "pirate"}})
	if _, has := body["unset"]; has || body["expectedVersion"] != 0 {
		t.Fatalf("body %v", body)
	}
	if _, ok := body["writerConfigVersion"].(int); !ok {
		t.Fatalf("writer %v", body["writerConfigVersion"])
	}
	mustEqualJSON(t, "values", p.profile("default").values, map[string]any{"display": map[string]any{"personality": "pirate"}})
	if loadConfig(t, p).Get("display.personality") != "pirate" {
		t.Fatal("read back")
	}
	if _, err := os.Stat(filepath.Join(p.home, "config.yaml")); err == nil {
		t.Fatal("config.yaml written")
	}
}

func TestChangeUnderExistingSectionIsALeafSet(t *testing.T) {
	p := setupPlane(t)
	p.upper = map[string]any{"display": map[string]any{"personality": "concise", "compact": false}}
	if err := setValue(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "set", p.patches()[0].body["set"], map[string]any{"display.personality": "pirate"})
	mustEqualJSON(t, "values", p.profile("default").values, map[string]any{"display": map[string]any{"personality": "pirate"}})
}

func TestWriteConflictRereadsAndRetriesOnce(t *testing.T) {
	p := setupPlane(t)
	loadConfig(t, p)
	p.mu.Lock()
	p.profile("default").version = 5
	p.mu.Unlock()
	if err := writeKey(p, "display.personality", "pirate"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 2 || patches[0].body["expectedVersion"] != 0 || patches[1].body["expectedVersion"] != 5 || p.profile("default").version != 6 {
		t.Fatalf("patches %v", patches)
	}
}

func TestLockedKeyRefusedByConfigSetWithoutSending(t *testing.T) {
	p := setupPlane(t)
	p.upper = map[string]any{"terminal": map[string]any{"timeout": 180}}
	p.upperLocks = []map[string]any{{"path": "terminal", "level": "tenant"}}
	var notice bytes.Buffer
	prev := config.FacadeNoticeOut
	config.FacadeNoticeOut = &notice
	defer func() { config.FacadeNoticeOut = prev }()
	err := setValue(p, "terminal.timeout", "5")
	var exit *config.ConfigExit
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(notice.String(), "locked by the tenant level") || len(p.patches()) != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestLockedKeyRefusedByBackendSingleKeyWrite(t *testing.T) {
	p := setupPlane(t)
	p.upperLocks = []map[string]any{{"path": "terminal", "level": "tenant"}}
	err := writeKey(p, "terminal.backend", "local")
	var locked *config.ConfigLockedError
	if !errors.As(err, &locked) || locked.Path != "terminal" || locked.LockedBy != "tenant" || len(p.patches()) != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestR7ScalarOverLockedSubtreeRefused(t *testing.T) {
	p := setupPlane(t)
	p.upperLocks = []map[string]any{{"path": "a.b", "level": "tenant"}}
	var locked *config.ConfigLockedError
	if err := writeKey(p, "a", "scalar"); !errors.As(err, &locked) || len(p.patches()) != 0 {
		t.Fatalf("err %v", err)
	}
}

func TestBulkSaveStripsLockedKeysAndSendsTheRest(t *testing.T) {
	p := setupPlane(t)
	errOut := captureStderr(t)
	p.upper = map[string]any{"terminal": map[string]any{"timeout": 180}, "model": map[string]any{"default": "hermes-4"}}
	p.upperLocks = []map[string]any{{"path": "terminal", "level": "tenant"}}
	ctx := context.Background()
	doc, err := config.ReadRawConfig(ctx, p.home)
	if err != nil {
		t.Fatal(err)
	}
	doc["terminal"].(map[string]any)["timeout"] = 5
	doc["display"] = map[string]any{"personality": "pirate"}
	if err := config.SaveConfig(ctx, p.home, doc, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	if len(patches) != 1 {
		t.Fatalf("patches %d", len(patches))
	}
	mustEqualJSON(t, "set", patches[0].body["set"], map[string]any{"display": map[string]any{"personality": "pirate"}})
	if _, has := patches[0].body["set"].(map[string]any)["terminal.timeout"]; has {
		t.Fatal("locked key sent")
	}
	if !strings.Contains(errOut.String(), "locked by Remote Config were not saved: terminal") {
		t.Fatalf("stderr %q", errOut.String())
	}
}

func TestSecretLiteralRefusedClientSide(t *testing.T) {
	p := setupPlane(t)
	var verr *config.ConfigValueError
	if err := writeKey(p, "mcp_servers.github.env.GITHUB_TOKEN", "ghp_live"); !errors.As(err, &verr) || verr.Code != "config_secret_literal" {
		t.Fatalf("err %v", err)
	}
	if err := setValue(p, "delegation.api_key", "sk-live-abc"); err == nil || !strings.Contains(err.Error(), "secret-shaped") {
		t.Fatalf("err %v", err)
	}
	if len(p.patches()) != 0 {
		t.Fatal("sent")
	}
	if err := writeKey(p, "delegation.api_key", "${OPENROUTER_API_KEY}"); err != nil {
		t.Fatal(err)
	}
	patches := p.patches()
	mustEqualJSON(t, "set", patches[len(patches)-1].body["set"], map[string]any{"delegation": map[string]any{"api_key": "${OPENROUTER_API_KEY}"}})
}

func TestUnsetOfInheritedKeyWarns(t *testing.T) {
	p := setupPlane(t)
	logs := captureLog(t)
	p.upper = map[string]any{"display": map[string]any{"personality": "concise"}}
	p.profile("default").values = map[string]any{"display": map[string]any{"personality": "pirate"}}
	if err := writeKey(p, "display.personality", nil); err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "unset", p.patches()[0].body["unset"], []any{"display.personality"})
	if !strings.Contains(logs.String(), "still set by an upper level") {
		t.Fatalf("log %q", logs.String())
	}
}

func TestYAMLOnlyValuesAreConvertedBeforeTheDiff(t *testing.T) {
	p := setupPlane(t)
	big60 := new(big.Int).Lsh(big.NewInt(1), 60)
	if err := writeKey(p, "x.big", big60); err != nil {
		t.Fatal(err)
	}
	if err := writeKey(p, "x.big2", 1<<60); err != nil {
		t.Fatal(err)
	}
	sent := p.patches()
	mustEqualJSON(t, "first", sent[0].body["set"], map[string]any{"x": map[string]any{"big": big60.String()}})
	mustEqualJSON(t, "second", sent[1].body["set"], map[string]any{"x.big2": big60.String()})
	if err := writeKey(p, "x.nan", math.NaN()); err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Fatalf("err %v", err)
	}
}

func TestPollPicksUpChangesAndInvalidatesCaches(t *testing.T) {
	p := setupPlane(t)
	ctx := context.Background()
	p.upper = map[string]any{"display": map[string]any{"personality": "concise"}}
	if loadConfig(t, p).Get("display.personality") != "concise" {
		t.Fatal("first read")
	}
	b := testBackend()
	v1, _ := b.Version(ctx, p.home)
	b.pollAll()
	p.mu.Lock()
	last := p.requests[len(p.requests)-1]
	p.mu.Unlock()
	if v, _ := b.Version(ctx, p.home); last.ifNoneMatch == "" || !JSONEqual(v, v1) {
		t.Fatalf("unchanged poll moved the version")
	}
	p.mu.Lock()
	p.upper = map[string]any{"display": map[string]any{"personality": "pirate"}}
	p.mu.Unlock()
	b.pollAll()
	if v, _ := b.Version(ctx, p.home); JSONEqual(v, v1) {
		t.Fatal("version kept")
	}
	if loadConfig(t, p).Get("display.personality") != "pirate" {
		t.Fatal("poll not picked up")
	}
}

func TestConfigCacheVersionIsTheRemoteVersion(t *testing.T) {
	p := setupPlane(t)
	ctx := context.Background()
	cfg := filepath.Join(p.home, "config.yaml")
	p.upper = map[string]any{"display": map[string]any{"personality": "concise"}}
	v1, err := config.ConfigCacheVersion(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	b := testBackend()
	bv, _ := b.Version(ctx, p.home)
	none, _ := config.ConfigCacheVersionOrNone(ctx, cfg)
	if !JSONEqual(v1, bv) || !JSONEqual(v1, none) {
		t.Fatalf("versions %v %v %v", v1, bv, none)
	}
	p.mu.Lock()
	p.upper = map[string]any{"display": map[string]any{"personality": "pirate"}}
	p.mu.Unlock()
	b.pollAll()
	if v2, _ := config.ConfigCacheVersion(ctx, cfg); JSONEqual(v2, v1) {
		t.Fatal("cache version kept")
	}
}

func TestOutageWhileRunningKeepsInMemoryDoc(t *testing.T) {
	p := setupPlane(t)
	ctx := context.Background()
	p.upper = map[string]any{"display": map[string]any{"personality": "concise"}}
	loadConfig(t, p)
	b := testBackend()
	p.mu.Lock()
	p.failStatus = 503
	p.mu.Unlock()
	b.pollAll()
	if loadConfig(t, p).Get("display.personality") != "concise" || !strings.Contains(b.Describe(ctx, p.home), "503") {
		t.Fatal("outage dropped the doc")
	}
	p.mu.Lock()
	p.failStatus = 0
	p.mu.Unlock()
	b.pollAll()
	if strings.Contains(b.Describe(ctx, p.home), "last poll failed") {
		t.Fatal("error kept after recovery")
	}
}

func TestPollRefusalLogsErrorButKeepsRunning(t *testing.T) {
	p := setupPlane(t)
	loadConfig(t, p)
	levels := func(logs *bytes.Buffer) []string {
		var out []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "poll for profile") {
				_, rest, _ := strings.Cut(line, "level=")
				lvl, _, _ := strings.Cut(rest, " ")
				out = append(out, lvl)
			}
		}
		return out
	}
	p.mu.Lock()
	p.failStatus = 403
	p.mu.Unlock()
	logs := captureLog(t)
	testBackend().pollAll()
	if got := levels(logs); len(got) != 1 || got[0] != "ERROR" {
		t.Fatalf("levels %v", got)
	}
	p.mu.Lock()
	p.failStatus = 503
	p.mu.Unlock()
	logs.Reset()
	testBackend().pollAll()
	if got := levels(logs); len(got) != 1 || got[0] != "WARN" {
		t.Fatalf("levels %v", got)
	}
}

func TestADeadPollerIsRearmedByACachedRead(t *testing.T) {
	p := setupPlane(t)
	loadConfig(t, p)
	b := testBackend()
	dead := make(chan struct{})
	close(dead)
	b.mu.Lock()
	first := b.pollerDone
	b.pollerDone = dead
	b.mu.Unlock()
	loadConfig(t, p)
	b.mu.Lock()
	rearmed := b.pollerDone
	b.mu.Unlock()
	if rearmed == dead || rearmed == first || !b.pollerAlive() {
		t.Fatal("poller not rearmed")
	}
}

func TestPollIntervalIsAConstantNoEnvTunes(t *testing.T) {
	for _, value := range []string{"1", "600"} {
		t.Setenv("ICEBATH_CONFIG_REMOTE_POLL_SECONDS", value)
		if pollInterval() != 300 {
			t.Fatal("interval tuned")
		}
	}
	b := newRemoteBackend()
	names := append(append(b.BootstrapEnvNames(), b.DeploymentEnvNames()...), b.ProtectedEnvNames()...)
	if minPollSeconds != 30 || strings.Contains(strings.Join(names, ","), "POLL") {
		t.Fatal("poll knob exposed")
	}
}

func TestBackendEditsPathsWithTheDiffHelpers(t *testing.T) {
	doc := map[string]any{"a": map[string]any{"b": 1}}
	if !popPath(doc, Path{"a", "b"}) || !JSONEqual(doc, map[string]any{"a": map[string]any{}}) {
		t.Fatal("pop")
	}
	if popPath(doc, Path{"a", "b"}) || popPath(doc, Path{"x", "y"}) {
		t.Fatal("pop of an absent path")
	}
}

func TestNamedProfileFetchesItsOwnLayer(t *testing.T) {
	p := setupPlane(t)
	work := filepath.Join(p.home, "profiles", "work")
	os.MkdirAll(work, 0o755)
	p.profile("work").values = map[string]any{"display": map[string]any{"personality": "formal"}}
	tree, err := config.LoadContext(context.Background(), work, os.LookupEnv)
	if err != nil || tree.Get("display.personality") != "formal" {
		t.Fatalf("load %v %v", err, tree.Get("display.personality"))
	}
	for _, g := range p.gets() {
		if g.profile != "work" {
			t.Fatalf("profile %s fetched", g.profile)
		}
	}
}

func TestOldWriterVersionMigratesInMemoryOnly(t *testing.T) {
	p := setupPlane(t)
	latest := config.LatestConfigVersion()
	old := max(config.SupportFloorVersion, latest-1)
	prof := p.profile("default")
	prof.values, prof.version, prof.writer = map[string]any{"display": map[string]any{"personality": "pirate"}}, 1, old
	doc, err := config.ReadRawConfig(context.Background(), p.home)
	if err != nil {
		t.Fatal(err)
	}
	if doc["_config_version"] != latest || doc["display"].(map[string]any)["personality"] != "pirate" || len(p.patches()) != 0 {
		t.Fatalf("doc %v", doc)
	}
	if _, err := os.Stat(filepath.Join(p.home, "config.yaml")); err == nil {
		t.Fatal("config.yaml written")
	}
}

func TestMigrationStepsPersistIntoMemory(t *testing.T) {
	p := setupPlane(t)
	latest := config.LatestConfigVersion()
	prof := p.profile("default")
	prof.values, prof.version, prof.writer = map[string]any{"old_key": 1}, 1, latest-1
	saved := runMigrations
	runMigrations = func(ctx context.Context, h string, current int) {
		doc, err := config.ReadRawConfig(ctx, h)
		if err != nil {
			t.Error(err)
			return
		}
		doc["new_key"] = doc["old_key"]
		delete(doc, "old_key")
		b, _ := config.GetConfigBackend(ctx)
		if err := b.ApplyInMemory(ctx, h, doc); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { runMigrations = saved })
	doc, err := config.ReadRawConfig(context.Background(), p.home)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := doc["old_key"]; doc["new_key"] != 1 || has || len(p.patches()) != 0 {
		t.Fatalf("doc %v", doc)
	}
}

func TestUnknownTopLevelKeyWarnsOnce(t *testing.T) {
	p := setupPlane(t)
	logs := captureLog(t)
	p.upper = map[string]any{"from_the_future": map[string]any{"x": 1}}
	loadConfig(t, p)
	loadConfig(t, p)
	if n := strings.Count(logs.String(), "unknown config key 'from_the_future'"); n != 1 {
		t.Fatalf("warned %d times", n)
	}
}

func TestManagedConfigYAMLIsIgnoredAndFlagged(t *testing.T) {
	p := setupPlane(t)
	errOut := captureStderr(t)
	managed := filepath.Join(t.TempDir(), "etc-icebath")
	os.MkdirAll(managed, 0o755)
	os.WriteFile(filepath.Join(managed, "config.yaml"), []byte(`{"display": {"personality": "managed"}}`), 0o644)
	t.Setenv(config.EnvManagedDir, managed)
	p.upper = map[string]any{"display": map[string]any{"personality": "remote"}}
	if err := testBackend().Boot(context.Background(), p.home); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "IGNORED") || len(config.LoadManagedConfig()) != 0 {
		t.Fatalf("stderr %q", errOut.String())
	}
	if loadConfig(t, p).Get("display.personality") != "remote" {
		t.Fatal("managed config read")
	}
	if w := testBackend().DoctorWarnings(context.Background(), p.home); len(w) != 1 || w[0].Hint != "(remove it)" {
		t.Fatalf("doctor %v", w)
	}
}

func TestFileToolingRefused(t *testing.T) {
	setupPlane(t)
	err := config.RequireFileTooling(context.Background(), "Profile clone")
	var unavailable *config.ConfigBackendUnavailable
	if !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "Profile clone") {
		t.Fatalf("err %v", err)
	}
}
