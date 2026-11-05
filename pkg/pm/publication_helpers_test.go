package pm

import (
	"context"
	"path/filepath"
	"testing"
)

// Venv state double whose Prepare and Stamp run hooks before or instead of the catalogue venv
type pubVenv struct {
	*Venv
	prepare func(ctx context.Context, extras []string, opts VenvApplyOptions) (*VenvGeneration, error)
	stamp   func(ctx context.Context, extras []string, pluginDirs []MemberDir) (string, error)
}

func (v *pubVenv) Prepare(ctx context.Context, extras []string, opts VenvApplyOptions) (*VenvGeneration, error) {
	if v.prepare != nil {
		return v.prepare(ctx, extras, opts)
	}
	return v.Venv.Prepare(ctx, extras, opts)
}

func (v *pubVenv) Stamp(ctx context.Context, extras []string, pluginDirs []MemberDir) (string, error) {
	if v.stamp != nil {
		return v.stamp(ctx, extras, pluginDirs)
	}
	return v.Venv.Stamp(ctx, extras, pluginDirs)
}

// Registers a venv double in place of the catalogue venv until the test ends
func pubSwapVenv(t *testing.T, v *pubVenv) *pubVenv {
	t.Helper()
	if v.Venv == nil {
		v.Venv = newVenv("")
	}
	v.Venv.BasePackage.bind(v)
	registryMu.Lock()
	saved := packages
	swapped := map[string]Package{}
	for name, p := range saved {
		swapped[name] = p
	}
	swapped["venv"] = v
	packages = swapped
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		packages = saved
		registryMu.Unlock()
	})
	return v
}

// Client environment with an empty lock under a temp dir, returning the dir and the icebath home
func pubClient(t *testing.T) (string, string) {
	t.Helper()
	tmp := ieClientEnv(t)
	return tmp, filepath.Join(tmp, ".icebath")
}

// Project whose recorded venv fact is current for these member dirs over an existing environment
func pubCurrentEnvironment(t *testing.T, tmp string, members []string) string {
	t.Helper()
	ctx := context.Background()
	repo := filepath.Join(tmp, "project")
	peWrite(t, filepath.Join(repo, "uv.lock"), "version = 1\n")
	peSwap(t, &repoRoot, func() string { return repo })
	environment := filepath.Join(InstallStateDir(ctx, repo), "environments", "existing", "venv")
	peWrite(t, filepath.Join(environment, "pyvenv.cfg"), "home = test\n")
	stamp, err := newVenv("").Stamp(ctx, []string{}, sameMemberDirs(members))
	if err != nil {
		t.Fatal(err)
	}
	facts, err := NewFacts(RuntimeFactsPath(ctx), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := facts.RecordState("venv", stamp, []string{}, environment, ""); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Install state file of a project
func pubState(t *testing.T, repo, name string) string {
	t.Helper()
	return filepath.Join(InstallStateDir(context.Background(), repo), name)
}

// Strings as the JSON list a plugin input row carries
func pubList(items ...string) []any {
	out := []any{}
	for _, item := range items {
		out = append(out, item)
	}
	return out
}
