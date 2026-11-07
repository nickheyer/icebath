package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/profiles"
)

func resetLaunchPolicy(t *testing.T) {
	t.Helper()
	launchEnvSnapshot.Lock()
	launchEnvSnapshot.env = nil
	launchEnvSnapshot.Unlock()
	SetMultiplexActive(false)
	t.Cleanup(func() {
		launchEnvSnapshot.Lock()
		launchEnvSnapshot.env = nil
		launchEnvSnapshot.Unlock()
		SetMultiplexActive(false)
		launchProfilesToServe = profiles.ProfilesToServe
		enableProfileLogRouting = logging.EnableProfileLogRouting
	})
}

func TestCaptureLaunchEnvFirstCaptureWins(t *testing.T) {
	resetLaunchPolicy(t)
	t.Setenv("LPP_PROBE", "first")
	snap := CaptureLaunchEnv()
	snap["LPP_PROBE"] = "mutated"
	t.Setenv("LPP_PROBE", "second")
	t.Setenv("TERMINAL_ENV", "ssh")
	if got := CaptureLaunchEnv()["LPP_PROBE"]; got != "first" {
		t.Fatalf("frozen snapshot %q", got)
	}
	if _, ok := LaunchTerminalEnv()["TERMINAL_ENV"]; ok {
		t.Fatal("terminal overlay comes from the frozen snapshot")
	}
}

func TestLaunchSecretScopeEnvFileWinsAndFreezesOnActivation(t *testing.T) {
	resetLaunchPolicy(t)
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	os.WriteFile(filepath.Join(h, ".env"), []byte("LPP_SHARED=from-file\n"), 0o600)
	t.Setenv("LPP_SHARED", "from-env")
	t.Setenv("LPP_ENV_ONLY", "systemd")
	if IsGlobalEnv("LPP_ENV_ONLY") {
		t.Fatal("probe key must be a profile key")
	}
	scope := LaunchSecretScope(h)
	if scope["LPP_SHARED"] != "from-file" || scope["LPP_ENV_ONLY"] != "systemd" {
		t.Fatalf("precedence %q %q", scope["LPP_SHARED"], scope["LPP_ENV_ONLY"])
	}
	ActivateMultiProfileHosting()
	if !MultiplexProcess() {
		t.Fatal("activation flips fail closed")
	}
	t.Setenv("LPP_ENV_ONLY", "poisoned")
	if got := LaunchSecretScope(h)["LPP_ENV_ONLY"]; got != "systemd" {
		t.Fatalf("frozen launch env %q", got)
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, ".env"), []byte("LPP_SHARED=other\n"), 0o600)
	owner := OwnerSecretScope(context.Background(), other)
	if owner["LPP_SHARED"] != "other" || owner["LPP_ENV_ONLY"] != "" {
		t.Fatalf("secondary scope comes from its own files %v", owner)
	}
	if OwnerSecretScope(context.Background(), h)["LPP_ENV_ONLY"] != "systemd" {
		t.Fatal("process home owner scope is the launch scope")
	}
}

func TestLaunchProfileScopeIsUnchangedBeforeMultiplexing(t *testing.T) {
	resetLaunchPolicy(t)
	ctx := context.Background()
	if withLaunchProfileScopeIfMultiplexed(ctx) != ctx {
		t.Fatal("single profile host binds nothing")
	}
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	os.WriteFile(filepath.Join(h, ".env"), []byte("LPP_BODY=launch\n"), 0o600)
	body := WithLaunchBody(ctx)
	resolver, _ := body.Value(launchBodyKey{}).(func() map[string]string)
	if resolver == nil || resolver()["LPP_BODY"] != "launch" {
		t.Fatal("launch body resolves from the launch scope")
	}
}

func TestEagerActivation(t *testing.T) {
	resetLaunchPolicy(t)
	h := t.TempDir()
	t.Setenv(home.EnvHome, h)
	launchProfilesToServe = func(ctx context.Context, multiplex, standalone, parked bool) ([]profiles.ServedProfile, error) {
		return []profiles.ServedProfile{{Name: "default", Home: h}}, nil
	}
	if ok, err := ActivateMultiProfileHostingEagerly(context.Background()); ok || err != nil || MultiplexProcess() {
		t.Fatal("single servable home never activates")
	}
	launchProfilesToServe = func(ctx context.Context, multiplex, standalone, parked bool) ([]profiles.ServedProfile, error) {
		return nil, errors.New("permission denied")
	}
	if ok, err := ActivateMultiProfileHostingEagerly(context.Background()); !ok || err != nil || !MultiplexProcess() {
		t.Fatal("unreadable profiles directory fails closed")
	}
	if ok, _ := ActivateMultiProfileHostingEagerly(context.Background()); ok {
		t.Fatal("already active returns false")
	}
}

