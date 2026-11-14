package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Marker the test binary prints when pip has installed it as the icebath launcher
const launcherMarker = "icebath launcher from the wheel"

// Answers as the installed launcher when re-executed under the icebath name, otherwise runs the suite
func TestMain(m *testing.M) {
	if strings.HasPrefix(filepath.Base(os.Args[0]), "icebath") {
		fmt.Println(launcherMarker, strings.Join(os.Args[1:], " "))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Release inputs the recorded listing was made from, every binary holding its own name
func pwInputs(t *testing.T, root string, host []byte) {
	t.Helper()
	for _, p := range platforms {
		exe := ""
		if p.goos == "windows" {
			exe = ".exe"
		}
		data := []byte(fmt.Sprintf("binary %s %s\n", p.goos, p.goarch))
		if host != nil && p.goos == runtime.GOOS && p.goarch == runtime.GOARCH {
			data = host
		}
		path := filepath.Join(root, "bin", fmt.Sprintf("icebath-%s-%s%s", p.goos, p.goarch, exe))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "LICENSE"), []byte("MIT License\n\nCopyright (c) 2026 Nicholas W. Heyer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMissingBinaryAndUsageExitLikeWheelsPy(t *testing.T) {
	root := t.TempDir()
	pwInputs(t, root, nil)
	t.Chdir(root)
	if err := os.Remove(filepath.Join("bin", "icebath-darwin-amd64")); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"1.2.3", "bin", "out", "LICENSE"}, &stdout, &stderr); code != 1 || stderr.String() != "wheels: missing bin/icebath-darwin-amd64\n" {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
	if lines := strings.Split(strings.TrimSpace(stdout.String()), "\n"); len(lines) != 2 {
		t.Fatalf("wheels before the missing binary %q", lines)
	}
	stderr.Reset()
	if code := run([]string{"1.2.3"}, &stdout, &stderr); code != 1 || stderr.String() != "usage: pypiwheels <version> <binaries dir> <out dir> <license file>\n" {
		t.Fatalf("usage exit %d stderr %q", code, stderr.String())
	}
}

func TestPipInstallPutsAWorkingLauncherOnPath(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	pwInputs(t, root, host)
	t.Chdir(root)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"1.2.3", "bin", "out", "LICENSE"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d stderr %q", code, stderr.String())
	}
	var built string
	for _, p := range platforms {
		if p.goos == runtime.GOOS && p.goarch == runtime.GOARCH {
			built = filepath.Join(root, "out", fmt.Sprintf("%s-1.2.3-py3-none-%s.whl", name, p.tag))
		}
	}
	venv := filepath.Join(root, "venv")
	env := append(os.Environ(), "PIP_DISABLE_PIP_VERSION_CHECK=1", "PIP_NO_INPUT=1", "PIP_CONFIG_FILE="+os.DevNull)
	for _, argv := range [][]string{{python, "-m", "venv", venv}, {filepath.Join(venv, "bin", "python"), "-m", "pip", "install", "--no-index", "--no-deps", built}} {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", argv, err, out)
		}
	}
	t.Setenv("PATH", filepath.Join(venv, "bin"))
	launcher, err := exec.LookPath("icebath")
	if err != nil || launcher != filepath.Join(venv, "bin", "icebath") {
		t.Fatalf("icebath on PATH %q %v", launcher, err)
	}
	cmd := exec.Command(launcher, "--version")
	cmd.Env = append(env, "PATH="+filepath.Join(venv, "bin"))
	out, err := cmd.Output()
	if err != nil || string(out) != launcherMarker+" --version\n" {
		t.Fatalf("launcher %q %v", out, err)
	}
	if info, err := os.Stat(launcher); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("launcher mode %v %v", info, err)
	}
	if stamp, err := os.ReadFile(filepath.Join(venv, "bin", ".install_method")); err != nil || string(stamp) != "pip\n" {
		t.Fatalf("install method stamp %q %v", stamp, err)
	}
}
