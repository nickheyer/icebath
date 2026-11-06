package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const providerEntryPointModule = `
import os
from providers import register_provider
from providers.base import ProviderProfile

class Acme(ProviderProfile):
    def build_extra_body(self, *, session_id=None, **context):
        return {"pid": os.getpid(), "session_id": session_id}

register_provider(ProviderProfile(name="acme-module", base_url="https://module.example/v1"))

def register():
    register_provider(Acme(name="acme-call", base_url="https://call.example/v1"))

def general(ctx):
    raise AssertionError("a general plugin target is never called by the provider scan")

def optional(extra=None):
    register_provider(ProviderProfile(name="acme-optional", base_url="https://optional.example/v1"))

def explodes():
    register_provider(ProviderProfile(name="acme-before-raise", base_url="https://raise.example/v1"))
    raise RuntimeError("invoke boom")
`

const providerEntryPointBroken = `
from providers import register_provider
from providers.base import ProviderProfile
register_provider(ProviderProfile(name="acme-partial", base_url="https://partial.example/v1"))
raise ImportError("half imported")
`

// test_plugin_host.py and providers/__init__.py entry point discovery through a real plugin host child
func TestLiveProviderEntryPointsLoadInTheHost(t *testing.T) {
	installLiveServices(t)
	site := t.TempDir()
	t.Setenv("PYTHONPATH", site)
	memWrite(t, filepath.Join(site, "acme_eps.py"), providerEntryPointModule)
	memWrite(t, filepath.Join(site, "acme_broken.py"), providerEntryPointBroken)
	memWrite(t, filepath.Join(site, "acme-1.0.dist-info", "METADATA"), "Metadata-Version: 2.1\nName: acme\nVersion: 1.0\n")
	memWrite(t, filepath.Join(site, "acme-1.0.dist-info", "entry_points.txt"), "[icebath_agent.plugins]\n"+
		"acme-call = acme_eps:register\nacme-module = acme_eps\ngeneral = acme_eps:general\noptional = acme_eps:optional\n"+
		"explodes = acme_eps:explodes\nbroken = acme_broken\n[other.group]\nignored = acme_eps:register\n")
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	ctx := homeCtx(h)
	eps := map[string]PipEntryPoint{}
	names := []string{}
	for _, ep := range ProviderEntryPoints() {
		eps[ep.Name] = ep
		names = append(names, ep.Name)
	}
	if strings.Join(names, ",") != "acme-call,acme-module,general,optional,explodes,broken" {
		t.Fatalf("provider entry points = %v", names)
	}
	module, err := LoadProviderEntryPoint(ctx, eps["acme-module"])
	if err != nil || module.Callable || len(module.Profiles) != 1 || module.Profiles[0].Name != "acme-module" ||
		module.Profiles[0].Fields["base_url"] != "https://module.example/v1" {
		t.Fatalf("module target = %+v %v", module, err)
	}
	call, err := LoadProviderEntryPoint(ctx, eps["acme-call"])
	if err != nil || !call.Callable || call.RequiresArguments || len(call.Profiles) != 0 {
		t.Fatalf("callable target = %+v %v", call, err)
	}
	invoked, err := InvokeProviderEntryPoint(ctx, eps["acme-call"])
	if err != nil || len(invoked) != 1 || invoked[0].Name != "acme-call" || invoked[0].Fields["base_url"] != "https://call.example/v1" {
		t.Fatalf("invoke = %v %v", invoked, err)
	}
	if _, ok := invoked[0].Calls["build_extra_body"]; !ok {
		t.Fatalf("override calls = %v", invoked[0].Calls)
	}
	body, err := invoked[0].Call(ctx, "build_extra_body", nil, map[string]any{"session_id": "s9"})
	got, _ := body.(map[string]any)
	if pid, _ := got["pid"].(int64); err != nil || got["session_id"] != "s9" || pid == 0 || int(pid) == os.Getpid() {
		t.Fatalf("build_extra_body = %v %v", body, err)
	}
	m.pluginHostInstance.Shutdown()
	body, err = invoked[0].Call(ctx, "build_extra_body", nil, map[string]any{"session_id": "after-restart"})
	if got, _ := body.(map[string]any); err != nil || got["session_id"] != "after-restart" {
		t.Fatalf("build_extra_body after a host restart = %v %v", body, err)
	}
	general, err := LoadProviderEntryPoint(ctx, eps["general"])
	if err != nil || !general.Callable || !general.RequiresArguments {
		t.Fatalf("general plugin target = %+v %v", general, err)
	}
	optional, err := LoadProviderEntryPoint(ctx, eps["optional"])
	if err != nil || !optional.Callable || optional.RequiresArguments {
		t.Fatalf("defaulted argument target = %+v %v", optional, err)
	}
	if _, err := LoadProviderEntryPoint(ctx, eps["explodes"]); err != nil {
		t.Fatal(err)
	}
	raised, err := InvokeProviderEntryPoint(ctx, eps["explodes"])
	var builtin *HostBuiltinError
	if !errors.As(err, &builtin) || builtin.Type != "RuntimeError" || err.Error() != "invoke boom" ||
		len(raised) != 1 || raised[0].Name != "acme-before-raise" {
		t.Fatalf("raising invoke = %v %v", raised, err)
	}
	broken, err := LoadProviderEntryPoint(ctx, eps["broken"])
	if !errors.As(err, &builtin) || builtin.Type != "ImportError" || err.Error() != "half imported" ||
		len(broken.Profiles) != 1 || broken.Profiles[0].Name != "acme-partial" || broken.Callable {
		t.Fatalf("broken load = %+v %v", broken, err)
	}
}