// Fake HOME whose root home holds one named profile b, returning the b profile directory
func twoProfileHost(t *testing.T) string {
	t.Helper()
	resetLaunchPolicy(t)
	tmp := t.TempDir()
	root := filepath.Join(tmp, "fakehome", ".icebath")
	b := filepath.Join(root, "profiles", "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(tmp, "fakehome"))
	t.Setenv(home.EnvHome, root)
	os.Unsetenv("GATEWAY_MULTIPLEX_PROFILES")
	if !strings.HasPrefix(profiles.ProfilesRoot(), tmp) {
		t.Fatalf("profiles root %q escapes the fake home", profiles.ProfilesRoot())
	}
	return b
}

func TestEagerActivationArmsForARealSecondProfile(t *testing.T) {
	b := twoProfileHost(t)
	os.WriteFile(filepath.Join(b, "config.yaml"), []byte("{}\n"), 0o600)
	enableProfileLogRouting = func([]string) (bool, error) { return true, nil }
	if ok, err := ActivateMultiProfileHostingEagerly(context.Background()); !ok || err != nil || !IsMultiplexActive(context.Background()) {
		t.Fatalf("second profile with an identity activates, got %v %v", ok, err)
	}
}

func TestEagerActivationRoutesLogsPerProfileHome(t *testing.T) {
	b := twoProfileHost(t)
	os.WriteFile(filepath.Join(b, "config.yaml"), []byte("{}\n"), 0o600)
	var routed [][]string
	enableProfileLogRouting = func(homes []string) (bool, error) {
		routed = append(routed, append([]string(nil), homes...))
		return true, nil
	}
	if ok, err := ActivateMultiProfileHostingEagerly(context.Background()); !ok || err != nil {
		t.Fatalf("activation, got %v %v", ok, err)
	}
	if len(routed) != 1 || len(routed[0]) != 2 || !slices.Contains(routed[0], launchResolvedPath(b)) {
		t.Fatalf("both homes routed, got %v", routed)
	}
}

func TestEagerActivationRetiredOptOutNoLongerDisarmsTheCredentialGuard(t *testing.T) {
	b := twoProfileHost(t)
	os.WriteFile(filepath.Join(b, "config.yaml"), []byte("{}\n"), 0o600)
	os.WriteFile(filepath.Join(os.Getenv(home.EnvHome), "config.yaml"), []byte("gateway:\n  multiplex_profiles: false\n"), 0o600)
	enableProfileLogRouting = func([]string) (bool, error) { return true, nil }
	if ok, err := ActivateMultiProfileHostingEagerly(context.Background()); !ok || err != nil || !IsMultiplexActive(context.Background()) {
		t.Fatalf("multiplex_profiles false still activates, got %v %v", ok, err)
	}
}

func TestEagerActivationCrashedProfileCreateShellIsNotASecondTenant(t *testing.T) {
	b := twoProfileHost(t)
	os.WriteFile(filepath.Join(b, ".env"), nil, 0o600)
	if ok, err := ActivateMultiProfileHostingEagerly(context.Background()); ok || err != nil || IsMultiplexActive(context.Background()) {
		t.Fatalf("empty .env never flips the host, got %v %v", ok, err)
	}
}

func TestEagerActivationSurfacesALogRoutingFailure(t *testing.T) {
	b := twoProfileHost(t)
	os.WriteFile(filepath.Join(b, "config.yaml"), []byte("{}\n"), 0o600)
	boom := errors.New("routing failed")
	enableProfileLogRouting = func([]string) (bool, error) { return false, boom }
	if ok, err := ActivateMultiProfileHostingEagerly(context.Background()); !ok || !errors.Is(err, boom) || !IsMultiplexActive(context.Background()) {
		t.Fatalf("routing error reaches the caller after activation, got %v %v", ok, err)
	}
}
