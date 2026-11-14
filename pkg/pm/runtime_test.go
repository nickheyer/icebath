package pm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Copies a directory tree, following symlinks the way shutil copytree does by default
func sttCopyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		st, err := os.Stat(path)
		if err != nil {
			return err
		}
		if st.IsDir() {
			if d.Type()&fs.ModeSymlink != 0 {
				sttCopyTree(t, path, target)
				return nil
			}
			return os.MkdirAll(target, 0o777)
		}
		return sttCopyFile(path, target, st.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Copies one file's bytes and permission bits
func sttCopyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func TestSealedWorkerCommandUsesOnlyItsRecordedSite(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("platforms macos, windows")
	}
	isolateHome(t)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := strings.Split(strings.TrimSpace(sttOutput(t, tmp, sttHostPython(t), "-c",
		"import sys; print(sys._base_executable); print(sys.base_prefix)")), "\n")
	baseExecutable, basePrefix := strings.TrimSpace(host[0]), strings.TrimSpace(host[1])
	repo := sttMkdir(t, filepath.Join(tmp, "payload", "icebath"))
	writeFile(t, filepath.Join(filepath.Dir(repo), "manifest.json"), []byte(`{"repo":"icebath"}`), 0o666)
	rt := filepath.Join(filepath.Dir(repo), "pm-runtime")
	site := sttMkdir(t, filepath.Join(rt, "site"))
	base := filepath.Join(filepath.Dir(repo), "python")
	var python string
	if runtime.GOOS == "windows" {
		sttCopyTree(t, basePrefix, base)
		python = filepath.Join(base, "python.exe")
	} else {
		python = filepath.Join(sttMkdir(t, filepath.Join(base, "bin")), "python")
		resolved, err := filepath.EvalSymlinks(baseExecutable)
		if err != nil {
			t.Fatal(err)
		}
		if err := sttCopyFile(resolved, python, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(basePrefix, "lib"), filepath.Join(base, "lib")); err != nil {
			t.Fatal(err)
		}
	}
	rel, err := filepath.Rel(rt, python)
	if err != nil {
		t.Fatal(err)
	}
	marker, _ := json.Marshal(map[string]string{"python": rel, "sitePackages": "site"})
	writeFile(t, filepath.Join(rt, "pm-runtime.json"), marker, 0o666)
	script := filepath.Join(repo, "probe.py")
	writeFile(t, script, []byte("import sys,json; print(json.dumps(sys.path))"), 0o666)
	sttRepoRoot(t, repo)

	command, err := RuntimeCommand(context.Background(), script, nil, RuntimeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var entries []string
	if err := json.Unmarshal([]byte(sttOutput(t, tmp, command...)), &entries); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(entries, site) {
		t.Fatalf("%s not in %q", site, entries)
	}
	for _, entry := range entries {
		if name := filepath.Base(entry); name == "site-packages" || name == "dist-packages" {
			t.Fatalf("ambient site %s in %q", entry, entries)
		}
	}
}

func TestOneStoreThroughSymlinkedHomesKeepsOnePMRuntime(t *testing.T) {
	tmp := isolateHome(t)
	project := sttMkdir(t, filepath.Join(tmp, "project"))
	writeFile(t, filepath.Join(project, "pyproject.toml"), []byte("[project]\nname = 'fixture'\n"), 0o666)
	writeFile(t, filepath.Join(project, "uv.lock"), []byte("version = 1\n"), 0o666)
	real := filepath.Join(tmp, "volume", "tools")
	sttTouch(t, filepath.Join(sttMkdir(t, filepath.Join(real, "python", "bin")), "python3.14"))
	homeDir, task := filepath.Join(tmp, "home"), filepath.Join(tmp, "task")
	if err := os.Symlink("python3.14", filepath.Join(real, "python", "bin", "python3")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, linked := range []string{homeDir, task} {
		sttMkdir(t, linked)
		if err := os.Symlink(real, filepath.Join(linked, "tools")); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	preFixIdentity := func(python string) string {
		digest := sha256.New()
		for _, name := range []string{"pyproject.toml", "uv.lock"} {
			digest.Write(readFile(t, filepath.Join(project, name)))
			digest.Write([]byte{0})
		}
		digest.Write([]byte(python))
		return hex.EncodeToString(digest.Sum(nil))
	}

	var staged []string
	realStage, realValidate, realHold := stageRuntime, validateRuntime, holdForChildren
	t.Cleanup(func() { stageRuntime, validateRuntime, holdForChildren = realStage, realValidate, realHold })
	stageRuntime = func(ctx context.Context, uv, python, environment string, opts StageRuntimeOptions) (string, error) {
		staged = append(staged, python)
		if err := os.MkdirAll(environment, 0o777); err != nil {
			return "", err
		}
		return VenvPython(environment), nil
	}
	validateRuntime = func(ctx context.Context, python, imports string, env map[string]string) string { return "" }
	holdForChildren = func(environment string) error { return nil }
	root := filepath.Join(tmp, "pm-runtime")
	old := sttMkdir(t, filepath.Join(root, "generations", "old"))
	writeFile(t, filepath.Join(old, "pm-runtime.json"), []byte("{}"), 0o666)
	selection, _ := json.Marshal(map[string]string{
		"inputs":     preFixIdentity(filepath.Join(homeDir, "tools", "python", "bin", "python3")),
		"generation": "generations/old",
	})
	writeFile(t, filepath.Join(root, "selected.json"), selection, 0o666)

	launch := func(tools string, bootstrap bool) string {
		t.Helper()
		python, err := PrepareRuntime(context.Background(), filepath.Join(tools, "uv"),
			filepath.Join(tools, "python", "bin", "python3"), root,
			RuntimeOptions{Project: project, Imports: PMRuntimeImports, NoBootstrap: !bootstrap})
		if err != nil {
			t.Fatal(err)
		}
		return python
	}
	if got := launch(filepath.Join(homeDir, "tools"), false); got != VenvPython(old) {
		t.Fatalf("pre canonical record not reused, got %s", got)
	}
	current := launch(filepath.Join(task, "tools"), true)
	for _, tools := range []string{filepath.Join(homeDir, "tools"), filepath.Join(task, "tools"), real, filepath.Join(homeDir, "tools")} {
		if got := launch(tools, false); got != current {
			t.Fatalf("%s selected %s, want %s", tools, got, current)
		}
	}
	if len(staged) != 1 {
		t.Fatalf("staged %d times", len(staged))
	}
}
