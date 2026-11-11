package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

const flatprovInit = `
import json
from pathlib import Path
from agent.memory_provider import MemoryProvider


class FlatProvMemoryProvider(MemoryProvider):
    @property
    def name(self):
        return "flatprov"

    def is_available(self):
        return True

    def initialize(self, session_id, **kwargs):
        pass

    def get_tool_schemas(self):
        return []

    def get_config_schema(self):
        return [
            {"key": "mode", "label": "Mode", "choices": ["cloud", "local_external"], "default": "cloud"},
            {"key": "api_url", "label": "API URL", "default": ""},
            {"key": "api_key", "label": "API key", "secret": True, "env_var": "FLATPROV_API_KEY"},
            {"key": "bank_id", "label": "Bank", "default": "hermes"},
            {"key": "recall_budget", "label": "Budget", "choices": ["low", "mid", "high"], "default": "mid"},
        ]

    def save_config(self, values, hermes_home):
        path = Path(hermes_home) / "flatprov" / "config.json"
        path.parent.mkdir(parents=True, exist_ok=True)
        existing = json.loads(path.read_text()) if path.exists() else {}
        existing.update(values)
        path.write_text(json.dumps(existing))


def register(ctx):
    ctx.register_memory_provider(FlatProvMemoryProvider())
`

const flatprovSchema = `
from plugins.memory.config_schema import (
    KIND_SECRET, KIND_SELECT, KIND_TEXT, ProviderConfigSchema, ProviderField, ProviderFieldOption,
)

CONFIG_SCHEMA = ProviderConfigSchema(
    name="flatprov",
    label="Flat Provider",
    fields=(
        ProviderField(key="mode", label="Mode", kind=KIND_SELECT, description="", default="cloud",
                      options=(ProviderFieldOption("cloud", "Cloud"), ProviderFieldOption("local_external", "Local"))),
        ProviderField(key="api_url", label="API URL", kind=KIND_TEXT, description=""),
        ProviderField(key="api_key", label="API key", kind=KIND_SECRET, description="", env_key="FLATPROV_API_KEY"),
    ),
)
`

const hostprovInit = `
from agent.memory_provider import MemoryProvider


class HostProv(MemoryProvider):
    name = "hostprov"

    def is_available(self):
        return True

    def initialize(self, session_id, **kwargs):
        pass

    def get_tool_schemas(self):
        return []


def register(ctx):
    ctx.register_memory_provider(HostProv())
`

const hostprovClient = `
from icebath_constants import get_icebath_home


def resolve_active_host():
    return "hermes"


def resolve_config_path():
    return get_icebath_home() / "hostprov.json"


def _host_block(cfg, host):
    return (cfg.get("hosts") or {}).get(host) or {}
`

const hostprovOAuth = `
import contextlib
import json
import threading

ACCESS_TOKEN_PREFIX = "oat_"
_refresh_lock = threading.Lock()


@contextlib.contextmanager
def _config_refresh_lock(path):
    yield


def _read_config_strict(path):
    return json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}
`

const hostprovSchema = `
from plugins.memory.config_schema import (
    KIND_SECRET, KIND_SELECT, KIND_TEXT, STORAGE_HONCHO_HOST_BLOCK, ProviderConfigSchema, ProviderField,
    ProviderFieldOption,
)

CONFIG_SCHEMA = ProviderConfigSchema(
    name="hostprov",
    label="Host-block Provider",
    storage=STORAGE_HONCHO_HOST_BLOCK,
    fields=(
        ProviderField(key="apiKey", label="API key", kind=KIND_SECRET, description="", env_key="HOSTPROV_API_KEY"),
        ProviderField(key="baseUrl", label="Base URL", kind=KIND_TEXT, description="", scope="root"),
        ProviderField(key="environment", label="Environment", kind=KIND_SELECT, description="", default="production",
                      options=(ProviderFieldOption("production", "Cloud"), ProviderFieldOption("local", "Local"))),
        ProviderField(key="workspace", label="Workspace", kind=KIND_TEXT, description=""),
        ProviderField(key="peerName", label="Peer name", kind=KIND_TEXT, description=""),
        ProviderField(key="aiPeer", label="AI peer", kind=KIND_TEXT, description=""),
        ProviderField(key="sessionStrategy", label="Session strategy", kind=KIND_TEXT, description=""),
    ),
)
`

