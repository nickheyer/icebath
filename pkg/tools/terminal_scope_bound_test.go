package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

const scopeLaunchCwd = "/srv/launch-a"
const scopeLaunchVolumes = `["/srv/a-secrets:/secrets"]`

// Launch profile A bridged a docker backend into the process env that a routed profile never observes
func pollutedLaunchEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("ICEBATH_HOME", filepath.Join(tmp, ".icebath"))
	t.Setenv("TERMINAL_ENV", "docker")
	t.Setenv("TERMINAL_CWD", scopeLaunchCwd)
	t.Setenv("TERMINAL_DOCKER_VOLUMES", scopeLaunchVolumes)
	t.Setenv("TERMINAL_DOCKER_SHARED_CONTAINER_KEY", "alpha-shared")
	t.Setenv("TERMINAL_SSH_HOST", "10.10.0.103")
	ResetTerminalScopeCache()
	return tmp
}

// Writes a profile home under tmp with optional config and dotenv text
func scopeProfile(t *testing.T, tmp, name, configYAML, dotenv string) string {
	t.Helper()
	h := filepath.Join(tmp, "profiles", name)
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if configYAML != "" {
		if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(configYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if dotenv != "" {
		if err := os.WriteFile(filepath.Join(h, ".env"), []byte(dotenv), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// Points the managed scope at a fresh directory and returns it
func scopeManagedDir(t *testing.T) string {
	t.Helper()
	md := filepath.Join(t.TempDir(), "managed")
	if err := os.MkdirAll(md, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.EnvManagedDir, md)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	return md
}

// Writes the managed config body and drops the managed cache
func writeScopeManaged(t *testing.T, md, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(md, "config.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	config.InvalidateManagedCache()
}

// Makes every read of path fail with a permission error as a sandbox denial does
func denyProfileEnvReads(t *testing.T, path string) {
	t.Helper()
	orig := readProfileEnvBytes
	readProfileEnvBytes = func(p string) ([]byte, error) {
		if p == path {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrPermission}
		}
		return orig(p)
	}
	t.Cleanup(func() { readProfileEnvBytes = orig })
}

// Policy a routed turn reads through the env bound from ctx
func scopedPolicy(t *testing.T, ctx context.Context, h string) TerminalPolicy {
	t.Helper()
	policy, err := terminalPolicy(EnvInTerminalScope(ctx, &Env{Home: h}))
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestManagedBackendBeatsProfileConfig(t *testing.T) {
	md := scopeManagedDir(t)
	writeScopeManaged(t, md, "terminal:\n  backend: busybox\n")
	h := scopeProfile(t, t.TempDir(), "work", "terminal:\n  backend: local\n  timeout: 77\n", "")
	scope, err := BuildProfileTerminalScope(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope["TERMINAL_ENV"] != "busybox" || scope["TERMINAL_TIMEOUT"] != "77" {
		t.Fatalf("scope = %v", scope)
	}
}

func TestManagedMappingMergesWithTheProfileMapping(t *testing.T) {
	md := scopeManagedDir(t)
	writeScopeManaged(t, md, "terminal:\n  docker_env:\n    A: managed\n")
	h := scopeProfile(t, t.TempDir(), "work", "terminal:\n  docker_env:\n    A: profile\n    B: keep\n", "")
	scope, err := BuildProfileTerminalScope(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(scope["TERMINAL_DOCKER_ENV"]), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]any{"A": "managed", "B": "keep"}) {
		t.Fatalf("docker env = %v", got)
	}
}

func TestManagedDockerImagePinsTheImage(t *testing.T) {
	md := scopeManagedDir(t)
	writeScopeManaged(t, md, "terminal:\n  docker_image: lab/image:1\n")
	h := scopeProfile(t, t.TempDir(), "work", "", "")
	scope, err := BuildProfileTerminalScope(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope["TERMINAL_DOCKER_IMAGE"] != "lab/image:1" || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "1" {
		t.Fatalf("scope = %v", scope)
	}
}

func TestNullManagedValuesPinNothing(t *testing.T) {
	md := scopeManagedDir(t)
	writeScopeManaged(t, md, "terminal:\n  backend:\n  docker_image:\n")
	h := scopeProfile(t, t.TempDir(), "work", "terminal:\n  backend: docker\n", "")
	scope, err := BuildProfileTerminalScope(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope["TERMINAL_ENV"] != "docker" || scope["TERMINAL_DOCKER_IMAGE_PINNED"] != "0" {
		t.Fatalf("scope = %v", scope)
	}
}

func TestUnreadableEnvReadsAsEmptyUnderAPinnedBackend(t *testing.T) {
	md := scopeManagedDir(t)
	writeScopeManaged(t, md, "terminal:\n  backend: busybox\n")
	h := scopeProfile(t, t.TempDir(), "work", "", "TERMINAL_ENV=docker\nTERMINAL_TIMEOUT=999\n")
	denyProfileEnvReads(t, filepath.Join(h, ".env"))
	scope, err := BuildProfileTerminalScope(context.Background(), h, map[string]string{"TERMINAL_TIMEOUT": "42"})
	if err != nil {
		t.Fatal(err)
	}
	if scope["TERMINAL_ENV"] != "busybox" || scope["TERMINAL_TIMEOUT"] != "42" {
		t.Fatalf("scope = %v", scope)
	}
}

func TestUnreadableEnvFailsClosedWithoutAPinnedBackend(t *testing.T) {
	cases := map[string]*string{"none": nil}
	for name, body := range map[string]string{
		"timeout": "terminal:\n  timeout: 5\n", "null": "terminal:\n  backend:\n",
		"empty": "terminal:\n  backend: ''\n", "blank": "terminal:\n  backend: '   '\n",
	} {
		cases[name] = &body
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			md := scopeManagedDir(t)
			if body != nil {
				writeScopeManaged(t, md, *body)
			}
			h := scopeProfile(t, t.TempDir(), "work", "terminal:\n  backend: local\n", "TERMINAL_ENV=docker\n")
			denyProfileEnvReads(t, filepath.Join(h, ".env"))
			_, err := BuildProfileTerminalScope(context.Background(), h, nil)
			var unavailable *TerminalPolicyUnavailable
			if !errors.As(err, &unavailable) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestNoScopeKeepsProcessEnvBehavior(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	policy, err := terminalPolicy(&Env{Home: filepath.Join(tmp, ".icebath")})
	if err != nil {
		t.Fatal(err)
	}
	if policy.Get("TERMINAL_ENV", "") != "docker" || policy.Get("TERMINAL_SSH_HOST", "") != "10.10.0.103" {
		t.Fatalf("policy = %v", policy)
	}
}

func TestScopedReadNeverFallsThroughToProcessEnv(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	ctx := SetTerminalScope(context.Background(), &TerminalScope{Policy: TerminalPolicy{"TERMINAL_ENV": "local"}})
	policy := scopedPolicy(t, ctx, filepath.Join(tmp, ".icebath"))
	if policy.Get("TERMINAL_ENV", "") != "local" || policy.Get("TERMINAL_SSH_HOST", "") != "" || policy.Get("TERMINAL_DOCKER_VOLUMES", "[]") != "[]" {
		t.Fatalf("policy = %v", policy)
	}
	if os.Getenv("TERMINAL_ENV") != "docker" {
		t.Fatal("process env mutated")
	}
}

func TestRoutedTurnReadsEveryTerminalConsumerFromProfile(t *testing.T) {
	for _, form := range []string{"config-yaml", "dotenv-only"} {
		t.Run(form, func(t *testing.T) {
			tmp := pollutedLaunchEnv(t)
			bCwd := filepath.Join(tmp, "b-work")
			if err := os.MkdirAll(bCwd, 0o755); err != nil {
				t.Fatal(err)
			}
			var h string
			if form == "config-yaml" {
				h = scopeProfile(t, tmp, "bee", "terminal:\n  backend: local\n  cwd: "+bCwd+"\n", "")
			} else {
				h = scopeProfile(t, tmp, "bee", "", "TERMINAL_ENV=local\nTERMINAL_CWD="+bCwd+"\n")
			}
			ctx := WithProfileTerminalScope(context.Background(), h, nil)
			env := EnvInTerminalScope(ctx, &Env{Home: h})
			cfg, err := getEnvConfig(env)
			if err != nil {
				t.Fatal(err)
			}
			if cfg["env_type"] != "local" || cfg["cwd"] != bCwd || cfg["docker_shared_container_key"] != "" {
				t.Fatalf("cfg = %v", cfg)
			}
			if vols, _ := cfg["docker_volumes"].([]any); len(vols) != 0 {
				t.Fatalf("docker_volumes = %v", cfg["docker_volumes"])
			}
			key, err := resolveContainerTaskID(env, "")
			if err != nil || key != "home:"+realPathOf(h) {
				t.Fatalf("container key = %q %v", key, err)
			}
			if got, err := configuredTerminalCwd(env); err != nil || got != bCwd {
				t.Fatalf("configured cwd = %q %v", got, err)
			}
			if got, err := ResolveAgentCwd(env); err != nil || got != bCwd {
				t.Fatalf("agent cwd = %q %v", got, err)
			}
			if local, err := isLocalTerminalBackend(env); err != nil || !local {
				t.Fatalf("local backend = %v %v", local, err)
			}
			if got := resolveTerminalBackend(env); got != "local" {
				t.Fatalf("probe backend = %q", got)
			}
			if GetTerminalScope(context.Background()) != nil || os.Getenv("TERMINAL_DOCKER_VOLUMES") != scopeLaunchVolumes {
				t.Fatal("scope leaked past the turn")
			}
		})
	}
}

func TestProfileOmittingKeysGetsDefaultsNotLaunchValues(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	h := scopeProfile(t, tmp, "bee", "terminal:\n  backend: docker\n", "")
	env := EnvInTerminalScope(WithProfileTerminalScope(context.Background(), h, nil), &Env{Home: h})
	cfg, err := getEnvConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if vols, _ := cfg["docker_volumes"].([]any); cfg["env_type"] != "docker" || len(vols) != 0 || cfg["docker_shared_container_key"] != "" || cfg["ssh_host"] != "" || cfg["cwd"] == scopeLaunchCwd {
		t.Fatalf("cfg = %v", cfg)
	}
	var launch []any
	if err := json.Unmarshal([]byte(os.Getenv("TERMINAL_DOCKER_VOLUMES")), &launch); err != nil || len(launch) == 0 {
		t.Fatal("launch volumes changed")
	}
}

func TestPersistentDockerRoutedProfileKeepsOneContainer(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	keys := map[string]string{}
	for _, name := range []string{"bee", "wasp"} {
		h := scopeProfile(t, tmp, name, "terminal:\n  backend: docker\n  container_persistent: true\n", "")
		env := EnvInTerminalScope(WithProfileTerminalScope(context.Background(), h, nil), &Env{Home: h})
		cronKey, err := resolveContainerTaskID(env, "")
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("ICEBATH_SESSION_KEY", "agent:"+name+":chat")
		t.Setenv("ICEBATH_SESSION_PROFILE", name)
		sessionKey, err := resolveContainerTaskID(env, "")
		if err != nil {
			t.Fatal(err)
		}
		os.Unsetenv("ICEBATH_SESSION_KEY")
		os.Unsetenv("ICEBATH_SESSION_PROFILE")
		if cronKey != sessionKey || cronKey != "profile:"+name {
			t.Fatalf("%s keys = %q %q", name, cronKey, sessionKey)
		}
		keys[name] = cronKey
	}
	if keys["bee"] == keys["wasp"] {
		t.Fatal("profiles share a container")
	}
}

func TestMalformedProfileConfigRefusesExecution(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	h := scopeProfile(t, tmp, "broken", "terminal: [unclosed\n", "")
	ctx := WithProfileTerminalScope(context.Background(), h, nil)
	scope := GetTerminalScope(ctx)
	if scope == nil || scope.Refusal == nil {
		t.Fatalf("scope = %+v", scope)
	}
	var unavailable *TerminalPolicyUnavailable
	if err := EnforceNoRefusal(ctx); !errors.As(err, &unavailable) {
		t.Fatalf("enforce = %v", err)
	}
	if _, err := terminalPolicy(EnvInTerminalScope(ctx, &Env{Home: h})); !errors.As(err, &unavailable) {
		t.Fatalf("policy err = %v", err)
	}
	out := TerminalTool(ctx, EnvInTerminalScope(ctx, &Env{Home: h}), TerminalRequest{Command: "whoami"})
	if !strings.Contains(out, "terminal policy unavailable") {
		t.Fatalf("terminal result = %s", out)
	}
}

func TestGatewayRuntimeScopeResetsOnError(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	h := scopeProfile(t, tmp, "qa", "terminal:\n  backend: local\n", "")
	parent := context.Background()
	turn := func() (err error) {
		ctx := WithProfileTerminalScope(parent, h, nil)
		if scopedPolicy(t, ctx, h).Get("TERMINAL_ENV", "") != "local" {
			t.Fatal("turn did not see the profile backend")
		}
		return errors.New("turn blew up")
	}
	if err := turn(); err == nil || GetTerminalScope(parent) != nil {
		t.Fatal("scope survived the failed turn")
	}
}

func TestCronBoundaryBindsAndResets(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	h := scopeProfile(t, tmp, "dash", "terminal:\n  backend: local\n", "")
	parent := context.Background()
	ctx := WithPreparedTerminalScope(parent, h, nil)
	policy := scopedPolicy(t, ctx, h)
	if policy.Get("TERMINAL_ENV", "") != "local" || policy.Get("TERMINAL_SSH_HOST", "") != "" {
		t.Fatalf("policy = %v", policy)
	}
	prepared := PrepareProfileTerminalScope(parent, h, nil)
	if got := GetTerminalScope(WithPreparedTerminalScope(parent, h, prepared)); got != prepared {
		t.Fatal("prepared scope not installed")
	}
	if GetTerminalScope(parent) != nil {
		t.Fatal("scope leaked to the parent")
	}
}

func TestConfigListAndDictValuesAreJSONNotRepr(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	h := scopeProfile(t, tmp, "docker-lists", strings.Join([]string{"terminal:", "  backend: docker",
		"  docker_forward_env:", "    - EMAIL_HOME_ADDRESS", "  docker_volumes:", "    - /tmp/a:/data",
		"  docker_env:", "    FOO: bar", "  docker_extra_args:", "    - --network=host", ""}, "\n"), "")
	scope, err := BuildProfileTerminalScope(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"TERMINAL_DOCKER_FORWARD_ENV": []any{"EMAIL_HOME_ADDRESS"}, "TERMINAL_DOCKER_VOLUMES": []any{"/tmp/a:/data"},
		"TERMINAL_DOCKER_ENV": map[string]any{"FOO": "bar"}, "TERMINAL_DOCKER_EXTRA_ARGS": []any{"--network=host"},
	}
	for k, w := range want {
		var got any
		if err := json.Unmarshal([]byte(scope[k]), &got); err != nil || !reflect.DeepEqual(got, w) {
			t.Fatalf("%s = %q", k, scope[k])
		}
	}
	cfg, err := getEnvConfig(EnvInTerminalScope(WithProfileTerminalScope(context.Background(), h, nil), &Env{Home: h}))
	if err != nil {
		t.Fatal(err)
	}
	for k, w := range map[string]any{"docker_forward_env": []any{"EMAIL_HOME_ADDRESS"}, "docker_volumes": []any{"/tmp/a:/data"},
		"docker_env": map[string]any{"FOO": "bar"}, "docker_extra_args": []any{"--network=host"}} {
		if got := jsonRoundTrip(cfg[k]); !reflect.DeepEqual(got, w) {
			t.Fatalf("cfg %s = %#v", k, cfg[k])
		}
	}
}

func TestDotenvJSONStringsStayJSONStrings(t *testing.T) {
	tmp := pollutedLaunchEnv(t)
	h := scopeProfile(t, tmp, "dotenv-json", "terminal:\n  backend: docker\n",
		"TERMINAL_DOCKER_FORWARD_ENV=[\"EMAIL_HOME_ADDRESS\"]\nTERMINAL_DOCKER_VOLUMES=[\"/tmp/a:/data\"]\n")
	scope, err := BuildProfileTerminalScope(context.Background(), h, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope["TERMINAL_DOCKER_FORWARD_ENV"] != `["EMAIL_HOME_ADDRESS"]` || scope["TERMINAL_DOCKER_VOLUMES"] != `["/tmp/a:/data"]` {
		t.Fatalf("scope = %v", scope)
	}
}

func TestLaunchTurnBindsTerminalScopeOnceMultiplexingIsActive(t *testing.T) {
	launch := filepath.Join(t.TempDir(), ".icebath")
	scopeProfile(t, filepath.Dir(launch), "x", "", "")
	if err := os.MkdirAll(launch, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(launch, "config.yaml"), []byte("terminal:\n  backend: local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	t.Setenv("TERMINAL_ENV", "docker")
	t.Setenv("TERMINAL_DOCKER_IMAGE", "bee/img:1")
	ctx := WithProfileTerminalScope(context.Background(), launch, nil)
	if GetTerminalScope(ctx) == nil {
		t.Fatal("no scope bound")
	}
	policy := scopedPolicy(t, ctx, launch)
	if policy.Get("TERMINAL_ENV", "") != "local" || policy.Get("TERMINAL_DOCKER_IMAGE", "") == "bee/img:1" {
		t.Fatalf("policy = %v", policy)
	}
	if os.Getenv("TERMINAL_ENV") != "docker" || os.Getenv("TERMINAL_DOCKER_IMAGE") != "bee/img:1" {
		t.Fatal("process env mutated")
	}
}

func TestResolvePlaceholderTerminalCwd(t *testing.T) {
	if got, ok := ResolvePlaceholderTerminalCwd(".", "local", "/home/user/project", false, "/home/user"); !ok || got != "/home/user/project" {
		t.Fatalf("local placeholder = %q %v", got, ok)
	}
	if got, ok := ResolvePlaceholderTerminalCwd(".", "docker", "/home/user", false, "/home/user"); ok {
		t.Fatalf("docker mount off = %q", got)
	}
}

// Value as JSON decoding gives it back, so typed slices compare to decoded ones
func jsonRoundTrip(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}
