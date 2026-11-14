package app

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Restores the desktop launcher and entry installer steps after the test
func swapGUISteps(t *testing.T) {
	t.Helper()
	savedGUI, savedLDE := gui, lde
	var locks []guiBuildLock
	create := gui.newBuildLock
	gui.newBuildLock = func(projectRoot string) guiBuildLock {
		lock := create(projectRoot)
		locks = append(locks, lock)
		return lock
	}
	t.Cleanup(func() {
		for _, lock := range locks {
			lock.Release(context.Background())
		}
		gui, lde = savedGUI, savedLDE
	})
}

// Writes a file with its parents created
func writeTestFile(t *testing.T, path, content string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// Desktop launcher steps that never leave the test, every run and spawn recorded
type guiRecorder struct {
	runs   [][]string
	popens []guiRun
	specs  []guiRun
}

// Records each run and answers it with exit status zero
func (r *guiRecorder) run(spec guiRun) (*guiResult, error) {
	r.runs = append(r.runs, append([]string{}, spec.Argv...))
	r.specs = append(r.specs, spec)
	return &guiResult{}, nil
}

// Records each spawn and answers it with a fixed pid
func (r *guiRecorder) popen(spec guiRun) (int, error) {
	r.popens = append(r.popens, spec)
	return 4242, nil
}

// Desktop launch namespace with the parser defaults and overrides
func guiNS(overrides map[string]any) map[string]any {
	ns := map[string]any{"skip_build": false, "build_only": false, "force_build": false, "source": false, "fake_boot": false,
		"ignore_existing": false, "icebath_root": nil, "cwd": nil, "setup_tcc_identity": false, "identity": nil,
		"close_preview": false, "local": false, "install_apparmor_profile": false}
	for k, v := range overrides {
		ns[k] = v
	}
	return ns
}

// Runs the launcher and returns its exit code and printed output
func runCmdGUI(t *testing.T, ns map[string]any, explicitProfile string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code, err := CmdGUI(context.Background(), ns, explicitProfile, &out)
	if err != nil {
		t.Fatalf("CmdGUI error %v, output %s", err, out.String())
	}
	return code, out.String()
}

// Fails unless two string slices are equal
func assertArgv(t *testing.T, got, want []string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) || len(got) != len(want) {
		t.Fatalf("argv %q, want %q", got, want)
	}
}

// Runs as a child process for the desktop launcher tests when the helper variable names an action
func TestDesktopHelperProcess(t *testing.T) {
	switch os.Getenv("ICEBATH_DESKTOP_TEST_HELPER") {
	case "marker":
		_ = os.WriteFile(os.Getenv("ICEBATH_DESKTOP_TEST_MARKER"), []byte("yes"), 0o644)
		os.Exit(0)
	case "flood":
		os.Stdout.Write(bytes.Repeat([]byte("x"), 131072))
		os.Stderr.WriteString("diagnostic\n")
		os.Exit(7)
	case "ordinary":
		os.Stdout.WriteString("ordinary-output\n")
		os.Stderr.WriteString("fatal-diagnostic\n")
		os.Exit(7)
	case "payload":
		data, _ := os.ReadFile(os.Getenv("ICEBATH_DESKTOP_TEST_MARKER"))
		out := os.Stdout
		if os.Getenv("ICEBATH_DESKTOP_TEST_STREAM") == "stderr" {
			out = os.Stderr
		}
		for i := 0; i < len(data); i += 997 {
			out.Write(data[i:min(i+997, len(data))])
		}
		os.Exit(0)
	}
}

// Argv and environment that run this test binary as a helper child doing action
func helperChild(action string, extra ...string) ([]string, map[string]string) {
	env := map[string]string{}
	for _, kv := range os.Environ() {
		if i := bytes.IndexByte([]byte(kv), '='); i > 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	env["ICEBATH_DESKTOP_TEST_HELPER"] = action
	for i := 0; i+1 < len(extra); i += 2 {
		env[extra[i]] = extra[i+1]
	}
	return []string{os.Args[0], "-test.run=^TestDesktopHelperProcess$"}, env
}