// Session client over the dashboard application whose plugin hosts stop with the case
func memoryProviderClient(t *testing.T) (sessionClient, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	t.Cleanup(plugins.SetHostInterpreterForTests(python))
	t.Setenv("PYTHONPATH", "")
	t.Setenv("ICEBATH_ENABLE_PROJECT_PLUGINS", "")
	c := newSessionClient(t)
	t.Cleanup(func() {
		for _, host := range plugins.RunningPluginHosts() {
			host.Shutdown()
		}
	})
	return c, os.Getenv("ICEBATH_HOME")
}

// Writes the modules of a user memory provider plugin under the home
func installMemoryPlugin(t *testing.T, h, name string, modules map[string]string) string {
	t.Helper()
	dir := filepath.Join(h, "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for module, source := range modules {
		if err := os.WriteFile(filepath.Join(dir, module+".py"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func installFlatprov(t *testing.T, h string) string {
	return installMemoryPlugin(t, h, "flatprov", map[string]string{"__init__": flatprovInit, "config_schema": flatprovSchema})
}

func installHostprov(t *testing.T, h string) string {
	installMemoryPlugin(t, h, "hostprov", map[string]string{"__init__": hostprovInit, "client": hostprovClient, "oauth": hostprovOAuth, "config_schema": hostprovSchema})
	path := filepath.Join(h, "hostprov.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Fields of a provider config payload by key
func providerFieldMap(t *testing.T, payload map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	fields, _ := payload["fields"].([]any)
	for _, raw := range fields {
		field := raw.(map[string]any)
		out[field["key"].(string)] = field
	}
	return out
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out
}

func memoryConfigProvider(t *testing.T, h string) any {
	t.Helper()
	cfg, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		t.Fatal(err)
	}
	section, _ := map[string]any(cfg)["memory"].(map[string]any)
	return section["provider"]
}

func TestDeclaredSurfacePutWritesConfigAndSecret(t *testing.T) {
	c, h := memoryProviderClient(t)
	installFlatprov(t, h)
	r := c.send("PUT", "/api/memory/providers/flatprov/config?surface=declared", map[string]any{"values": map[string]any{
		"mode": "local_external", "api_url": "http://localhost:8888", "api_key": "fp-declared-key"}})
	if r.Status != 200 || !reflect.DeepEqual(jsonObject(t, r), map[string]any{"ok": true}) {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if got := config.LoadEnv(h)["FLATPROV_API_KEY"]; got != "fp-declared-key" {
		t.Fatalf("env %q", got)
	}
	providerConfig := readJSONFile(t, filepath.Join(h, "flatprov", "config.json"))
	if providerConfig["mode"] != "local_external" || providerConfig["api_url"] != "http://localhost:8888" {
		t.Fatalf("config %v", providerConfig)
	}
	if _, held := providerConfig["api_key"]; held {
		t.Fatalf("secret stored in %v", providerConfig)
	}
}

func TestPostMemoryProviderSetupRoutesPythonDepsThroughPm(t *testing.T) {
	c, h := memoryProviderClient(t)
	var prepared []string
	realPrepare := prepareMemoryProviderDependencies
	prepareMemoryProviderDependencies = func(_ context.Context, _ *tools.Env, name string) (map[string]any, string, error) {
		prepared = append(prepared, name)
		return map[string]any{}, "installed", nil
	}
	realRun := setupSubprocessRun
	setupSubprocessRun = func(ctx context.Context, argv []string, shellCommand string, shell bool, timeoutSeconds int) (*setupCompleted, error) {
		flat := shellCommand
		if !shell {
			flat = strings.Join(argv, " ")
		}
		if strings.Contains(flat, "pip install") {
			t.Errorf("direct pip call leaked: %s", flat)
		}
		return realRun(ctx, argv, shellCommand, shell, timeoutSeconds)
	}
	t.Cleanup(func() { prepareMemoryProviderDependencies, setupSubprocessRun = realPrepare, realRun })
	installFlatprov(t, h)
	r := c.send("POST", "/api/memory/providers/flatprov/setup", map[string]any{"values": map[string]any{}})
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	var pipRows []map[string]any
	for _, raw := range jsonObject(t, r)["results"].([]any) {
		if row := raw.(map[string]any); row["kind"] == "pip" {
			pipRows = append(pipRows, row)
		}
	}
	if len(pipRows) == 0 || pipRows[0]["status"] != "installed" || pipRows[0]["command"] != "icebath pm install" {
		t.Fatalf("pip rows %v", pipRows)
	}
	if !reflect.DeepEqual(prepared, []string{"flatprov"}) {
		t.Fatalf("prepared %v", prepared)
	}
}

func TestPutMemoryProviderConfigWritesConfigAndSecret(t *testing.T) {
	c, h := memoryProviderClient(t)
	installFlatprov(t, h)
	r := c.send("PUT", "/api/memory/providers/flatprov/config", map[string]any{"values": map[string]any{
		"mode": "local_external", "api_url": "http://localhost:8888", "api_key": "fp-test-key", "bank_id": "ben-bank", "recall_budget": "high"}})
	if r.Status != 200 || !reflect.DeepEqual(jsonObject(t, r), map[string]any{"ok": true, "active": "flatprov"}) {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if got := memoryConfigProvider(t, h); got != "flatprov" {
		t.Fatalf("memory.provider %v", got)
	}
	if got := config.LoadEnv(h)["FLATPROV_API_KEY"]; got != "fp-test-key" {
		t.Fatalf("env %q", got)
	}
	raw, err := os.ReadFile(filepath.Join(h, "flatprov", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"mode": "local_external", "api_url": "http://localhost:8888", "bank_id": "ben-bank", "recall_budget": "high"}` {
		t.Fatalf("config %s", raw)
	}
}

func TestGetMemoryProviderConfigDoesNotReturnSecret(t *testing.T) {
	c, h := memoryProviderClient(t)
	installFlatprov(t, h)
	c.send("PUT", "/api/memory/providers/flatprov/config", map[string]any{"values": map[string]any{
		"mode": "cloud", "api_url": "https://api.example.invalid", "api_key": "secret-value", "bank_id": "hermes", "recall_budget": "mid"}})
	r := c.get("/api/memory/providers/flatprov/config")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	fields := providerFieldMap(t, jsonObject(t, r))
	if fields["api_key"]["is_set"] != true || fields["api_key"]["value"] != "" || strings.Contains(string(r.Body), "secret-value") {
		t.Fatalf("body %s", r.Body)
	}
}

func TestPutHostBlockWritesHostBlockRootAndSecret(t *testing.T) {
	c, h := memoryProviderClient(t)
	configPath := installHostprov(t, h)
	r := c.send("PUT", "/api/memory/providers/hostprov/config?surface=declared", map[string]any{"values": map[string]any{
		"apiKey": "hch-test-key", "baseUrl": "https://honcho.example.dev", "environment": "local", "workspace": "myws",
		"peerName": "eri", "aiPeer": "hermes", "sessionStrategy": "per-repo"}})
	if r.Status != 200 || !reflect.DeepEqual(jsonObject(t, r), map[string]any{"ok": true}) {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	if got := memoryConfigProvider(t, h); got != "hostprov" {
		t.Fatalf("memory.provider %v", got)
	}
	if got := config.LoadEnv(h)["HOSTPROV_API_KEY"]; got != "hch-test-key" {
		t.Fatalf("env %q", got)
	}
	cfg := readJSONFile(t, configPath)
	block, _ := cfg["hosts"].(map[string]any)["hermes"].(map[string]any)
	if cfg["baseUrl"] != "https://honcho.example.dev" || block["workspace"] != "myws" || block["peerName"] != "eri" ||
		block["environment"] != "local" || block["sessionStrategy"] != "per-repo" || block["apiKey"] != "hch-test-key" {
		t.Fatalf("config %v", cfg)
	}
	if st, err := os.Stat(configPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st, err)
	}
}

func TestGetHostBlockConfigDoesNotReturnSecret(t *testing.T) {
	c, h := memoryProviderClient(t)
	installHostprov(t, h)
	c.send("PUT", "/api/memory/providers/hostprov/config?surface=declared", map[string]any{"values": map[string]any{"apiKey": "secret-value"}})
	r := c.get("/api/memory/providers/hostprov/config?surface=declared")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	fields := providerFieldMap(t, jsonObject(t, r))
	if fields["apiKey"]["is_set"] != true || fields["apiKey"]["value"] != "" || strings.Contains(string(r.Body), "secret-value") {
		t.Fatalf("body %s", r.Body)
	}
	if fields["workspace"]["placeholder"] != "hermes" || fields["environment"]["value"] != "production" {
		t.Fatalf("fields %v", fields)
	}
}

func TestPutHostBlockNeverReplacesAnUnparseableConfig(t *testing.T) {
	for _, corrupt := range []bool{true, false} {
		name := map[bool]string{true: "unparseable-file-is-left-alone", false: "parseable-file-is-merged"}[corrupt]
		t.Run(name, func(t *testing.T) {
			c, h := memoryProviderClient(t)
			configPath := installHostprov(t, h)
			before := `{"hosts": {"other": {"apiKey": "keep-me"}}}`
			if corrupt {
				before = "{not json"
			}
			if err := os.WriteFile(configPath, []byte(before), 0o644); err != nil {
				t.Fatal(err)
			}
			r := c.send("PUT", "/api/memory/providers/hostprov/config?surface=declared", map[string]any{"values": map[string]any{"workspace": "myws"}})
			if corrupt {
				after, _ := os.ReadFile(configPath)
				if r.Status != 400 || string(after) != before || memoryConfigProvider(t, h) == "hostprov" {
					t.Fatalf("status %d %s file %s", r.Status, r.Body, after)
				}
				return
			}
			if r.Status != 200 {
				t.Fatalf("status %d %s", r.Status, r.Body)
			}
			raw, _ := os.ReadFile(configPath)
			want := "{\n  \"hosts\": {\n    \"other\": {\n      \"apiKey\": \"keep-me\"\n    },\n    \"hermes\": {\n      \"workspace\": \"myws\"\n    }\n  }\n}"
			if string(raw) != want {
				t.Fatalf("config %s", raw)
			}
		})
	}
}

const userDirHonchoClient = `
import json, os
from pathlib import Path

def resolve_active_host():
    return "hermes"

def resolve_config_path():
    return Path(os.environ["ICEBATH_HOME"]) / "honcho.json"

def _host_block(cfg, host):
    return (cfg.get("hosts") or {}).get(host) or {}
`

const userDirHonchoOAuthFlow = `
from . import client  # relative import: only a real package load resolves it

def start_loopback_flow_background():
    return {"state": "pending", "config": str(client.resolve_config_path())}

def get_flow_status():
    return {"state": "idle"}
`

const userDirHonchoSchema = `
from plugins.memory.config_schema import STORAGE_HONCHO_HOST_BLOCK, ProviderConfigSchema, ProviderField

CONFIG_SCHEMA = ProviderConfigSchema(
    name="honcho", label="Honcho (user dir)", storage=STORAGE_HONCHO_HOST_BLOCK,
    fields=(ProviderField(key="workspace", label="Workspace", inline=True),),
)
`

// A user dir honcho provider with its host block config, the bundled copy absent
func userDirHoncho(t *testing.T) (sessionClient, string, string) {
	t.Helper()
	c, h := memoryProviderClient(t)
	dir := installMemoryPlugin(t, h, "honcho", map[string]string{
		"__init__":      "\"\"\"fake provider: register_memory_provider\"\"\"\nfrom .client import resolve_active_host\n",
		"client":        userDirHonchoClient,
		"oauth_flow":    userDirHonchoOAuthFlow,
		"config_schema": userDirHonchoSchema,
	})
	if err := os.WriteFile(filepath.Join(h, "honcho.json"), []byte(`{"hosts": {"hermes": {"workspace": "from-user-dir"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return c, h, dir
}

func TestUserDirHostBlockProviderServesItsDeclaredConfig(t *testing.T) {
	c, _, _ := userDirHoncho(t)
	r := c.get("/api/memory/providers/honcho/config?surface=declared")
	if r.Status != 200 {
		t.Fatalf("status %d %s", r.Status, r.Body)
	}
	fields, _ := jsonObject(t, r)["fields"].([]any)
	if len(fields) != 1 {
		t.Fatalf("fields %s", r.Body)
	}
	field := fields[0].(map[string]any)
	if field["key"] != "workspace" || field["value"] != "from-user-dir" {
		t.Fatalf("field %v", field)
	}
}

func TestUserDirProviderOauthFlowResolvesFromItsDirectory(t *testing.T) {
	_, h, dir := userDirHoncho(t)
	ctx := context.Background()
	flow, err := resolveMemoryOAuthFlow(ctx, "honcho")
	if err != nil {
		t.Fatal(err)
	}
	if got := memory.FindProviderDir(ctx, memoryToolsEnv(ctx), "honcho"); got != dir {
		t.Fatalf("provider dir %q, want %q", got, dir)
	}
	started, err := flow.StartLoopbackFlowBackground(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := started.(map[string]any)
	if reply["state"] != "pending" || reply["config"] != filepath.Join(h, "honcho.json") {
		t.Fatalf("start %v", started)
	}
}

func TestSetupReportsRestartAndPreservesExternalSteps(t *testing.T) {
	for _, pythonFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "False", true: "True"}[pythonFailure], func(t *testing.T) {
			_, h := memoryProviderClient(t)
			python, _ := exec.LookPath("python3")
			manifest, _ := json.Marshal(map[string]any{"name": "provider", "extra": "mem0", "external_dependencies": []any{
				map[string]any{"name": "external-sidecar", "check": pyshlex.Quote(python) + ` -c "pass"`}}})
			dir := installMemoryPlugin(t, h, "provider", map[string]string{"__init__": "def register(ctx):\n    ctx.register_memory_provider(None)\n"})
			if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), manifest, 0o644); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(memory.SwapSetupPM(func(context.Context, pm.SyncVenvOptions) error {
				if pythonFailure {
					return errors.New("Python preparation refused")
				}
				return nil
			}, func(context.Context, string) (bool, string, error) {
				return false, filepath.Join(h, "next-generation"), nil
			}))
			result, err := installMemoryProviderSetup(context.Background(), "provider")
			if err != nil {
				t.Fatal(err)
			}
			results, _ := result.GetOr("results", nil).([]any)
			if len(results) < 2 {
				t.Fatalf("results %v", results)
			}
			first, second := results[0].(*pyjson.Object), results[1].(*pyjson.Object)
			wantFirst := "restart_required"
			if pythonFailure {
				wantFirst = "failed"
			}
			if result.GetOr("ok", nil) != !pythonFailure || first.GetOr("status", nil) != wantFirst ||
				second.GetOr("name", nil) != "external-sidecar" || second.GetOr("status", nil) != "already_installed" {
				raw, _ := pyjson.Dumps(result)
				t.Fatalf("result %s", raw)
			}
			for _, row := range results {
				if row.(*pyjson.Object).GetOr("status", nil) == "no_declared_steps" {
					t.Fatalf("results %v", results)
				}
			}
		})
	}
}

func TestMalformedCandidateManifestIsNotASuccessfulNoop(t *testing.T) {
	_, h := memoryProviderClient(t)
	dir := installMemoryPlugin(t, h, "broken", map[string]string{"__init__": "def register(ctx):\n    ctx.register_memory_provider(None)\n"})
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte("name: [unterminated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := installMemoryProviderPythonDependencies(context.Background(), "broken")
	if len(rows) != 1 || rows[0].(*pyjson.Object).GetOr("status", nil) != "failed" {
		t.Fatalf("rows %v", rows)
	}
}
