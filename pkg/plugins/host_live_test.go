package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const probePlugin = `
import json, os
from agent.image_gen_provider import ImageGenProvider

class Painter(ImageGenProvider):
    @property
    def name(self):
        return "hostpainter"
    def generate(self, prompt, aspect_ratio="landscape", **kw):
        return {"success": True, "image": f"{prompt}@{os.getpid()}"}

SEEN = []

def register(ctx):
    ctx.register_tool(name="hostprobe_pid", toolset="hostprobe", schema={"name": "hostprobe_pid",
        "description": "d", "parameters": {"type": "object", "properties": {}}},
        handler=lambda args, **kw: json.dumps({"pid": os.getpid(), "seen": SEEN, "x": args.get("x")}))
    ctx.register_tool(name="hostprobe_nested", toolset="hostprobe", schema={"name": "hostprobe_nested",
        "description": "d", "parameters": {"type": "object", "properties": {}}},
        handler=lambda args, **kw: ctx.dispatch_tool("hostprobe_pid", {"x": "nested"}))
    ctx.register_tool(name="hostprobe_crash", toolset="hostprobe", schema={"name": "hostprobe_crash",
        "description": "d", "parameters": {"type": "object", "properties": {}}},
        handler=lambda args, **kw: os._exit(3))
    ctx.register_hook("post_tool_call", lambda tool_name=None, **kw: SEEN.append(tool_name))
    ctx.register_image_gen_provider(Painter())
`

const platformPlugin = `
def register(ctx):
    ctx.register_platform("x", "X", adapter_factory=lambda cfg: None, check_fn=lambda: True)
`

const secretSourcePlugin = `
import os
from agent.secret_sources.base import ErrorKind, FetchResult, SecretSource

class DummyVault(SecretSource):
    name = "hostvault"
    label = "Host vault"

    def fetch(self, cfg, home_path):
        if cfg.get("mode") == "locked":
            return FetchResult().fail("vault is locked", ErrorKind.AUTH_FAILED)
        return FetchResult(secrets={"HOSTED_DUMMY_TOKEN": f"dummy-from-{os.getpid()}"},
                           warnings=["dummy warning"], binary_path=home_path / "bin" / "vault")

def register(ctx):
    ctx.register_secret_source(DummyVault())
`

const contextReferencePlugin = `
from agent.context_references import ContextCompletionItem, ContextReferenceProvider

class Tickets(ContextReferenceProvider):
    prefix = "hostticket"
    description = "tickets"

    async def autocomplete(self, query, *, limit=10):
        return [ContextCompletionItem(text=f"{query}-1", meta="open"), ContextCompletionItem(text=f"{query}-2")]

    async def expand(self, target):
        return f"ticket {target}"

def register(ctx):
    ctx.register_context_reference(Tickets())
`

const contextEnginePlugin = `
from agent.context_engine import ContextEngine

class Ledger(ContextEngine):
    @property
    def name(self):
        return "hostledger"
    def update_from_response(self, usage):
        self.last_prompt_tokens = usage["prompt_tokens"]
    def should_compress(self, prompt_tokens=None):
        return False
    def compress(self, messages, current_tokens=None, **kw):
        self.compress_options = {"current_tokens": current_tokens, **kw}
        return messages[-1:]

def register(ctx):
    ctx.register_context_engine(Ledger())
`

const sdkPlugin = `
import json, os
from agent.redact import redact_sensitive_text
from icebath_cli.config import get_env_value
from icebath_constants import get_icebath_home
from agent.secret_scope import get_secret
from providers import list_providers, register_provider
from providers.base import ProviderProfile
from tools.environments.local import build_subprocess_env

register_provider(ProviderProfile(name="sdkprobe", aliases=("sdkp",)))

def register(ctx):
    ctx.register_tool(name="sdkprobe", toolset="sdkprobe", schema={"name": "sdkprobe",
        "description": "d", "parameters": {"type": "object", "properties": {}}},
        handler=lambda args, **kw: json.dumps({
            "redacted": redact_sensitive_text("key sk-ant-api03-" + "a" * 90, force=True),
            "env": get_env_value("SDK_PROBE_VALUE"), "home": str(get_icebath_home()),
            "secret": get_secret("SDK_PROBE_VALUE"), "providers": [p.name for p in list_providers()],
            "child_env": sorted(k for k in build_subprocess_env(extra={"SDK_EXTRA": "1"}) if k.startswith("SDK_"))}))
`

