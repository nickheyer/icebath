//go:build linux && !icebath_core && !icebath_noextras

package selfupdate

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Copies the build inputs of the module into dst, test files left out
func liveCopyTree(t *testing.T, root, dst string) {
	t.Helper()
	for _, name := range []string{"go.mod", "go.sum", "cmd", "pkg", "docker", "third_party", "setup-icebath.sh", ".env.example", "cli-config.yaml.example", ".gitignore"} {
		src := filepath.Join(root, name)
		err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			if strings.HasSuffix(path, "_test.go") || !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Environment of the install's processes, a scratch user and icebath home with the go settings of this run
func liveEnv(t *testing.T, user, home string) []string {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		goBin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + strings.Join([]string{filepath.Join(user, ".local", "bin"), filepath.Dir(goBin), filepath.Dir(gitBin), "/usr/bin", "/bin"}, ":"),
		"HOME=" + user, "ICEBATH_HOME=" + home, "SHELL=/bin/bash", "TERM=dumb", "LANG=C.UTF-8", "TMPDIR=" + os.TempDir(),
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
	}
	out, err := exec.Command(goBin, "env", "GOCACHE", "GOMODCACHE", "GOPATH").Output()
	if err != nil {
		t.Fatal(err)
	}
	values := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
		env = append(env, key+"="+values[i])
	}
	for _, key := range []string{"GOMEMLIMIT", "GOFLAGS", "GOTMPDIR", "GOPROXY"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return append(env, "GOTOOLCHAIN=local")
}

// Runs one command of the install and returns its combined output, failing the test on a non zero exit
func liveRun(t *testing.T, dir string, env []string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir, cmd.Env = dir, env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v\n%s", argv, err, out.String())
	}
	return out.String()
}

// Gateway state the home's runtime status file records
func liveGatewayState(home string) map[string]any {
	raw, err := os.ReadFile(filepath.Join(home, "gateway_state.json"))
	if err != nil {
		return nil
	}
	var state map[string]any
	if json.Unmarshal(raw, &state) != nil {
		return nil
	}
	return state
}

// Waits until the home's gateway reports running at sha, returning its pid
func liveWaitGateway(t *testing.T, home, sha string, notPID int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		state := liveGatewayState(home)
		if pid, ok := state["pid"].(float64); ok && state["gateway_state"] == "running" && state["code_sha"] == sha && int(pid) != notPID &&
			syscall.Kill(int(pid), 0) == nil {
			return int(pid)
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no gateway running at %s, state %v", sha, liveGatewayState(home))
	return 0
}

// Commit the binary at path was built from, read from its embedded build info
func liveBuiltRevision(t *testing.T, env []string, binary string) string {
	t.Helper()
	var info struct{ Commit string }
	if err := json.Unmarshal([]byte(liveRun(t, filepath.Dir(binary), env, binary, "install-self", "--release-info")), &info); err != nil {
		t.Fatal(err)
	}
	return info.Commit
}

// A source install's own update lands on origin's next commit, rebuilds, and restarts its gateway on the new binary
func TestLiveUpdateLeavesASourceCheckoutAndItsGatewayAlone(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	origin, install, user, home := filepath.Join(tmp, "o"), filepath.Join(tmp, "i"), filepath.Join(tmp, "u"), filepath.Join(tmp, "h")
	for _, dir := range []string{origin, user, home} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	liveCopyTree(t, root, origin)
	srGit(t, origin, "init", "-q", "-b", "main")
	srGit(t, origin, "add", "-A")
	srGit(t, origin, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "one")
	first := srGit(t, origin, "rev-parse", "HEAD")
	srGit(t, tmp, "clone", "-q", origin, install)
	if err := os.WriteFile(filepath.Join(origin, "cmd", "icebath", "live_fixture_revision.go"), []byte("package main\n\nconst liveFixtureRevision = \"two\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srGit(t, origin, "add", "-A")
	srGit(t, origin, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-qm", "two")
	second := srGit(t, origin, "rev-parse", "HEAD")

	env := liveEnv(t, user, home)
	if err := os.WriteFile(filepath.Join(user, ".bashrc"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Log(liveRun(t, install, env, "bash", "./setup-icebath.sh"))
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("telemetry:\n  shared_metrics:\n    enabled: false\n    offer_version: 2\n    send: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(install, "icebath")
	if got := liveBuiltRevision(t, env, binary); got != first {
		t.Fatalf("setup built %q, want %s", got, first)
	}
	if version := liveRun(t, install, env, binary, "--version"); !strings.Contains(version, "Install method: git") {
		t.Fatalf("version %s", version)
	}
	gatewayLog, err := os.Create(filepath.Join(tmp, "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer gatewayLog.Close()
	gateway := exec.Command(binary, "gateway", "run")
	gateway.Dir, gateway.Env = install, env
	gateway.Stdout, gateway.Stderr = gatewayLog, gatewayLog
	gateway.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := gateway.Start(); err != nil {
		t.Fatal(err)
	}
	go gateway.Wait()
	t.Cleanup(func() {
		stop := exec.Command(binary, "gateway", "stop")
		stop.Dir, stop.Env = install, env
		stop.Run()
		if state := liveGatewayState(home); state != nil {
			if pid, ok := state["pid"].(float64); ok {
				syscall.Kill(int(pid), syscall.SIGKILL)
			}
		}
		gateway.Process.Kill()
	})
	before := liveWaitGateway(t, home, first, 0)
	for _, args := range [][]string{{"update", "--check"}, {"update", "--yes"}} {
		cmd := exec.Command(binary, args...)
		cmd.Dir, cmd.Env = install, env
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "is a source checkout") || !strings.Contains(string(out), "./setup-icebath.sh") {
			t.Fatalf("%v on a source checkout: %v\n%s", args, err, out)
		}
	}
	if head := srGit(t, install, "rev-parse", "HEAD"); head != first || head == second {
		t.Fatalf("checkout moved to %s, want it left at %s", head, first)
	}
	if got := liveBuiltRevision(t, env, binary); got != first {
		t.Fatalf("checkout binary rebuilt from %q, want it left at %s", got, first)
	}
	if syscall.Kill(before, 0) != nil {
		t.Fatalf("the gateway %d of the checkout stopped", before)
	}
}
