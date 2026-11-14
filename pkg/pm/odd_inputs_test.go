package pm

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// Fails unless err is the Python exception class with exactly that message
func oddWantPyError(t *testing.T, err error, class, message string) {
	t.Helper()
	if err == nil || errorClass(err) != class || err.Error() != message {
		t.Fatalf("error %v (%s), want %s: %s", err, errorClass(err), class, message)
	}
}

func TestLockedExtrasFollowsPythonOnOddShapes(t *testing.T) {
	for _, c := range []struct {
		id, lock, pyproject string
		want                []string
		class, message      string
	}{
		{"table package", "[package]\nname = \"demo\"\n", "[project]\nname = \"demo\"\n", nil, "AttributeError", "'str' object has no attribute 'get'"},
		{"string metadata", "[[package]]\nname = \"demo\"\nmetadata = \"x\"\n", "[project]\nname = \"demo\"\n", nil, "AttributeError", "'str' object has no attribute 'get'"},
		{"string provides", "[[package]]\nname = \"demo\"\n[package.metadata]\nprovides-extras = \"ab\"\n", "[project]\nname = \"demo\"\n", []string{"a", "b"}, "", ""},
		{"table provides", "[[package]]\nname = \"demo\"\n[package.metadata]\nprovides-extras = [{x = 1}]\n", "[project]\nname = \"demo\"\n", nil, "TypeError", "cannot use 'dict' as a set element (unhashable type: 'dict')"},
		{"string project", "[[package]]\nname = \"demo\"\n", "project = \"demo\"\n", nil, "TypeError", "string indices must be integers, not 'str'"},
		{"list name", "[[package]]\nname = [\"a\"]\n[package.metadata]\nprovides-extras = [\"e\"]\n", "[project]\nname = [\"a\"]\n", []string{"e"}, "", ""},
		{"float name equal to int", "[[package]]\nname = 1.0\n[package.metadata]\nprovides-extras = [\"e\", 3]\n", "[project]\nname = 1\n", []string{"e"}, "", ""},
		{"integer package", "package = 5\n", "[project]\nname = \"demo\"\n", nil, "TypeError", "'int' object is not iterable"},
		{"no name", "[[package]]\nname = \"demo\"\n", "[project]\nversion = \"1\"\n", []string{}, "", ""},
	} {
		t.Run(c.id, func(t *testing.T) {
			root := t.TempDir()
			peWrite(t, filepath.Join(root, "uv.lock"), c.lock)
			peWrite(t, filepath.Join(root, "pyproject.toml"), c.pyproject)
			got, err := lockedExtras(root)
			if c.class != "" {
				oddWantPyError(t, err, c.class, c.message)
				return
			}
			if err != nil || !reflect.DeepEqual(sortedKeys(got), c.want) {
				t.Fatalf("extras %v %v", got, err)
			}
		})
	}
}

func TestExtraRequirementsFollowsPythonOnOddShapes(t *testing.T) {
	for _, c := range []struct {
		id, pyproject  string
		extras, want   []string
		class, message string
	}{
		{"no project", "x = 1\n", []string{"a"}, nil, "KeyError", "'project'"},
		{"integer name", "[project]\nname = 5\n[project.optional-dependencies]\na = [\"r\"]\n", []string{"a"}, nil, "TypeError", "decoding to str: need a bytes-like object, int found"},
		{"integer name without rows", "[project]\nname = 5\n[project.optional-dependencies]\na = []\n", []string{"a"}, nil, "", ""},
		{"list table", "[project]\nname = \"d\"\noptional-dependencies = [\"a\"]\n", []string{"a"}, nil, "TypeError", "list indices must be integers or slices, not str"},
		{"string table hit", "[project]\nname = \"d\"\noptional-dependencies = \"abc\"\n", []string{"b"}, nil, "TypeError", "string indices must be integers, not 'str'"},
		{"integer table", "[project]\nname = \"d\"\noptional-dependencies = 5\n", []string{"a"}, nil, "TypeError", "argument of type 'int' is not a container or iterable"},
		{"integer requirement", "[project]\nname = \"d\"\n[project.optional-dependencies]\na = [5]\n", []string{"a"}, nil, "AttributeError", "'int' object has no attribute 'replace'"},
		{"string rows", "[project]\nname = \"d\"\n[project.optional-dependencies]\na = \"xy\"\n", []string{"a"}, []string{"x", "y"}, "", ""},
		{"universal newlines", "[project]\r\nname = \"d\"\r\n[project.optional-dependencies]\r\na = [\"\"\"x\r\ny\"\"\"]\r\n", []string{"a"}, []string{"x\ny"}, "", ""},
	} {
		t.Run(c.id, func(t *testing.T) {
			root := t.TempDir()
			peWrite(t, filepath.Join(root, "pyproject.toml"), c.pyproject)
			got, err := extraRequirements(root, c.extras)
			if c.class != "" {
				oddWantPyError(t, err, c.class, c.message)
				return
			}
			if err != nil || strings.Join(got, "\x00") != strings.Join(c.want, "\x00") || len(got) != len(c.want) {
				t.Fatalf("requirements %q %v", got, err)
			}
		})
	}
	root := t.TempDir()
	peWrite(t, filepath.Join(root, "pyproject.toml"), "[project]\nname = \"d\"\noptional-dependencies = \"abc\"\n")
	_, err := extraRequirements(root, []string{"z"})
	oddWantPyError(t, err, "ValueError", "'z' is not an extra declared in "+filepath.Join(root, "pyproject.toml"))
}

