package pm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Test directory standing for the user's home, returning it with the platform default data root inside it
func bhLayout(t *testing.T) (string, string) {
	t.Helper()
	tmp := isolateHome(t)
	if runtime.GOOS == "windows" {
		t.Setenv("LOCALAPPDATA", tmp)
	}
	t.Setenv(EnvDisableLazyInstalls, "")
	os.Unsetenv(EnvDisableLazyInstalls)
	return tmp, home.PlatformDefault()
}

// Source checkout in the install.sh layout under parent, with its git dir, pyproject and self update stamp
func bhCheckout(t *testing.T, parent string) string {
	t.Helper()
	root := filepath.Join(parent, "icebath-agent")
	sttMkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, "pyproject.toml"), []byte("[project]\nname='example'\n"), 0o666)
	stamp, _ := json.Marshal(map[string]any{"updateMechanism": "self", "commit": "abc"})
	writeFile(t, filepath.Join(root, "install-stamp.json"), stamp, 0o666)
	return root
}

// Records the committed dependency state a data root keeps for the checkout
func bhState(t *testing.T, dataRoot, checkout string) {
	t.Helper()
	facts, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"stamp": "complete", "extras": []any{"all"}}}})
	writeFile(t, filepath.Join(dataRoot, "installs", InstallKey(checkout), "facts.json"), facts, 0o666)
}

// Makes path the process data root
func bhHome(t *testing.T, path string) string {
	t.Helper()
	sttMkdir(t, path)
	t.Setenv(home.EnvHome, path)
	return path
}

// Fails unless the checkout's store resolves to want
func bhWantStore(t *testing.T, root, want string) {
	t.Helper()
	store, err := EnvironmentStoreRoot(context.Background(), root, true)
	if err != nil || store != want {
		t.Fatalf("store %q %v, want %q", store, err, want)
	}
}

func TestAFreshInstallAndACustomRootKeepTheirOwnStore(t *testing.T) {
	tmp, def := bhLayout(t)
	ctx := context.Background()
	custom := filepath.Join(tmp, "data", "h")
	root := bhCheckout(t, custom)
	bhHome(t, custom)
	if owner := OwningHomeRoot(ctx, root); owner != "" {
		t.Fatalf("a fresh install has owner %q", owner)
	}
	bhWantStore(t, root, filepath.Join(custom, "tools"))
	bhState(t, custom, root)
	bhState(t, def, root)
	if owner := OwningHomeRoot(ctx, root); owner != "" {
		t.Fatalf("a custom root that owns its tree has owner %q", owner)
	}
	bhWantStore(t, root, filepath.Join(custom, "tools"))
}

func TestABorrowersOwnDependencyStateNeverMakesItTheOwner(t *testing.T) {
	tmp, def := bhLayout(t)
	ctx := context.Background()
	custom := filepath.Join(tmp, "data", "h")
	root := bhCheckout(t, custom)
	bhState(t, custom, root)
	t.Setenv(home.EnvHome, "")
	os.Unsetenv(home.EnvHome)
	if owner := OwningHomeRoot(ctx, root); owner != resolvePath(custom) {
		t.Fatalf("owner %q, want %q", owner, custom)
	}
	bhState(t, def, root)
	if owner := OwningHomeRoot(ctx, root); owner != resolvePath(custom) {
		t.Fatalf("after the borrower's own sync the owner is %q, want %q", owner, custom)
	}
	bhWantStore(t, root, filepath.Join(resolvePath(custom), "tools"))
}

func TestNamedContextProfileSharesItsInstallRoot(t *testing.T) {
	tmp := isolateHome(t)
	homeDir := filepath.Join(tmp, "home")
	profile := sttMkdir(t, filepath.Join(homeDir, "profiles", "worker"))
	t.Setenv(home.EnvHome, homeDir)
	project := filepath.Join(tmp, "repo")
	state := InstallStateDir(context.Background(), project)
	if got := InstallStateDir(home.WithOverride(context.Background(), profile), project); got != state {
		t.Fatalf("profile state %q, want %q", got, state)
	}
}
