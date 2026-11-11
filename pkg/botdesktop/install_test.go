//go:build !windows

package botdesktop

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/tools"
)

func swapInstall[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

type installLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *installLines) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *installLines) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}

func installChildPid(t *testing.T, lines []string) int {
	t.Helper()
	for _, line := range lines {
		if strings.HasPrefix(line, "child ") {
			pid, err := strconv.Atoi(strings.Fields(line)[1])
			if err != nil {
				t.Fatal(err)
			}
			return pid
		}
	}
	t.Fatalf("no child line in %q", lines)
	return 0
}

func onlyBinaries(names ...string) func(string) string {
	return func(name string) string {
		if slices.Contains(names, name) {
			return "/usr/bin/" + name
		}
		return ""
	}
}

// Linux host with a sudo apt-get install line, a password needing sudo and a fresh profile home
func isolatedInstallHost(t *testing.T) *tools.Env {
	t.Helper()
	env := &tools.Env{Home: t.TempDir()}
	swapInstall(t, &sudoNopasswd, func() bool { return false })
	swapInstall(t, &hostPlatform, "linux")
	swapInstall(t, &geteuid, func() int { return 1000 })
	swapInstall(t, &whichBinary, onlyBinaries("apt-get", "sudo"))
	t.Cleanup(func() {
		installLock.Lock()
		clear(installRunning)
		for key, fh := range installSlotFiles {
			fh.Close()
			delete(installSlotFiles, key)
		}
		installLock.Unlock()
	})
	return env
}

func failingExec(t *testing.T) func(string, ...string) *exec.Cmd {
	return func(string, ...string) *exec.Cmd {
		t.Error("package manager spawned")
		return exec.Command("true")
	}
}

func fakeSudoExec(script ...string) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		if name != "sudo" {
			return exec.Command(name, args...)
		}
		return exec.Command(script[0], script[1:]...)
	}
}

func TestEmptyPasswordCancelsWithoutSpawning(t *testing.T) {
	env := isolatedInstallHost(t)
	swapInstall(t, &installExec, failingExec(t))
	var lines []string
	code, err := InstallPackages(env, func() (string, error) { return "", nil }, func(l string) { lines = append(lines, l) })
	if err != nil || code != -1 {
		t.Fatalf("code %d err %v", code, err)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "cancelled") }) {
		t.Fatalf("lines %q", lines)
	}
}

func TestSecondInstallForSameProfileIsRefused(t *testing.T) {
	env := isolatedInstallHost(t)
	gate, entered := make(chan struct{}), make(chan struct{})
	swapInstall(t, &installRun, func(string, func() (string, error), func(string), float64) (int, error) {
		close(entered)
		select {
		case <-gate:
		case <-time.After(5 * time.Second):
		}
		return 0, nil
	})
	pw := func() (string, error) { return "pw", nil }
	done := make(chan struct{})
	go func() {
		defer close(done)
		InstallPackages(env, pw, func(string) {})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never entered the run")
	}
	_, err := InstallPackages(env, pw, func(string) {})
	var busy *InstallBusy
	if !errors.As(err, &busy) {
		t.Fatalf("err %v", err)
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never finished")
	}
	if err := AssertNotRunning(env); err != nil {
		t.Fatalf("slot still held: %v", err)
	}
}

func TestClaimIsAtomicAndRefusesASecondClaim(t *testing.T) {
	env := isolatedInstallHost(t)
	key, err := Claim(env)
	if err != nil {
		t.Fatal(err)
	}
	var busy *InstallBusy
	if _, err := Claim(env); !errors.As(err, &busy) {
		t.Fatalf("second claim err %v", err)
	}
	if _, err := InstallPackages(env, func() (string, error) { return "pw", nil }, func(string) {}); !errors.As(err, &busy) {
		t.Fatalf("install err %v", err)
	}
	Release(key)
	if _, err := Claim(env); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	Release(key)
}

