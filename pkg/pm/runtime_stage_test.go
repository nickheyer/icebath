package pm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Reference project whose lock pins the PM runtime graph the Python engine shipped
func peRuntimeProject(t *testing.T) string {
	t.Helper()
	project := filepath.Join(peReferences(t, "pm/"), "pm")
	if !isRegularFile(filepath.Join(project, "uv.lock")) {
		t.Fatalf("runtime project %s has no lock", project)
	}
	return project
}

// Host compatible wheels of the runtime project's lock, downloaded once and hash checked, with their versions
func peRuntimeWheelhouse(t *testing.T) (string, map[string]string) {
	t.Helper()
	project := peRuntimeProject(t)
	script := "import json, sys, tomllib\nfrom packaging.tags import sys_tags\nfrom packaging.utils import parse_wheel_filename\n" +
		"lock = tomllib.loads(open(sys.argv[1], encoding='utf-8-sig').read())\ntags = set(sys_tags())\nout = []\n" +
		"for package in lock['package']:\n    if 'registry' not in package.get('source', {}):\n        continue\n" +
		"    choices = [w for w in package['wheels'] if parse_wheel_filename(w['url'].rsplit('/', 1)[1])[3] & tags]\n" +
		"    out.append([package['name'], package['version'], choices[0]['url'], choices[0]['hash']])\nprint(json.dumps(out))"
	cmd := exec.Command(peHostPython(t), "-I", "-c", script, filepath.Join(project, "uv.lock"))
	var stdout bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("wheel selection: %v", err)
	}
	var rows [][4]string
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	wheelhouse := filepath.Join(t.TempDir(), "pm-wheelhouse")
	os.MkdirAll(wheelhouse, 0o777)
	cache := filepath.Join(peCacheRoot, "icebath-pm-tests", "wheels")
	os.MkdirAll(cache, 0o777)
	versions := map[string]string{}
	client := &http.Client{Timeout: 5 * time.Minute}
	for _, row := range rows {
		name := row[2][strings.LastIndex(row[2], "/")+1:]
		cached := filepath.Join(cache, name)
		data, err := os.ReadFile(cached)
		if err != nil {
			resp, err := client.Get(row[2])
			if err != nil {
				t.Fatal(err)
			}
			data, err = io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		sum := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(sum[:]) != row[3] {
			t.Fatalf("%s hash mismatch", name)
		}
		os.WriteFile(cached, data, 0o644)
		writeFile(t, filepath.Join(wheelhouse, name), data, 0o644)
		versions[row[0]] = row[1]
	}
	return wheelhouse, versions
}

// Isolated builder home whose config would break any uv that read it
func peIsolatedBuilder(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("platforms linux")
	}
	tmp := isolateHome(t)
	t.Setenv(EnvRuntimeDir, filepath.Join(tmp, "tools"))
	t.Chdir(tmp)
	peWrite(t, filepath.Join(tmp, "uv.toml"), "required-version = \"<0.1\"\n")
	cache, err := UvCacheDir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(cache)
	for _, entry := range entries {
		if entry.Name() != ".seeded" {
			t.Fatalf("uv cache already holds %s", entry.Name())
		}
	}
	return tmp
}

