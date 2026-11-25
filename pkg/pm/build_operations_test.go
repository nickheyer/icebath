//go:build !icebath_core && !icebath_noextras

package pm

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
)

// Every regular file under a root with its bytes
func peTreeBytes(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			rel, _ := filepath.Rel(root, path)
			out[rel] = peRead(t, path)
		}
		return nil
	})
	return out
}

// Build tools environment and the isolated home build operations run against
func peBuildTools(t *testing.T) (string, map[string]string) {
	t.Helper()
	tmp := isolateHome(t)
	peToolchain(t, peUV(t), peHostPython(t))
	env := map[string]string{}
	for k, v := range environValue() {
		if !strings.HasPrefix(k, "UV_") && !strings.HasPrefix(k, "PYTHON") && k != "VIRTUAL_ENV" {
			env[k] = v
		}
	}
	h := filepath.Join(tmp, "home")
	os.MkdirAll(h, 0o777)
	env["HOME"], env["USERPROFILE"], env["ICEBATH_HOME"] = h, h, filepath.Join(h, ".icebath")
	t.Setenv("ICEBATH_HOME", env["ICEBATH_HOME"])
	t.Setenv(EnvDisableLazyInstalls, "")
	os.Unsetenv(EnvDisableLazyInstalls)
	return tmp, env
}

// A locked offline source with an extra carrying a marker and a dev group
func peLockedSource(t *testing.T, tmp string, env map[string]string) string {
	t.Helper()
	wheels := filepath.Join(tmp, "wheelhouse")
	for _, name := range []string{"base_dep", "chosen_dep", "dev_dep"} {
		peWheel(t, wheels, name, "", nil, nil)
	}
	source := filepath.Join(tmp, "source with spaces")
	wheelsJSON, _ := json.Marshal(filepath.ToSlash(wheels))
	peWrite(t, filepath.Join(source, "pyproject.toml"), "[project]\nname=\"export-root\"\nversion=\"1\"\nrequires-python=\">=3.11\"\n"+
		"dependencies=[\"base-dep==1.0\"]\n[project.optional-dependencies]\nchosen=[\"chosen-dep==1.0; sys_platform == 'win32'\"]\n"+
		"[dependency-groups]\ndev=[\"dev-dep==1.0\"]\n[tool.uv]\npackage=false\nno-index=true\nfind-links=["+string(wheelsJSON)+"]\n")
	if err := LockProject(context.Background(), LockProjectOptions{Source: source, Python: peHostPython(t), Cache: filepath.Join(tmp, "cache"),
		Env: env, Offline: true, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestBuildOperationsCheckLockNeverChangesSource(t *testing.T) {
	for _, state := range []string{"current", "stale", "missing"} {
		t.Run(state, func(t *testing.T) {
			tmp, env := peBuildTools(t)
			source := peLockedSource(t, tmp, env)
			switch state {
			case "stale":
				manifest := filepath.Join(source, "pyproject.toml")
				peWrite(t, manifest, strings.Replace(peRead(t, manifest), `version="1"`, `version="2"`, 1))
			case "missing":
				os.Remove(filepath.Join(source, "uv.lock"))
			}
			before := peTreeBytes(t, source)
			err := CheckProjectLock(context.Background(), CheckProjectLockOptions{Source: source, Python: peHostPython(t), Cache: filepath.Join(tmp, "cache"),
				Env: env, Offline: true, Explicit: true})
			var ie *InstallError
			if state == "current" && err != nil || state != "current" && !errors.As(err, &ie) {
				t.Fatalf("check %s: %v", state, err)
			}
			if !reflect.DeepEqual(peTreeBytes(t, source), before) || existsOrLink(filepath.Join(source, ".venv")) {
				t.Fatal("check changed the source")
			}
		})
	}
}

func TestBuildOperationsReadyToolsDoNotBypassDisabledLazyOperations(t *testing.T) {
	for _, operation := range []string{"check"} {
		t.Run(operation, func(t *testing.T) {
			tmp, env := peBuildTools(t)
			source := peLockedSource(t, tmp, env)
			t.Setenv(EnvDisableLazyInstalls, "1")
			ctx := context.Background()
			out := filepath.Join(tmp, "blocked-output")
			before := peTreeBytes(t, source)
			err := checkProjectLock(ctx, CheckProjectLockOptions{Source: source, Env: env, Cache: filepath.Join(tmp, "cache")})
			if err == nil || !strings.Contains(err.Error(), "lazy installs are disabled") {
				t.Fatalf("%s: %v", operation, err)
			}
			if existsOrLink(out) || !reflect.DeepEqual(peTreeBytes(t, source), before) {
				t.Fatal("refused operation wrote output")
			}
			if err := checkProjectLock(ctx, CheckProjectLockOptions{Source: source, Env: env, Cache: filepath.Join(tmp, "cache"), Offline: true}); err != nil {
				t.Fatalf("passive check: %v", err)
			}
		})
	}
}