func TestWorkerErrorRaisesWhatPythonRaises(t *testing.T) {
	for _, c := range []struct {
		id             string
		payload        map[string]any
		class, message string
	}{
		{"empty remedy takes the default", map[string]any{"package": "p", "type": "InstallError", "cause": "c", "remedy": ""}, "InstallError", "p: c — retry, or run `icebath pm doctor`"},
		{"conflict", map[string]any{"package": "p", "type": "ResolutionConflict", "cause": "c", "remedy": "r"}, "ResolutionConflict", "p: c — r"},
		{"install without type", map[string]any{"package": "p", "cause": "c", "remedy": "r"}, "KeyError", "'type'"},
		{"install without remedy", map[string]any{"package": "p", "type": "InstallError", "cause": "c"}, "KeyError", "'remedy'"},
		{"key error repr", map[string]any{"type": "KeyError", "message": "x"}, "KeyError", "'x'"},
		{"unhashable type", map[string]any{"type": []any{"a"}, "message": "m"}, "TypeError", "cannot use 'list' as a dict key (unhashable type: 'list')"},
		{"missing message", map[string]any{"type": "ValueError"}, "KeyError", "'message'"},
		{"unknown class", map[string]any{"type": "Weird", "message": "m"}, "RuntimeError", "m"},
		{"paused without message", map[string]any{"type": "DownloadPaused"}, "KeyError", "'message'"},
	} {
		t.Run(c.id, func(t *testing.T) {
			oddWantPyError(t, workerError(c.payload), c.class, c.message)
		})
	}
}

// Write end of a worker conversation collecting what the client sends
type oddSink struct{ bytes.Buffer }

func (*oddSink) Close() error { return nil }

func TestWorkerExchangeRaisesOnMalformedLines(t *testing.T) {
	const id = "abc"
	progress := func(args []any) error { return nil }
	for _, c := range []struct {
		id, lines      string
		class, message string
		sent           string
	}{
		{"list line", "[1]\n", "TypeError", "list indices must be integers or slices, not str", ""},
		{"no id", "{\"type\": \"result\"}\n", "KeyError", "'id'", ""},
		{"other id", "{\"id\": \"zzz\", \"type\": \"result\"}\n", "InstallError", "pm: worker returned a different request id — retry, or run `icebath pm doctor`", ""},
		{"no type", "{\"id\": \"abc\"}\n", "KeyError", "'type'", ""},
		{"no callback", "{\"id\": \"abc\", \"type\": \"callback\"}\n", "KeyError", "'callback'", ""},
		{"no call", "{\"id\": \"abc\", \"type\": \"callback\", \"callback\": \"progress\", \"args\": []}\n", "KeyError", "'call'", ""},
		{"null args", "{\"id\": \"abc\", \"type\": \"callback\", \"callback\": \"progress\", \"args\": null, \"call\": 1}\n", "InstallError",
			"pm: worker exited without a result — check the worker diagnostics on stderr", "TypeError: progress() argument after * must be an iterable, not NoneType"},
		{"unknown callback", "{\"id\": \"abc\", \"type\": \"callback\", \"callback\": 5, \"call\": 1}\n", "InstallError",
			"pm: worker exited without a result — check the worker diagnostics on stderr", "KeyError: 5"},
	} {
		t.Run(c.id, func(t *testing.T) {
			sink := &oddSink{}
			session := &workerSession{id: id, stdin: sink, stdout: strings.NewReader(c.lines), stopped: make(chan struct{})}
			_, err := session.exchange(context.Background(), map[string]any{"operation": "ensure"}, map[string]func([]any) error{"progress": progress}, nil)
			oddWantPyError(t, err, c.class, c.message)
			if c.sent != "" && !strings.Contains(sink.String(), c.sent) {
				t.Fatalf("sent %q, want %q", sink.String(), c.sent)
			}
		})
	}
}

