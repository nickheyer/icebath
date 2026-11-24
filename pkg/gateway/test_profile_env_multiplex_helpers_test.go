package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	orderedutil "github.com/nickheyer/icebath/pkg/utils/ordered"
)

// Runner of a multiplexing gateway, every owner handler scoped to the profile owning its authority
type profileEnvMultiplexRunner struct {
	secretScopeRunner
}

func (profileEnvMultiplexRunner) MultiplexProfiles() bool { return true }

// Launch home with a launch only credential in its env file and the process env, a provider key and the real secret source
func profileEnvLaunchHome(t *testing.T) string {
	t.Helper()
	launch := filepath.Join(t.TempDir(), "launch")
	if err := os.MkdirAll(launch, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	swapSeam(t, &tools.ProfileSecrets, provider.ProfileSecretSource())
	quietHandleHealth(t)
	installPrefillLoaders(t)
	envRefsWrite(t, launch, ".env", "LAUNCH_ONLY_TOKEN=launch-secret\n")
	t.Setenv("LAUNCH_ONLY_TOKEN", "launch-secret")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-"+strings.Repeat("f", 48))
	for _, name := range []string{"SVC_TOKEN", "SKILL_TOKEN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return launch
}

// Multiplexed secondary profile under launch declaring SVC_TOKEN in config and holding a skill registered SKILL_TOKEN
func profileEnvServedHome(t *testing.T, launch string) string {
	t.Helper()
	served := filepath.Join(launch, "profiles", "beta")
	if err := os.MkdirAll(served, 0o700); err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, served, ".env", "SVC_TOKEN=served-secret\nSKILL_TOKEN=skill-secret\n")
	envRefsWrite(t, served, "config.yaml", "model: {default: m1, provider: custom, base_url: \"http://127.0.0.1:9/v1\", api_key: route-token}\n"+
		"platform_toolsets: {cli: [terminal]}\nterminal: {backend: local, env_passthrough: [SVC_TOKEN]}\n")
	t.Setenv("SKILL_TOKEN", "launch-skill")
	tools.RegisterEnvPassthrough(nil, []string{"SKILL_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	return served
}

// Owner connection of a profile home holding one created local session in work, and that session id
func profileEnvOwner(t *testing.T, ctx context.Context, profileHome, work string, multiplex bool) (*AuthorityConnection, string) {
	t.Helper()
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(profileHome, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := NewSessionStore(ctx, filepath.Join(profileHome, "sessions"), DefaultGatewayConfig(ctx), nil)
	t.Cleanup(store.CloseAllDBHandles)
	store.PinDB(db)
	base := &recoverTestRunner{store: store, adapters: orderedutil.NewMap[Platform, PlatformAdapter](), profiles: orderedutil.NewMap[string, *orderedutil.Map[Platform, PlatformAdapter]]()}
	var runner AuthorityRunner = secretScopeRunner{base}
	if multiplex {
		runner = profileEnvMultiplexRunner{secretScopeRunner{base}}
	}
	a, err := InitializeSessionAuthority(ctx, runner, profileHome, "shell", db, false)
	if err != nil {
		t.Fatal(err)
	}
	base.launch = a
	owner := secretScopeConnect(t, a)
	t.Cleanup(func() { _ = owner.Close(ctx) })
	sid, _ := secretScopeCall(t, ctx, owner, "session.create", map[string]any{"request_id": "shell", "source": "cli", "cwd": work})["session_id"].(string)
	return owner, sid
}
