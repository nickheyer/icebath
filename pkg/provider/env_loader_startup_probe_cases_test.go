package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Process argv for the test, the real argv restored afterwards
func withProcessArgs(t *testing.T, args ...string) {
	t.Helper()
	prev := os.Args
	os.Args = args
	t.Cleanup(func() { os.Args = prev })
}

func TestNormalStartupStillLoadsEnabledExternalSecretSource(t *testing.T) {
	root := secretSourceFixture(t)
	h := envLoaderProfile(t, root, "hermes-home")
	writeProfileConfig(t, h, "secrets:\n  bitwarden:\n    enabled: true\n    project_id: test-project\n")
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("BWS_ACCESS_TOKEN", "")
	skipExternalSecretSourcesFn = pm.ShouldSkipExternalSecretSources
	withProcessArgs(t, "icebath", "chat")
	freshSecretRegistry(t)
	mustLoad(t, DotenvLoadOptions{})
	if !BuiltinSecretSourcesLoaded() {
		t.Fatal("a normal startup left the enabled Bitwarden source unloaded")
	}
}

func TestFirstConfigReadBeforeDotenvUsesTheDotenvDeployment(t *testing.T) {
	p := setupBootPlane(t, true)
	deployment := bootPlaneEnv(p, true)
	managed := filepath.Join(filepath.Dir(p.Home), "managed")
	if err := os.MkdirAll(managed, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvBytes(t, filepath.Join(managed, ".env"), []byte(remote.InstanceEnv+"="+deployment[remote.InstanceEnv]+"\n"))
	delete(deployment, remote.InstanceEnv)
	var homeEnv strings.Builder
	for name, value := range deployment {
		if name != config.BackendEnv {
			homeEnv.WriteString(name + "=" + value + "\n")
		}
	}
	writeEnvBytes(t, filepath.Join(p.Home, ".env"), []byte(homeEnv.String()))
	for name := range bootPlaneEnv(p, true) {
		_ = os.Unsetenv(name)
	}
	t.Setenv(config.BackendEnv, "remote")
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	p.Upper = map[string]any{"display": map[string]any{"personality": "concise"}}
	config.ResetBootstrapForTests()
	t.Cleanup(config.ResetBootstrapForTests)
	first, err := config.LoadContext(context.Background(), p.Home, os.LookupEnv)
	if err != nil || first.Get("display.personality") != "concise" {
		t.Fatalf("first read before the dotenv load %v %v", err, first.Get("display.personality"))
	}
	if err := loadDotenv(p); err != nil {
		t.Fatal(err)
	}
	tree, err := config.LoadContext(context.Background(), p.Home, os.LookupEnv)
	if err != nil || tree.Get("display.personality") != "concise" {
		t.Fatalf("read after the dotenv load %v %v", err, tree.Get("display.personality"))
	}
}
