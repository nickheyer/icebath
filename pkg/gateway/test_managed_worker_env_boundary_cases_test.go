package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Launch home with a secondary profile under it, both env files written, the real secret source wired
func workerEnvHomes(t *testing.T, launchEnv, secondaryEnv string) (string, string) {
	t.Helper()
	root := t.TempDir()
	launch := filepath.Join(root, ".icebath")
	secondary := filepath.Join(launch, "profiles", "l106742sec")
	if err := os.MkdirAll(secondary, 0o700); err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, launch, ".env", launchEnv)
	envRefsWrite(t, secondary, ".env", secondaryEnv)
	t.Setenv("HOME", root)
	t.Setenv("ICEBATH_HOME", launch)
	swapSeam(t, &tools.ProfileSecrets, provider.ProfileSecretSource())
	return launch, secondary
}

// Child env of a worker owned by the profile home as a map, nil when the worker inherits the process env
func workerEnvFor(t *testing.T, ctx context.Context, profileHome string) map[string]string {
	t.Helper()
	pairs, err := workerEnv(ctx, &SessionAuthority{ProfileID: profileHome})
	if err != nil {
		t.Fatalf("worker env for %s: %v", profileHome, err)
	}
	if pairs == nil {
		return nil
	}
	env := map[string]string{}
	for _, pair := range pairs {
		key, value, _ := strings.Cut(pair, "=")
		env[key] = value
	}
	return env
}

// Sorted names whose value carries the marker
func keysHolding(env map[string]string, markers ...string) []string {
	out := []string{}
	for key, value := range env {
		for _, marker := range markers {
			if strings.Contains(value, marker) {
				out = append(out, key)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestSecondaryProfileWorkerGetsNoLaunchCredentials(t *testing.T) {
	for _, multiplex := range []bool{true, false} {
		hermetic.Run(t, strconv.FormatBool(multiplex), func(t *testing.T) {
			launch, secondary := workerEnvHomes(t, "OPENROUTER_API_KEY=LAUNCH-or\nMY_SERVICE_TOKEN=LAUNCH-mysvc\n", "OPENROUTER_API_KEY=SEC-or\n")
			for key, value := range map[string]string{"OPENROUTER_API_KEY": "LAUNCH-or", "MY_SERVICE_TOKEN": "LAUNCH-mysvc",
				"ANTHROPIC_API_KEY": "LAUNCH-anthropic", "VAULT_ONLY_TOKEN": "LAUNCH-vault"} {
				t.Setenv(key, value)
			}
			t.Cleanup(provider.AttributeSecretSourceForTests("VAULT_ONLY_TOKEN", "bitwarden"))
			provider.SetMultiplexActive(multiplex)
			t.Cleanup(func() { provider.SetMultiplexActive(false) })
			env := workerEnvFor(t, context.Background(), secondary)
			own := workerEnvFor(t, context.Background(), launch)
			if env == nil || env["OPENROUTER_API_KEY"] != "SEC-or" {
				t.Fatalf("secondary worker env %v", env)
			}
			if leaked := keysHolding(env, "LAUNCH"); len(leaked) != 0 {
				t.Fatalf("launch values reached the secondary worker under %v", leaked)
			}
			if !multiplex && own != nil {
				t.Fatalf("single profile launch worker env %v, want the inherited process env", own)
			}
		})
	}
}

func TestLaunchProfileWorkerKeepsEnvOnlyToolCredentialsUnderMultiplex(t *testing.T) {
	launch, secondary := workerEnvHomes(t, "OPENROUTER_API_KEY=LAUNCH-or\n", "OPENROUTER_API_KEY=SEC-or\n")
	t.Setenv("FIRECRAWL_API_KEY", "ENVONLY-fc")
	t.Setenv("AUXILIARY_VISION_API_KEY", "ENVONLY-aux")
	provider.ActivateMultiProfileHosting()
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	scoped := provider.WithLaunchProfileRuntimeScope(context.Background(), launch)
	if value, _, err := provider.GetSecret(scoped, "FIRECRAWL_API_KEY"); err != nil || value != "ENVONLY-fc" {
		t.Fatalf("owner resolves FIRECRAWL_API_KEY as %q %v", value, err)
	}
	own := workerEnvFor(t, context.Background(), launch)
	other := workerEnvFor(t, context.Background(), secondary)
	if own["FIRECRAWL_API_KEY"] != "ENVONLY-fc" || own["OPENROUTER_API_KEY"] != "LAUNCH-or" {
		t.Fatalf("launch worker env %v", own)
	}
	if _, present := own["AUXILIARY_VISION_API_KEY"]; present {
		t.Fatal("an icebath internal secret reached the launch worker")
	}
	if _, present := other["FIRECRAWL_API_KEY"]; present || other["OPENROUTER_API_KEY"] != "SEC-or" {
		t.Fatalf("secondary worker env %v", other)
	}
	if leaked := keysHolding(other, "ENVONLY", "LAUNCH"); len(leaked) != 0 {
		t.Fatalf("launch values reached the secondary worker under %v", leaked)
	}
}

func TestSecondaryProfileWorkerUnderMultiplexResolvesItsDeclaredPassthroughWithoutFailingClosed(t *testing.T) {
	_, secondary := workerEnvHomes(t, "LAUNCH_ONLY_TOKEN=launch-secret\n", "SKILL_TOKEN=skill-secret\nSVC_TOKEN=served-secret\n")
	envRefsWrite(t, secondary, "config.yaml", "terminal:\n  env_passthrough: [SVC_TOKEN]\n")
	t.Setenv("LAUNCH_ONLY_TOKEN", "launch-secret")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	t.Setenv("SVC_TOKEN", "")
	os.Unsetenv("SVC_TOKEN")
	tools.RegisterEnvPassthrough(nil, []string{"SKILL_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	env := workerEnvFor(t, ProfileRuntimeScope(context.Background(), secondary, false), secondary)
	if env["SKILL_TOKEN"] != "skill-secret" || env["SVC_TOKEN"] != "served-secret" || env["ICEBATH_HOME"] != secondary {
		t.Fatalf("secondary worker env %v", env)
	}
	if leaked := keysHolding(env, "launch-"); len(leaked) != 0 {
		t.Fatalf("launch values reached the secondary worker under %v", leaked)
	}
}