const createClientPlugin = `
import os
from providers import register_provider
from providers.base import ProviderProfile

class Completions:
    def __init__(self, key):
        self.key = key
    def create(self, **kwargs):
        return {"pid": os.getpid(), "model": kwargs.get("model"), "key": self.key}

class Chat:
    def __init__(self, key):
        self.completions = Completions(key)

class Client:
    def __init__(self, key):
        self.chat = Chat(key)

class OwnClient(ProviderProfile):
    def create_client(self, **client_kwargs):
        return Client(client_kwargs.get("api_key"))

register_provider(ProviderProfile(name="plain", base_url="https://plain.example/v1"))
register_provider(OwnClient(name="ownclient", base_url="https://own.example/v1"))
`

// test_plugin_host_mixed_profiles.py test_create_client_profile_loads_in_process_and_its_sibling_stays_hosted at the plugin host seam
func TestLiveCreateClientOverrideServesALiveHostClient(t *testing.T) {
	installLiveServices(t)
	h, _ := liveHome(t, liveInterpreters(t)["system"], nil)
	m := memoryManager(t, h)
	pluginDir := createClientPluginDir(t, h)
	ctx := homeCtx(h)
	profiles, err := LoadProviderPluginProfiles(ctx, pluginDir, "_icebath_user_provider_test_ownpair")
	if err != nil || len(profiles) != 2 {
		t.Fatalf("profiles = %v %v", profiles, err)
	}
	own := profiles[1]
	if own.Name != "ownclient" || own.InProcess == "" || own.Fields["base_url"] != "https://own.example/v1" {
		t.Fatalf("create_client profile = %+v", own)
	}
	if _, ok := own.Calls["create_client"]; !ok {
		t.Fatalf("create_client is not a host call: %v", own.Calls)
	}
	complete := func() map[string]any {
		t.Helper()
		client, err := own.CreateClient(ctx, nil, map[string]any{"api_key": "k-1"})
		if err != nil {
			t.Fatal(err)
		}
		chat, err := client.Get(ctx, "chat")
		if err != nil {
			t.Fatal(err)
		}
		completions, err := chat.(*HostedObject).Get(ctx, "completions")
		if err != nil {
			t.Fatal(err)
		}
		reply, err := completions.(*HostedObject).Call(ctx, "create", nil, map[string]any{"model": "m-1"})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := reply.(map[string]any)
		return got
	}
	got := complete()
	if pid, _ := got["pid"].(int64); got["model"] != "m-1" || got["key"] != "k-1" || pid == 0 || int(pid) == os.Getpid() {
		t.Fatalf("client reply = %v", got)
	}
	client, err := own.CreateClient(ctx, nil, map[string]any{"api_key": "k-2"})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := client.Get(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	m.pluginHostInstance.Shutdown()
	completions, err := chat.(*HostedObject).Get(ctx, "completions")
	if err != nil {
		t.Fatalf("nested client object after a host restart: %v", err)
	}
	reply, err := completions.(*HostedObject).Call(ctx, "create", nil, map[string]any{"model": "m-2"})
	if got, _ := reply.(map[string]any); err != nil || got["key"] != "k-2" || got["model"] != "m-2" {
		t.Fatalf("client after a host restart = %v %v", reply, err)
	}
}

// Writes the two profile model provider plugin whose second profile overrides create_client
func createClientPluginDir(t *testing.T, h string) string {
	t.Helper()
	pluginDir := filepath.Join(h, "plugins", "model-providers", "ownpair")
	memWrite(t, filepath.Join(pluginDir, "plugin.yaml"), "name: ownpair\nkind: model-provider\n")
	memWrite(t, filepath.Join(pluginDir, "__init__.py"), createClientPlugin)
	return pluginDir
}

// The same test's notice and sibling halves, one warning naming the create_client profile and none its hosted sibling
func TestLiveCreateClientProfileWarnsOnceAndItsSiblingStaysHosted(t *testing.T) {
	logs := captureLogs(t)
	h, m := liveHome(t, liveInterpreters(t)["system"], nil)
	pluginDir := createClientPluginDir(t, h)
	ctx := homeCtx(h)
	hosted, inProcess, err := LoadHostedProfiles(ctx, pluginDir, "_icebath_user_provider_test_ownpair")
	if err != nil || len(hosted) != 1 || hosted[0].Name != "plain" ||
		inProcess["ownclient"] != "it overrides create_client(), which returns a live SDK client" {
		t.Fatalf("hosted %v in process %v err %v", hosted, inProcess, err)
	}
	before := logs.Len()
	profiles, err := LoadProviderPluginProfiles(ctx, pluginDir, "_icebath_user_provider_test_ownpair")
	if err != nil || len(profiles) != 2 || profiles[0].Name != "plain" || profiles[0].InProcess != "" {
		t.Fatalf("profiles = %v %v", profiles, err)
	}
	if text := logs.String()[before:]; strings.Count(text, "ownclient") != 1 || !strings.Contains(text, "create_client") || strings.Contains(text, "plain") {
		t.Fatalf("load notices = %q", text)
	}
	if m.pluginHostInstance != nil && m.pluginHostInstance.PID() != 0 {
		t.Fatal("loading profile data started a plugin host")
	}
}