func TestPasswordlessSudoRunsTheInstallWithoutAskingForAPassword(t *testing.T) {
	env := isolatedInstallHost(t)
	swapInstall(t, &sudoNopasswd, func() bool { return true })
	var spawned [][]string
	swapInstall(t, &installExec, func(name string, args ...string) *exec.Cmd {
		spawned = append(spawned, append([]string{name}, args...))
		return exec.Command("printf", `Reading package lists...\nDone\n`)
	})
	var lines []string
	code, err := InstallPackages(env, func() (string, error) {
		t.Error("sudo password asked despite NOPASSWD")
		return "", nil
	}, func(l string) { lines = append(lines, l) })
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if len(spawned) == 0 || spawned[0][0] != "sudo" || slices.Contains(spawned[0], "-S") {
		t.Fatalf("spawned %q", spawned)
	}
	if !slices.Contains(lines, "Done") || !slices.Contains(lines, "Reading package lists...") {
		t.Fatalf("lines %q", lines)
	}
}

func TestRootInstallsWithoutSudoAndWithoutAsking(t *testing.T) {
	env := isolatedInstallHost(t)
	swapInstall(t, &geteuid, func() int { return 0 })
	swapInstall(t, &whichBinary, onlyBinaries("apt-get"))
	var spawned [][]string
	swapInstall(t, &installExec, func(name string, args ...string) *exec.Cmd {
		spawned = append(spawned, append([]string{name}, args...))
		return exec.Command("true")
	})
	code, err := InstallPackages(env, func() (string, error) {
		t.Error("root was asked for a sudo password")
		return "", nil
	}, func(string) {})
	if err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if len(spawned) == 0 || spawned[0][0] != "apt-get" {
		t.Fatalf("spawned %q", spawned)
	}
}

func TestNoSudoBinaryReturnsTheHostCommandInsteadOfAPasswordCard(t *testing.T) {
	env := isolatedInstallHost(t)
	swapInstall(t, &whichBinary, onlyBinaries("apt-get"))
	swapInstall(t, &installExec, failingExec(t))
	var lines []string
	code, err := InstallPackages(env, func() (string, error) {
		t.Error("password card raised without sudo")
		return "", nil
	}, func(l string) { lines = append(lines, l) })
	if err != nil || code != NoSudo {
		t.Fatalf("code %d err %v", code, err)
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "apt-get install") }) {
		t.Fatalf("lines %q", lines)
	}
	if slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, "as root: sudo ") }) {
		t.Fatalf("the host command kept its sudo prefix: %q", lines)
	}
}

func TestTimeoutFinishesTheGroupWhenOnlyTheLeaderDiesOnTerm(t *testing.T) {
	env := isolatedInstallHost(t)
	swapInstall(t, &sudoNopasswd, func() bool { return true })
	swapInstall(t, &termGraceSeconds, 0.3)
	swapInstall(t, &installExec, fakeSudoExec("sh", "-c", `sh -c 'trap "" TERM; echo child $$; sleep 30' & wait`))
	var lines []string
	if _, err := InstallPackagesWith(env, func() (string, error) { return "", nil }, func(l string) { lines = append(lines, l) }, 0.5, false); err != nil {
		t.Fatal(err)
	}
	child := installChildPid(t, lines)
	defer exec.Command("kill", "-9", strconv.Itoa(child)).Run()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && proc.Exists(child) {
		time.Sleep(50 * time.Millisecond)
	}
	if proc.Exists(child) {
		t.Fatal("the TERM-ignoring descendant survived the timeout cleanup")
	}
}

func TestInstallSlotIsHeldAcrossProcesses(t *testing.T) {
	env := isolatedInstallHost(t)
	key, err := Claim(env)
	if err != nil {
		t.Fatal(err)
	}
	probe := exec.Command(os.Args[0])
	probe.Env = append(os.Environ(), flockProbeEnv+"="+filepath.Join(StateDir(env), "install.lock"))
	out, perr := probe.Output()
	Release(key)
	if perr != nil || strings.TrimSpace(string(out)) != "held" {
		t.Fatalf("probe %q %v", out, perr)
	}
	key, err = Claim(env)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	Release(key)
}