func TestRuntimeStageOfflineWheelhouseStagesTheLockedRuntimeGraph(t *testing.T) {
	tmp := peIsolatedBuilder(t)
	wheelhouse, versions := peRuntimeWheelhouse(t)
	python := peHostPython(t)
	peToolchain(t, peUV(t), python)
	destination := filepath.Join(tmp, "payload", "pm-runtime")
	executable, err := StageManagerRuntime(context.Background(), StageManagerRuntimeOptions{Python: python, Destination: destination,
		Project: peRuntimeProject(t), Wheelhouse: wheelhouse, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if !isRegularFile(executable) {
		t.Fatalf("executable %s", executable)
	}
	marker := filepath.Join(destination, "pm-runtime.json")
	st, err := os.Stat(marker)
	if err != nil || st.Mode().Perm() != 0o600 || strings.TrimSpace(peRead(t, marker)) != "{}" {
		t.Fatalf("marker %v %v %q", st, err, peRead(t, marker))
	}
	if !isRegularFile(filepath.Join(destination, ".lock")) {
		t.Fatal("venv lock file missing")
	}
	probe := "import importlib.metadata, importlib.util, json, sys\nfrom packaging.utils import canonicalize_name\nfrom ruamel.yaml import YAML\n" +
		"import packaging, tomli_w\nassert YAML(typ='safe').load('isolated: true') == {'isolated': True}\n" +
		"assert importlib.util.find_spec('openai') is None\nassert importlib.util.find_spec('yaml') is None\n" +
		"print(json.dumps({canonicalize_name(d.metadata['Name']): d.version for d in importlib.metadata.distributions(path=[sys.argv[1]])}))"
	env, err := RuntimeEnvironment(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	site := peSitePackages(t, destination)
	out := peRun(t, []string{executable, "-I", "-B", "-c", probe, site}, tmp, env)
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil || !reflect.DeepEqual(got, versions) {
		t.Fatalf("distributions %s want %v", out, versions)
	}
}

func TestRuntimeStageOfflineWheelhouseRejectsMissingLockedWheel(t *testing.T) {
	tmp := peIsolatedBuilder(t)
	wheelhouse, _ := peRuntimeWheelhouse(t)
	incomplete := filepath.Join(tmp, "incomplete wheelhouse")
	os.MkdirAll(incomplete, 0o777)
	entries, _ := os.ReadDir(wheelhouse)
	removed := false
	for _, entry := range entries {
		if distribution, _, _ := strings.Cut(entry.Name(), "-"); CanonicalizeName(distribution) == "ruamel-yaml" && !removed {
			removed = true
			continue
		}
		writeFile(t, filepath.Join(incomplete, entry.Name()), readFile(t, filepath.Join(wheelhouse, entry.Name())), 0o644)
	}
	if !removed {
		t.Fatal("wheelhouse has no ruamel-yaml wheel")
	}
	python := peHostPython(t)
	peToolchain(t, peUV(t), python)
	stderr := peCaptureStderr(t)
	destination := filepath.Join(tmp, "pm-runtime")
	_, err := StageManagerRuntime(context.Background(), StageManagerRuntimeOptions{Python: python, Destination: destination,
		Project: peRuntimeProject(t), Wheelhouse: incomplete, Offline: true})
	var ie *InstallError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "pip exited") {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(stderr(), "ruamel-yaml") {
		t.Fatalf("stderr %q", stderr())
	}
	if existsOrLink(destination) {
		t.Fatal("failed stage left its destination")
	}
}

// Redirects the process stderr into a pipe for one test and returns a reader of what was written
func peCaptureStderr(t *testing.T) func() string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = w
	var buf bytes.Buffer
	done := make(chan struct{})
	go func() { io.Copy(&buf, r); close(done) }()
	closed := false
	finish := func() {
		if !closed {
			closed = true
			os.Stderr = original
			w.Close()
			<-done
		}
	}
	t.Cleanup(finish)
	return func() string { finish(); return buf.String() }
}

func TestRuntimeStageManagerRuntimeNeedsStagedToolsAndAFreshDestination(t *testing.T) {
	tmp := isolateHome(t)
	ctx := context.Background()
	peSwap(t, &uvToolchain, func(context.Context, bool, bool) (string, string, bool, error) { return "", "", false, nil })
	_, err := stageManagerRuntime(ctx, StageManagerRuntimeOptions{Python: peHostPython(t), Destination: filepath.Join(tmp, "runtime")})
	if err == nil || !strings.Contains(err.Error(), "stage pinned tools before building the manager runtime") || existsOrLink(filepath.Join(tmp, "runtime")) {
		t.Fatalf("got %v", err)
	}
	existing := filepath.Join(tmp, "existing")
	os.MkdirAll(existing, 0o777)
	if _, err := stageManagerRuntime(ctx, StageManagerRuntimeOptions{Python: peHostPython(t), Destination: existing}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing destination: %v", err)
	}
	if _, err := StageRuntime(ctx, "uv", "python", filepath.Join(tmp, "x"), StageRuntimeOptions{}); err == nil {
		t.Fatal("a runtime stage without its project ran")
	}
}
