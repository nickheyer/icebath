package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
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
	if ActivateMultiProfileHostingEagerly(context.Background()) || MultiplexProcess() {
		t.Fatal("single servable home never activates")
	}
	launchProfilesToServe = func(ctx context.Context, multiplex, standalone, parked bool) ([]profiles.ServedProfile, error) {
		return nil, errors.New("permission denied")
	}
	if !ActivateMultiProfileHostingEagerly(context.Background()) || !MultiplexProcess() {
		t.Fatal("unreadable profiles directory fails closed")
	}
	if ActivateMultiProfileHostingEagerly(context.Background()) {
		t.Fatal("already active returns false")
	}
}