// Scoped registry over hosted objects of one provider base, recording each registration
type hostedRegistry struct {
	base string
	mu   sync.Mutex
	got  map[string]any
}

func (r *hostedRegistry) Accepts(p any) bool {
	o, ok := p.(*HostedObject)
	return ok && o.Base == r.base
}

func (r *hostedRegistry) ProviderName(p any) string {
	if v, err := p.(*HostedObject).Get(context.Background(), "name"); err == nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
func (r *hostedRegistry) Identity(any) error       { return nil }
func (r *hostedRegistry) Snapshot(_, _ string) any { return nil }
func (r *hostedRegistry) Register(p any, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got[r.ProviderName(p)] = p
	return true, nil
}
func (r *hostedRegistry) Restore(_ string, _, _ any, _ string) bool { return true }

type liveServices struct {
	images, secrets *hostedRegistry
	refs            []any
}

// Installs services that adapt hosted objects the way the top level wiring does, restored at cleanup
func installLiveServices(t *testing.T) *liveServices {
	t.Helper()
	ls := &liveServices{images: &hostedRegistry{base: HostObjectBases["register_image_gen_provider"], got: map[string]any{}},
		secrets: &hostedRegistry{base: HostObjectBases["register_secret_source"], got: map[string]any{}}}
	previous := installedServices()
	InstallServices(&Services{
		ProviderRegistries: map[string]ScopedRegistry{"image_gen_provider": ls.images, "secret_source": ls.secrets},
		HostObject: func(kind string, obj any) (string, bool) {
			o, ok := obj.(*HostedObject)
			if !ok {
				return "", false
			}
			v, _ := o.Get(context.Background(), "name")
			s, _ := v.(string)
			return s, true
		},
		RegisterContextReference: func(p any) error { ls.refs = append(ls.refs, p); return nil },
		BindProfileSecretScope: func(ctx context.Context, h string) context.Context {
			return context.WithValue(ctx, liveSecretScopeKey{}, liveSecretScope{home: h, mapping: hostTestSecrets{}.BuildProfileSecretScope(h)})
		},
		GetSecret: liveGetSecret,
		DispatchTool: func(ctx context.Context, scope, name string, args, _ map[string]any) (string, error) {
			return tools.Default.Lookup(ctx, scope, name).Handler(ctx, &tools.Call{Name: name, Args: args}), nil
		},
	})
	t.Cleanup(func() { InstallServices(previous) })
	return ls
}

var hostEnvHooksOnce sync.Once

// Installs the tools env hooks the profiles and gateway ports install in production, valued as a hermes test process sees them
func installHostEnvHooks() {
	hostEnvHooksOnce.Do(func() {
		config.RegisterConfigFacade(config.FacadeHooks{CoreDeclaredEnvNames: config.StaticCoreEnvNames})
		tools.ConfigEnvTableKeys = func() []string { return []string{"GATEWAY_RELAY_URL"} }
		tools.RegisteredAdapterSecretEnv = func(env *tools.Env) map[string]bool {
			h := home.Process()
			if env != nil && env.Home != "" {
				h = env.Home
			}
			names, err := tools.HomeAdapterSecretEnv(h)
			if err != nil {
				panic(err)
			}
			return names
		}
		if tools.ProfileSecrets == nil {
			tools.ProfileSecrets = hostTestSecrets{}
		}
	})
}

type liveSecretScopeKey struct{}

type liveSecretScope struct {
	home    string
	mapping map[string]string
}

// get_secret over the bound scope, a miss reading the process env only for the launch home
func liveGetSecret(ctx context.Context, name string) (string, bool, error) {
	if tools.IsGlobalEnv(name) {
		v, ok := os.LookupEnv(name)
		return v, ok, nil
	}
	bound, ok := ctx.Value(liveSecretScopeKey{}).(liveSecretScope)
	if !ok {
		v, ok := os.LookupEnv(name)
		return v, ok, nil
	}
	if v, ok := bound.mapping[name]; ok {
		return v, true, nil
	}
	if tools.MultiplexActive() || home.Key(bound.home) != home.Key(home.Process()) {
		return "", false, nil
	}
	v, found := os.LookupEnv(name)
	return v, found, nil
}

// Profile secret scope of a home with no secret sources or managed env, its .env values only
type hostTestSecrets struct{}

func (hostTestSecrets) ManagedDotenvKeys() []string   { return nil }
func (hostTestSecrets) SourceSuppliedNames() []string { return nil }
func (hostTestSecrets) BuildProfileSecretScope(profileHome string) map[string]string {
	if secrets := config.LoadEnvFile(filepath.Join(profileHome, ".env")); secrets != nil {
		return secrets
	}
	return map[string]string{}
}
func (hostTestSecrets) CurrentSecretScope(context.Context) map[string]string { return nil }

// Interpreters the live tests launch the child with, the system python3 and the pm selected one when it exists
func liveInterpreters(t *testing.T) map[string]string {
	t.Helper()
	system, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	out := map[string]string{"system": system}
	if pmPython, err := pluginInterpreter(context.Background()); err == nil && isFile(pmPython) && pmPython != system {
		out["pm"] = pmPython
	}
	return out
}

// Temp home holding directory plugins enabled under host isolation, with the child launched by python
func liveHome(t *testing.T, python string, plugins map[string]string) (string, *Manager) {
	t.Helper()
	return liveHomeIsolated(t, python, plugins, IsolationHost)
}

// Temp home holding directory plugins enabled under the given isolation, with the child launched by python
func liveHomeIsolated(t *testing.T, python string, plugins map[string]string, isolation string) (string, *Manager) {
	t.Helper()
	return liveHomeLoadedIn(t, python, plugins, isolation, nil)
}

// Temp home like liveHomeIsolated whose first discovery runs in the context wrap derives from the home context
func liveHomeLoadedIn(t *testing.T, python string, plugins map[string]string, isolation string, wrap func(context.Context) context.Context) (string, *Manager) {
	t.Helper()
	installHostEnvHooks()
	root := t.TempDir()
	h := filepath.Join(root, ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for name, body := range plugins {
		dir := filepath.Join(h, "plugins", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "plugin.yaml"), "name: "+name+"\nversion: '1.0'\nkind: standalone\n")
		writeFile(t, filepath.Join(dir, "__init__.py"), body)
		names = append(names, name)
	}
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  isolation: "+isolation+"\n  enabled: ["+strings.Join(names, ", ")+"]\n")
	t.Setenv("HOME", filepath.Join(root, "os-home"))
	t.Setenv("ICEBATH_HOME", h)
	previous := hostInterpreter
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	m := NewManager(h)
	t.Cleanup(func() {
		if m.pluginHostInstance != nil {
			m.pluginHostInstance.Shutdown()
		}
	})
	loadCtx := homeCtx(h)
	if wrap != nil {
		loadCtx = wrap(loadCtx)
	}
	m.DiscoverAndLoad(loadCtx, false)
	return h, m
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dispatchLive(t *testing.T, h, name string, args map[string]any) string {
	t.Helper()
	def := tools.Default.Lookup(homeCtx(h), h, name)
	if def == nil {
		t.Fatalf("tool %s is not registered", name)
	}
	return def.Handler(homeCtx(h), &tools.Call{Name: name, Args: args})
}

func loadedError(m *Manager, key string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p := m.plugins[key]; p != nil {
		return p.Error
	}
	return "<not loaded>"
}

// Port of test_plugin_runs_out_of_process_with_ctx_round_trips_and_survives_host_crash
func TestPluginRunsOutOfProcessWithCtxRoundTripsAndSurvivesHostCrash(t *testing.T) {
	for label, python := range liveInterpreters(t) {
		t.Run(label, func(t *testing.T) {
			ls := installLiveServices(t)
			h, m := liveHome(t, python, map[string]string{"hostprobe": probePlugin, "platformprobe": platformPlugin})
			if e := loadedError(m, "hostprobe"); e != "" {
				t.Fatalf("hostprobe failed to load: %s", e)
			}
			var first map[string]any
			if err := json.Unmarshal([]byte(dispatchLive(t, h, "hostprobe_pid", map[string]any{"x": 1})), &first); err != nil {
				t.Fatal(err)
			}
			hostPID := first["pid"].(float64)
			if int(hostPID) == os.Getpid() || first["x"].(float64) != 1 {
				t.Fatalf("tool ran in process or lost its args: %v", first)
			}
			var nested map[string]any
			_ = json.Unmarshal([]byte(dispatchLive(t, h, "hostprobe_nested", nil)), &nested)
			if nested["pid"] != hostPID || nested["x"] != "nested" {
				t.Fatalf("nested ctx.dispatch_tool = %v", nested)
			}
			m.InvokeHook(homeCtx(h), "post_tool_call", map[string]any{"tool_name": "read_file", "args": map[string]any{}, "result": "ok"})
			if !strings.Contains(dispatchLive(t, h, "hostprobe_pid", nil), "read_file") {
				t.Fatal("post_tool_call hook did not run in the host")
			}
			painter, ok := ls.images.got["hostpainter"].(*HostedObject)
			if !ok {
				t.Fatalf("image provider not registered as a hosted object: %v", ls.images.got)
			}
			image, err := painter.Call(homeCtx(h), "generate", []any{"cat"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]any{"success": true, "image": "cat@" + jsonNumber(hostPID)}
			if got := image.(map[string]any); got["success"] != true || got["image"] != want["image"] {
				t.Fatalf("generate = %v", got)
			}
			if e := loadedError(m, "platformprobe"); !strings.Contains(e, "register_platform") {
				t.Fatalf("platformprobe error = %q", e)
			}
			if crashed := dispatchLive(t, h, "hostprobe_crash", nil); !strings.Contains(crashed, "plugin host") {
				t.Fatalf("crash result = %s", crashed)
			}
			deadline, result := time.Now().Add(15*time.Second), ""
			for time.Now().Before(deadline) {
				if tools.Default.Lookup(homeCtx(h), h, "hostprobe_pid") == nil {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				if result = dispatchLive(t, h, "hostprobe_pid", nil); strings.Contains(result, `"pid"`) {
					break
				}
				time.Sleep(200 * time.Millisecond)
			}
			var after map[string]any
			if err := json.Unmarshal([]byte(result), &after); err != nil || after["pid"] == hostPID || int(after["pid"].(float64)) == os.Getpid() {
				t.Fatalf("host did not restart: %s", result)
			}
		})
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// Port of test_hosted_secret_source_secrets_are_applied at the hosted object boundary
func TestLiveHostedSecretSourceFetchRestoresFetchResult(t *testing.T) {
	ls := installLiveServices(t)
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"hostvault": secretSourcePlugin})
	if e := loadedError(m, "hostvault"); e != "" {
		t.Fatalf("hostvault failed to load: %s", e)
	}
	source := ls.secrets.got[""]
	if source == nil {
		for _, v := range ls.secrets.got {
			source = v
		}
	}
	vault, ok := source.(*HostedObject)
	if !ok {
		t.Fatalf("secret source not hosted: %v", ls.secrets.got)
	}
	got, err := vault.Call(homeCtx(h), "fetch", []any{map[string]any{}, h}, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, ok := got.(*Dataclass)
	if !ok || result.Type != "FetchResult" {
		t.Fatalf("fetch = %#v", got)
	}
	secret := result.Fields["secrets"].(map[string]any)["HOSTED_DUMMY_TOKEN"].(string)
	if !strings.HasPrefix(secret, "dummy-from-") || secret == "dummy-from-"+jsonNumber(float64(os.Getpid())) {
		t.Fatalf("secret = %q", secret)
	}
	if w := result.Fields["warnings"].([]any); len(w) != 1 || w[0] != "dummy warning" {
		t.Fatalf("warnings = %v", w)
	}
	if result.Fields["binary_path"] != filepath.Join(h, "bin", "vault") {
		t.Fatalf("binary_path = %v", result.Fields["binary_path"])
	}
	got, err = vault.Call(homeCtx(h), "fetch", []any{map[string]any{"mode": "locked"}, h}, nil)
	if err != nil {
		t.Fatal(err)
	}
	locked := got.(*Dataclass)
	if locked.Fields["error"] != "vault is locked" || locked.Fields["error_kind"] != "auth_failed" || len(locked.Fields["secrets"].(map[string]any)) != 0 {
		t.Fatalf("locked = %#v", locked.Fields)
	}
}

// Port of test_hosted_context_reference_autocomplete_returns_items
func TestLiveHostedContextReferenceAutocompleteReturnsItems(t *testing.T) {
	ls := installLiveServices(t)
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"hostticket": contextReferencePlugin})
	if e := loadedError(m, "hostticket"); e != "" || len(ls.refs) != 1 {
		t.Fatalf("hostticket load error %q refs %v", e, ls.refs)
	}
	tickets := ls.refs[0].(*HostedObject)
	if !tickets.MethodIsAsync("autocomplete") {
		t.Fatal("autocomplete lost its async flag")
	}
	got, err := tickets.Call(homeCtx(h), "autocomplete", []any{"bug"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := got.([]any)
	first, second := items[0].(*Dataclass), items[1].(*Dataclass)
	if len(items) != 2 || first.Type != "ContextCompletionItem" || first.Fields["text"] != "bug-1" || first.Fields["meta"] != "open" ||
		second.Fields["text"] != "bug-2" || second.Fields["display"] != "bug-2" || second.Fields["meta"] != "" {
		t.Fatalf("autocomplete = %#v %#v", first, second)
	}
	if expanded, err := tickets.Call(homeCtx(h), "expand", []any{"T-1"}, nil); err != nil || expanded != "ticket T-1" {
		t.Fatalf("expand = %v %v", expanded, err)
	}
}

// Port of test_hosted_context_engine_clone_is_an_independent_engine
func TestLiveHostedContextEngineCloneIsAnIndependentEngine(t *testing.T) {
	installLiveServices(t)
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"hostledger": contextEnginePlugin})
	if e := loadedError(m, "hostledger"); e != "" {
		t.Fatalf("hostledger failed to load: %s", e)
	}
	ctx := homeCtx(h)
	shared := m.contextEngine.(*HostedObject)
	cloned, err := shared.Call(ctx, "clone_for_agent", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	clone, ok := cloned.(*HostedObject)
	if !ok || clone.Base != HostObjectBases["register_context_engine"] {
		t.Fatalf("clone_for_agent = %#v", cloned)
	}
	if name, _ := clone.Get(ctx, "name"); name != "hostledger" {
		t.Fatalf("clone name = %v", name)
	}
	if _, err := clone.Call(ctx, "update_from_response", []any{map[string]any{"prompt_tokens": 1234}}, nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := clone.Get(ctx, "last_prompt_tokens"); v != int64(1234) {
		t.Fatalf("clone last_prompt_tokens = %#v", v)
	}
	if v, _ := shared.Get(ctx, "last_prompt_tokens"); v != int64(0) {
		t.Fatalf("shared last_prompt_tokens = %#v", v)
	}
	messages := []any{map[string]any{"role": "user", "content": "one"}, map[string]any{"role": "user", "content": "two"}}
	got, err := clone.Call(ctx, "compress", []any{messages}, map[string]any{"current_tokens": 900, "focus_topic": "auth", "force": true})
	if err != nil || len(got.([]any)) != 1 {
		t.Fatalf("compress = %v %v", got, err)
	}
	opts, _ := clone.Get(ctx, "compress_options")
	if o := opts.(map[string]any); o["current_tokens"] != int64(900) || o["focus_topic"] != "auth" || o["force"] != true {
		t.Fatalf("compress_options = %v", opts)
	}
}

// SDK modules import in the child and reach Icebath for redaction, env values, child envs and profiles
func TestLivePluginSDKCallsReachIcebath(t *testing.T) {
	installLiveServices(t)
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{"sdkprobe": sdkPlugin})
	writeFile(t, filepath.Join(h, ".env"), "SDK_PROBE_VALUE=from-dotenv\n")
	if e := loadedError(m, "sdkprobe"); e != "" {
		t.Fatalf("sdkprobe failed to load: %s", e)
	}
	var got map[string]any
	out := dispatchLive(t, h, "sdkprobe", nil)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(got["redacted"].(string), strings.Repeat("a", 90)) {
		t.Fatalf("redaction did not mask the key: %v", got["redacted"])
	}
	if got["env"] != "from-dotenv" || got["home"] != h {
		t.Fatalf("env/home = %v %v", got["env"], got["home"])
	}
	if p := got["providers"].([]any); len(p) != 1 || p[0] != "sdkprobe" {
		t.Fatalf("providers = %v", p)
	}
	if e := got["child_env"].([]any); len(e) == 0 || e[len(e)-1] != "SDK_EXTRA" {
		t.Fatalf("child env = %v", e)
	}
}

// Port of test_managed_scope_pins_host_isolation_over_the_profiles_own_config
func TestManagedScopePinsHostIsolationOverTheProfilesOwnConfig(t *testing.T) {
	managed := t.TempDir()
	writeFile(t, filepath.Join(managed, "config.yaml"), "plugins:\n  isolation: host\n")
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	installLiveServices(t)
	h, m := liveHomeIsolated(t, liveInterpreters(t)["system"], map[string]string{"hostprobe": probePlugin}, IsolationInProcess)
	if e := loadedError(m, "hostprobe"); e != "" {
		t.Fatalf("hostprobe failed to load: %s", e)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(dispatchLive(t, h, "hostprobe_pid", nil)), &result); err != nil {
		t.Fatal(err)
	}
	if pid, _ := result["pid"].(float64); pid == 0 || int(pid) == os.Getpid() {
		t.Fatalf("plugin did not run in the host: %v", result)
	}
}

const modelProviderPlugin = `
import os
from providers import register_provider
from providers.base import ProviderProfile

class HostModel(ProviderProfile):
    def build_extra_body(self, *, session_id=None, **context):
        return {"pid": os.getpid(), "session_id": session_id}

register_provider(HostModel(name="hostmodel", base_url="https://hostmodel.example/v1", env_vars=("HOSTMODEL_KEY",)))
`

// Port of test_model_provider_profile_data_is_local_and_overrides_run_in_the_host
func TestModelProviderProfileDataIsLocalAndOverridesRunInTheHost(t *testing.T) {
	h, m := liveHome(t, liveInterpreters(t)["system"], map[string]string{})
	pluginDir := filepath.Join(h, "plugins", "model-providers", "hostmodel")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, "plugin.yaml"), "name: hostmodel\nkind: model-provider\n")
	writeFile(t, filepath.Join(pluginDir, "__init__.py"), modelProviderPlugin)
	ctx := homeCtx(h)
	profiles, inProcess, err := LoadHostedProfiles(ctx, pluginDir, "_icebath_user_provider_test_hostmodel")
	if err != nil || len(profiles) != 1 || len(inProcess) != 0 {
		t.Fatalf("profiles %v in process %v err %v", profiles, inProcess, err)
	}
	profile := profiles[0]
	envVars, _ := profile.Fields["env_vars"].([]any)
	if profile.Name != "hostmodel" || profile.Fields["base_url"] != "https://hostmodel.example/v1" || len(envVars) != 1 || envVars[0] != "HOSTMODEL_KEY" {
		t.Fatalf("profile data = %s %v", profile.Name, profile.Fields)
	}
	if cached, _ := filepath.Glob(filepath.Join(h, "cache", "plugin_host", "model-providers", "hostmodel-*.json")); len(cached) != 1 {
		t.Fatalf("extraction cache = %v", cached)
	}
	if m.pluginHostInstance != nil && m.pluginHostInstance.PID() != 0 {
		t.Fatal("discovery started a host for profile data")
	}
	t.Cleanup(func() {
		if mgr := GetPluginManager(ctx); mgr != nil && mgr.pluginHostInstance != nil {
			mgr.pluginHostInstance.Shutdown()
		}
	})
	body, err := profile.Call(ctx, "build_extra_body", nil, map[string]any{"session_id": "s1"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := body.(map[string]any)
	if pid, _ := got["pid"].(int64); got["session_id"] != "s1" || pid == 0 || int(pid) == os.Getpid() {
		t.Fatalf("build_extra_body = %v", body)
	}
}

// Port of test_profile_extraction_ignores_a_hermes_cli_package_in_the_launch_directory
func TestLiveProfileExtractionIgnoresAnIcebathCLIPackageInTheLaunchDirectory(t *testing.T) {
	installHostEnvHooks()
	root := t.TempDir()
	h := filepath.Join(root, "icebath-home")
	pluginDir := filepath.Join(h, "plugins", "model-providers", "cwdprobe")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, "__init__.py"), "from providers import register_provider\nfrom providers.base import ProviderProfile\n\n"+
		"register_provider(ProviderProfile(name=\"cwdprobe\", base_url=\"https://cwdprobe.example/v1\"))\n")
	writeFile(t, filepath.Join(h, "config.yaml"), `{"plugins": {"isolation": "host"}}`)
	launch := filepath.Join(root, "cloned-repo")
	if err := os.MkdirAll(filepath.Join(launch, "icebath_cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(launch, "icebath_cli", "__init__.py"),
		"from pathlib import Path\nPath(__file__).resolve().parents[1].joinpath(\"shadow-imported.txt\").write_text(\"imported\")\n")
	writeFile(t, filepath.Join(launch, "icebath_cli", "plugin_host_child.py"), "import json\nprint(json.dumps({\"profiles\": []}))\n")
	t.Setenv("ICEBATH_HOME", h)
	previous := hostInterpreter
	python := liveInterpreters(t)["system"]
	hostInterpreter = func(context.Context) (string, error) { return python, nil }
	t.Cleanup(func() { hostInterpreter = previous })
	t.Chdir(launch)
	profiles, _, err := LoadHostedProfiles(homeCtx(h), pluginDir, "_icebath_test_cwdprobe")
	if err != nil {
		t.Fatal(err)
	}
	if isFile(filepath.Join(launch, "shadow-imported.txt")) {
		t.Fatal("extraction imported the launch directory icebath_cli package")
	}
	if len(profiles) != 1 || profiles[0].Name != "cwdprobe" || profiles[0].Fields["base_url"] != "https://cwdprobe.example/v1" {
		t.Fatalf("profiles = %v", profiles)
	}
}

const seedingPlugin = `
import os
from providers import register_provider
from providers.base import ProviderProfile

class Seeds(ProviderProfile):
    def seed_pool_credentials(self, *, suppressed_sources, explicitly_configured):
        token = os.environ.get("GH_TOKEN", "")
        if not token or "env:GH_TOKEN" in suppressed_sources:
            return []
        return [{"source": "env:GH_TOKEN", "access_token": f"minted-{token}-{explicitly_configured}",
                 "base_url": None, "label": f"pid {os.getpid()}"}]

register_provider(Seeds(name="seedhost", base_url="https://seedhost.example/v1",
                        env_vars=("GH_TOKEN",), ambient_credentials=True))
`

// Hosted half of test_plugin_host_seeded_credentials.py, the seeding call with a shell exported ambient token and a suppressed source
func TestLiveHostedProfileSeedsFromAShellExportedAmbientToken(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], map[string]string{})
	pluginDir := filepath.Join(h, "plugins", "model-providers", "seedhost")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, "plugin.yaml"), "name: seedhost\nkind: model-provider\n")
	writeFile(t, filepath.Join(pluginDir, "__init__.py"), seedingPlugin)
	t.Setenv("GH_TOKEN", "shell-only")
	ctx := homeCtx(h)
	profiles, _, err := LoadHostedProfiles(ctx, pluginDir, "_icebath_user_provider_test_seedhost")
	if err != nil || len(profiles) != 1 || !IsHostedProfile(profiles[0]) {
		t.Fatalf("profiles %v err %v", profiles, err)
	}
	t.Cleanup(func() {
		hostedProfilesMu.Lock()
		delete(hostedProfiles, pluginDir)
		hostedProfilesMu.Unlock()
		if mgr := GetPluginManager(ctx); mgr != nil && mgr.pluginHostInstance != nil {
			mgr.pluginHostInstance.Shutdown()
		}
	})
	rows, err := profiles[0].Call(ctx, "seed_pool_credentials", nil, map[string]any{"suppressed_sources": []any{}, "explicitly_configured": true})
	if err != nil {
		t.Fatal(err)
	}
	list, _ := rows.([]any)
	if len(list) != 1 {
		t.Fatalf("rows = %v", rows)
	}
	row := list[0].(map[string]any)
	if row["source"] != "env:GH_TOKEN" || row["access_token"] != "minted-shell-only-True" || row["label"] == fmt.Sprintf("pid %d", os.Getpid()) {
		t.Fatalf("row = %v", row)
	}
	suppressed, err := profiles[0].Call(ctx, "seed_pool_credentials", nil, map[string]any{"suppressed_sources": []any{"env:GH_TOKEN"}, "explicitly_configured": true})
	if got, _ := suppressed.([]any); err != nil || len(got) != 0 {
		t.Fatalf("suppressed = %v %v", suppressed, err)
	}
}