func TestPayloadPathsFollowPathlib(t *testing.T) {
	isolateHome(t)
	ctx := context.Background()
	tmp := t.TempDir()
	payload := filepath.Join(tmp, "payload")
	repo := sttMkdir(t, filepath.Join(payload, "icebath-agent"))
	manifest, _ := json.Marshal(map[string]any{"repo": "icebath-agent", "venv": filepath.Join(tmp, "elsewhere")})
	writeFile(t, filepath.Join(payload, "manifest.json"), manifest, 0o666)
	_, err := PayloadVenv(repo)
	oddWantPyError(t, err, "RuntimeError", "payload environment escapes its root")
	writeFile(t, filepath.Join(payload, "manifest.json"), []byte("{\"repo\": \"icebath-agent\", \"venv\": \"venv\"}\xff"), 0o666)
	if venv, err := PayloadVenv(repo); err != nil || venv != "" {
		t.Fatalf("an undecodable manifest is no payload, got %q %v", venv, err)
	}
	writeFile(t, filepath.Join(payload, "manifest.json"), []byte("{\"repo\": \"icebath-agent\", \"venv\": \"venv\", \"x\": NaN}"), 0o666)
	if venv, err := PayloadVenv(repo); err != nil || venv != resolvePath(filepath.Join(payload, "venv")) {
		t.Fatalf("a NaN valued manifest still reads, got %q %v", venv, err)
	}
	checkout := sttMkdir(t, filepath.Join(tmp, "checkout"))
	writeFile(t, filepath.Join(checkout, "install-stamp.json"), []byte("{\"runtimeDir\": 5}"), 0o666)
	_, err = EnvironmentStoreRoot(ctx, checkout, false)
	oddWantPyError(t, err, "TypeError", "argument should be a str or an os.PathLike object where __fspath__ returns a str, not 'int'")
	if runtime.GOOS != "windows" {
		writeFile(t, filepath.Join(checkout, "install-stamp.json"), []byte("{\"runtimeDir\": \"/a\\u0000b\"}"), 0o666)
		_, err = EnvironmentStoreRoot(ctx, checkout, false)
		oddWantPyError(t, err, "ValueError", "lstat: embedded null character in path")
	}
}

func TestComposeEnvironKeepsPythonCompositionOrder(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("ICEBATH_ODD_EXISTING", "old")
	env := composeEnviron([]EnvDiff{{"ZETA": "1", "PATH": []any{"/dep/bin"}}, {"ALPHA": "2", "ICEBATH_ODD_EXISTING": "new", "PATH": []any{"/top/bin"}}})
	added := []string{}
	for _, key := range env.keys {
		if key == "ZETA" || key == "ALPHA" {
			added = append(added, key)
		}
	}
	if !reflect.DeepEqual(added, []string{"ZETA", "ALPHA"}) {
		t.Fatalf("new keys in order %v", added)
	}
	if env.values["ICEBATH_ODD_EXISTING"] != "new" || env.find("ICEBATH_ODD_EXISTING") > env.find("ZETA") {
		t.Fatalf("existing key %q kept position %d", env.values["ICEBATH_ODD_EXISTING"], env.find("ICEBATH_ODD_EXISTING"))
	}
	sep := string(filepath.ListSeparator)
	if env.values["PATH"] != Native("/top/bin")+sep+Native("/dep/bin")+sep+"/usr/bin" {
		t.Fatalf("PATH %q", env.values["PATH"])
	}
}

func TestSamePathComparesLikePathEquality(t *testing.T) {
	if runtime.GOOS == "windows" {
		if !samePath(`C:\Users\X\tools`, `c:/users/x/tools`) {
			t.Fatal("Windows paths compare case folded with either separator")
		}
		return
	}
	if samePath("/a/b/../c", "/a/c") || !samePath("/a//b/./c", "/a/b/c") || samePath("/a/B", "/a/b") {
		t.Fatal("POSIX paths compare spelled with empty and dot parts dropped")
	}
}

func TestLauncherBoundRootPicksTheCandidateWhoseToolsRunTheLauncher(t *testing.T) {
	tmp := t.TempDir()
	root := sttMkdir(t, filepath.Join(tmp, "checkout"))
	first, second := sttMkdir(t, filepath.Join(tmp, "first")), sttMkdir(t, filepath.Join(tmp, "second"))
	binary := filepath.Join(root, installBinaryName())
	writeFile(t, binary, []byte("binary"), 0o755)
	sttMkdir(t, filepath.Join(second, "tools"))
	if mintLauncher("icebath", root, sttMkdir(t, checkoutLauncherDir(root)), binary, filepath.Join(second, "tools")) == "" {
		t.Fatal("no launcher minted")
	}
	if got := launcherBoundRoot(root, []string{first, second}); got != second {
		t.Fatalf("bound root %q, want %q", got, second)
	}
	if got := launcherBoundRoot(root, []string{first}); got != "" {
		t.Fatalf("an unbound candidate list gave %q", got)
	}
	if got := launcherBoundRoot(sttMkdir(t, filepath.Join(tmp, "bare")), []string{first, second}); got != "" {
		t.Fatalf("a checkout without launchers gave %q", got)
	}
}
