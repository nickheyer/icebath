package plugins

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

// Installs the tools env hooks the profiles and gateway ports install in production, valued as a test process sees them
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

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadedError(m *Manager, key string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if p := m.plugins[key]; p != nil {
		return p.Error
	}
	return "<not loaded>"
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
