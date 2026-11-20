package pm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Marks the test binary a minted launcher runs as the install binary of the fixture tree
const launchSelectionChild = "ICEBATH_PM_LAUNCH_SELECTION_CHILD"

var probeValue = regexp.MustCompile(`^VALUE = '([^'\\]*)'\n$`)

// VALUE of the first source module of that name on the import path, the way the fixture entry imports it
func importProbeValue(module string) (string, error) {
	for _, entry := range importPathEntries() {
		data, err := os.ReadFile(filepath.Join(entry, module+".py"))
		if err != nil {
			continue
		}
		m := probeValue.FindSubmatch(data)
		if m == nil {
			return "", fmt.Errorf("%s holds no VALUE literal", filepath.Join(entry, module+".py"))
		}
		return string(m[1]), nil
	}
	return "", fmt.Errorf("ModuleNotFoundError: No module named %s", pyrepr.StrRepr(module))
}

// Fixture install binary running the launcher steps, the bootstrap activation, then printing what its entry imports
func launchSelectionChildMain() int {
	args, _ := LauncherStart(RepoRoot(), os.Args)
	os.Args = args
	if err := ActivateDependencies(context.Background(), RepoRoot()); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", errorClass(err), err)
		return 1
	}
	PinLauncherHome()
	var values []string
	for _, module := range []string{"selection_probe", "editable_probe"} {
		value, err := importProbeValue(module)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		values = append(values, value)
	}
	fmt.Println(strings.Join(values, " "))
	return 0
}

// Copies the running test binary to path as an executable install binary
func installTestBinary(t *testing.T, path string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
}

// Process environment without the named variables, names folded on Windows as os.environ holds them
func environWithout(names ...string) []string {
	var out []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		drop := false
		for _, candidate := range names {
			if name == candidate || (runtime.GOOS == "windows" && strings.EqualFold(name, candidate)) {
				drop = true
			}
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return out
}

// test_launcher_runtime_selection.py test_minted_launcher_reads_current_selection_and_editable_members
func TestMintedLauncherReadsCurrentSelectionAndEditableMembers(t *testing.T) {
	ctx := context.Background()
	tmp := resolvePath(t.TempDir())
	root := filepath.Join(tmp, "repo")
	binary := filepath.Join(root, installBinaryName())
	installTestBinary(t, binary)
	base, err := SitePackages(filepath.Join(root, "venv"))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(base, "selection_probe.py"), []byte("VALUE = 'base'\n"), 0o666)
	t.Setenv(home.EnvHome, filepath.Join(tmp, "home"))
	state := InstallStateDir(ctx, root)
	out := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(out, 0o777); err != nil {
		t.Fatal(err)
	}
	launcher := mintLauncher("icebath", root, out, binary, "")
	if launcher == "" {
		t.Fatal("no launcher was minted")
	}

	selected := filepath.Join(state, "environments", "selected", "venv")
	site, err := SitePackages(selected)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(site, 0o777); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(selected, "pyvenv.cfg"), []byte("home = fixture\n"), 0o666)
	writeFile(t, filepath.Join(site, "selection_probe.py"), []byte("VALUE = 'selected'\n"), 0o666)
	editable := filepath.Join(tmp, "editable")
	writeFile(t, filepath.Join(editable, "editable_probe.py"), []byte("VALUE = 'editable'\n"), 0o666)
	writeFile(t, filepath.Join(site, "member.pth"), []byte(editable+"\n"), 0o666)
	facts, err := pyjson.Dumps(map[string]any{"packages": map[string]any{"venv": map[string]any{"environment": selected}}})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(state, "facts.json"), []byte(facts), 0o666)

	run, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(run, launcher)
	cmd.Dir = tmp
	cmd.Env = append(environWithout("PYTHONPATH", "PYTHONHOME"), launchSelectionChild+"=1")
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatalf("launcher exited %d\n%s%s", exit.ExitCode(), stdout.String(), stderr.String())
	}
	if got := pystr.Strip(stdout.String()); got != "selected editable" {
		t.Fatalf("launched program loaded %q", got)
	}
}