// Port of test_a_routed_home_host_never_inherits_the_launch_shell_token
func TestARoutedHomeHostNeverInheritsTheLaunchShellToken(t *testing.T) {
	installLiveServices(t)
	root := t.TempDir()
	launch, coder, writer := filepath.Join(root, "launch"), filepath.Join(root, "coder"), filepath.Join(root, "writer")
	for _, dir := range []string{launch, coder, writer} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(writer, ".env"), "GH_TOKEN=writer-own\n")
	t.Setenv("ICEBATH_HOME", launch)
	t.Setenv("GH_TOKEN", "launch-shell")
	t.Setenv("GITHUB_TOKEN", "never-for-an-in-process-profile")
	key := filepath.Join(root, "amb-plugin")
	hostedProfilesMu.Lock()
	hostedProfiles[key] = []*HostedProfile{{Name: "amb", Fields: map[string]any{"env_vars": []any{"GH_TOKEN"}, "ambient_credentials": true}}}
	hostedProfilesMu.Unlock()
	t.Cleanup(func() {
		hostedProfilesMu.Lock()
		delete(hostedProfiles, key)
		hostedProfilesMu.Unlock()
	})
	for h, want := range map[string]map[string]string{launch: {"GH_TOKEN": "launch-shell"}, coder: {}, writer: {"GH_TOKEN": "writer-own"}} {
		got, err := hostedAmbientCredentials(homeCtx(h), h)
		if err != nil || len(got) != len(want) || got["GH_TOKEN"] != want["GH_TOKEN"] {
			t.Fatalf("%s: %v %v want %v", filepath.Base(h), got, err, want)
		}
	}
}
