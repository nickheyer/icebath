package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
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
