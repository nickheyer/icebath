//go:build !windows

package codeexec

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Pid plus start time so a recycled pid never reads as the same process
type procIdentity struct {
	pid     int
	started string
}

// Process state letters and start time as ps reports them, empty when the pid is gone
func psFacts(pid int) (string, string) {
	out, err := exec.Command("ps", "-o", "stat=", "-o", "lstart=", "-p", itoa(pid)).Output()
	if err != nil {
		return "", ""
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", ""
	}
	return fields[0], strings.Join(fields[1:], " ")
}

func identityOf(t *testing.T, pid int) procIdentity {
	t.Helper()
	_, started := psFacts(pid)
	if started == "" {
		t.Fatalf("pid %d is gone before its identity was read", pid)
	}
	return procIdentity{pid: pid, started: started}
}

// Zombie aware liveness, a killed orphan lingering unreaped counts as gone
func (p procIdentity) alive() bool {
	stat, started := psFacts(p.pid)
	return started == p.started && !strings.HasPrefix(stat, "Z")
}

func (p procIdentity) kill() {
	if p.alive() {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
}

func waitUntil(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Kernel and cell child identities from a receipt the cell wrote
func readReceipt(t *testing.T, path string) map[string]procIdentity {
	t.Helper()
	waitUntil(t, func() bool { _, err := os.Stat(path); return err == nil }, 30*time.Second, "kernel receipt "+filepath.Base(path))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var pids map[string]int
	if err := json.Unmarshal(data, &pids); err != nil {
		t.Fatal(err)
	}
	out := map[string]procIdentity{}
	for name, pid := range pids {
		out[name] = identityOf(t, pid)
	}
	return out
}

func TestOwnerSigkillReapsTheKernelGroupNotALiveOwners(t *testing.T) {
	env := testEnv(t)
	rt := hostRuntime(t)
	root := t.TempDir()
	log, err := os.Create(filepath.Join(root, "owners.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	owners := map[string]*exec.Cmd{}
	ids := map[string]map[string]procIdentity{}
	defer func() {
		for _, group := range ids {
			for _, identity := range group {
				identity.kill()
			}
		}
		for _, owner := range owners {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	}()
	for _, role := range []string{"control", "victim"} {
		owner := exec.Command(os.Args[0], "-test.run=^$")
		owner.Env = append(os.Environ(), "CODEEXEC_KERNEL_OWNER="+role, "CODEEXEC_OWNER_ROOT="+root, "HOME="+root,
			"ICEBATH_HOME="+env.Home, "CODEEXEC_KERNEL_PYTHON="+rt.Python, "CODEEXEC_KERNEL_PREFIX="+rt.Prefix)
		owner.Stdout, owner.Stderr = log, log
		if err := owner.Start(); err != nil {
			t.Fatal(err)
		}
		owners[role] = owner
	}
	for role := range owners {
		ids[role] = readReceipt(t, filepath.Join(root, role+".json"))
		pgid, err := syscall.Getpgid(ids[role]["cell_child"].pid)
		if err != nil || pgid != ids[role]["kernel"].pid {
			t.Fatalf("the cell's subprocess left the kernel's group (%s): pgid %d err %v", role, pgid, err)
		}
	}
	_ = owners["victim"].Process.Kill()
	_ = owners["victim"].Wait()
	waitUntil(t, func() bool {
		return !ids["victim"]["kernel"].alive() && !ids["victim"]["cell_child"].alive()
	}, 15*time.Second, "the dead owner's kernel and its cell child to exit")
	for name, identity := range ids["control"] {
		if !identity.alive() {
			t.Fatalf("a live owner's kernel group was killed (%s)", name)
		}
	}
}

type groupCall struct {
	verb  string
	pgids []int
}

// Real kernel with its spawn registration captured, running a cell that spawned childArgv
func kernelWithCellChild(t *testing.T, childArgv []string) (*sessionKernel, procIdentity, func() []groupCall) {
	t.Helper()
	env := testEnv(t)
	rt := hostRuntime(t)
	var mu sync.Mutex
	var calls []groupCall
	own, release, register := tools.OwnProcessGroupsFn, tools.ReleaseProcessGroupsFn, tools.RegisterChildFn
	tools.OwnProcessGroupsFn = func(pgids []int) {
		mu.Lock()
		calls = append(calls, groupCall{"register", slices.Clone(pgids)})
		mu.Unlock()
	}
	tools.ReleaseProcessGroupsFn = func(pgids []int) {
		mu.Lock()
		calls = append(calls, groupCall{"unregister", slices.Clone(pgids)})
		mu.Unlock()
	}
	tools.RegisterChildFn = func(int, string, string) bool { return true }
	t.Cleanup(func() {
		tools.OwnProcessGroupsFn, tools.ReleaseProcessGroupsFn, tools.RegisterChildFn = own, release, register
	})
	k := newSessionKernel(kernelKey{owner: "graceful"})
	if err := k.spawn(t.Context(), env, rt.Python, "", nil, 1); err != nil {
		t.Fatal(err)
	}
	receipt := filepath.Join(t.TempDir(), "cell_child.json")
	cell, _ := json.Marshal(map[string]string{"id": "x", "code": groupCellCode(childArgv, receipt)})
	if _, err := k.stdin.Write(append(cell, '\n')); err != nil {
		t.Fatal(err)
	}
	child := readReceipt(t, receipt)["cell_child"]
	return k, child, func() []groupCall {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(calls)
	}
}

func killIdentity(t *testing.T, p procIdentity) {
	p.kill()
	waitUntil(t, func() bool { return !p.alive() }, 10*time.Second, "cell child cleanup")
}

func TestTeardownReleasesTheKernelsGroup(t *testing.T) {
	k, child, calls := kernelWithCellChild(t, []string{"@python", "-c", "import time; time.sleep(60)"})
	defer func() {
		k.teardown()
		killIdentity(t, child)
	}()
	pid := k.proc.Load().p.Pid
	if got := calls(); len(got) != 1 || got[0].verb != "register" || !slices.Equal(got[0].pgids, []int{pid}) {
		t.Fatalf("spawn registration %v", got)
	}
	k.teardown()
	if child.alive() {
		t.Fatal("teardown left the cell's subprocess running")
	}
	got := calls()
	if last := got[len(got)-1]; last.verb != "unregister" || !slices.Equal(last.pgids, []int{pid}) {
		t.Fatalf("a kernel group this process tore down itself stayed registered: %v", got)
	}
}

func TestTeardownKeepsCoveringACellChildThatSurvivedIt(t *testing.T) {
	realKill := killProcessGroup
	killProcessGroup = func(pid int, kill func() error, exited <-chan struct{}, _ bool) { realKill(pid, kill, exited, false) }
	t.Cleanup(func() { killProcessGroup = realKill })
	ready := filepath.Join(t.TempDir(), "trapped")
	k, child, calls := kernelWithCellChild(t, []string{"sh", "-c", "trap '' TERM; : > '" + ready + "'; exec sleep 60"})
	defer killIdentity(t, child)
	waitUntil(t, func() bool { _, err := os.Stat(ready); return err == nil }, 30*time.Second, "the cell child to ignore SIGTERM")
	pid := k.proc.Load().p.Pid
	k.teardown()
	if !child.alive() {
		t.Fatal("precondition: the TERM-ignoring cell child survived teardown")
	}
	if got := calls(); len(got) != 1 || got[0].verb != "register" || !slices.Equal(got[0].pgids, []int{pid}) {
		t.Fatalf("the group was released while a cell child was still in it: %v", got)
	}
}
