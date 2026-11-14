package pm

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestReleaseRemovesTheLeaseFile(t *testing.T) {
	tmp := isolateHome(t)
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	t.Setenv(home.EnvHome, filepath.Join(tmp, "home"))
	ctx := context.Background()
	state := InstallStateDir(ctx, repo)
	for _, name := range []string{"first", "second"} {
		venv := filepath.Join(state, "environments", name, "venv")
		writeFile(t, filepath.Join(venv, "pyvenv.cfg"), []byte("version = 3.11"), 0o666)
		site := filepath.Join(venv, "lib", "python3.11", "site-packages")
		if runtime.GOOS == "windows" {
			site = filepath.Join(venv, "Lib", "site-packages")
		}
		sttMkdir(t, site)
		sttTouch(t, filepath.Join(filepath.Dir(venv), ".lease-managed"))
	}
	selectEnvironment := func(name string) string {
		environment := filepath.Join(state, "environments", name, "venv")
		facts, _ := json.Marshal(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": environment}}})
		writeFile(t, EnvironmentRuntimeFactsPath(ctx, repo), facts, 0o666)
		return resolvePath(environment)
	}

	environment := selectEnvironment("first")
	release, err := LeaseGeneration(environment)
	if err != nil {
		t.Fatal(err)
	}
	leases := filepath.Join(filepath.Dir(environment), ".leases")
	if len(sttChildren(t, leases)) == 0 {
		release()
		t.Fatal("no lease file while leased")
	}
	release()
	sttSamePaths(t, sttChildren(t, leases), nil)
}
