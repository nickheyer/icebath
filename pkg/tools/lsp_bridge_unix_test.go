//go:build !windows

package tools

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/lsp"
)

// Whether a pid names a live process that is not a zombie
func lspPidAlive(pid int) bool {
	return osProcessProber{}.Running(pid) && !(osProcessProber{}).Zombie(pid)
}

func TestLSPOwnerHelperProcess(t *testing.T) {
	pidFile := os.Getenv("ICEBATH_LSP_OWNER_CHILD_PID")
	if pidFile == "" {
		t.Skip("runs only as the owner process the supervisor tests start")
	}
	installTestEnvHooks(t)
	ws, mock, h := os.Getenv("ICEBATH_LSP_OWNER_WS"), os.Getenv("ICEBATH_LSP_OWNER_MOCK"), os.Getenv("ICEBATH_HOME")
	idx := slices.IndexFunc(lsp.Servers, func(s *lsp.ServerDef) bool { return s.ServerID == "pyright" })
	lsp.Servers[idx] = &lsp.ServerDef{ServerID: "pyright", Extensions: lsp.Servers[idx].Extensions,
		ResolveRoot: func(_, ws string) string { return ws },
		BuildSpawn: func(root string, _ lsp.ServerContext) (*lsp.SpawnSpec, error) {
			return &lsp.SpawnSpec{Command: []string{mock}, WorkspaceRoot: root, Cwd: root,
				Env: map[string]string{"MOCK_LSP_SCRIPT": "slow_tree", "MOCK_LSP_CHILD_PID": pidFile}}, nil
		}}
	env := &Env{Home: h, Secrets: config.NewSecrets(h), Values: &sync.Map{}, Config: config.Tree{"lsp": map[string]any{
		"wait_timeout": 1.0, "install_strategy": "manual", "idle_timeout": 0}}}
	svc, err := LSPServiceFor(env)
	if err != nil || svc == nil {
		t.Fatal(svc, err)
	}
	svc.SnapshotBaseline(filepath.Join(ws, "x.py"), map[string]bool{ws: true})
	os.Stdout.WriteString("ready\n")
	time.Sleep(300 * time.Second)
}

// Owner process running an LSP client on the slow_tree mock, with the server leader and its SIGTERM ignoring child
func startLSPOwner(t *testing.T) (*exec.Cmd, int, int) {
	tmp := t.TempDir()
	ws := lspGitRepo(t, filepath.Join(tmp, "ws"))
	if err := os.WriteFile(filepath.Join(ws, "x.py"), []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(tmp, "child.pid")
	owner := exec.Command(os.Args[0], "-test.run=^TestLSPOwnerHelperProcess$", "-test.v")
	owner.Dir = ws
	owner.Env = append(os.Environ(), "ICEBATH_LSP_OWNER_CHILD_PID="+pidFile, "ICEBATH_LSP_OWNER_WS="+ws,
		"ICEBATH_LSP_OWNER_MOCK="+lspMockServer(t), "ICEBATH_HOME="+filepath.Join(tmp, "home"), "HOME="+filepath.Join(tmp, "home"))
	out, err := owner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		owner.Process.Kill()
		owner.Wait()
	})
	lines := bufio.NewScanner(out)
	ready := make(chan bool, 1)
	go func() {
		for lines.Scan() {
			if strings.TrimSpace(lines.Text()) == "ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("the owner exited before starting the server")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the owner never started the server")
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	leader, ok := osProcessProber{}.PPID(child)
	if !ok || child == 0 {
		t.Fatalf("child %d leader %d", child, leader)
	}
	t.Cleanup(func() {
		osProcessProber{}.Kill(child)
		osProcessProber{}.Kill(leader)
	})
	return owner, leader, child
}

func TestLSPSigkilledOwnerTakesItsWholeServerTreeWithIt(t *testing.T) {
	owner, leader, child := startLSPOwner(t)
	owner.Process.Kill()
	owner.Wait()
	deadline := time.Now().Add(15 * time.Second)
	for lspPidAlive(leader) || lspPidAlive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("leader alive %v child alive %v after the owner was kill -9'd", lspPidAlive(leader), lspPidAlive(child))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLSPLiveOwnerKeepsItsServerTree(t *testing.T) {
	owner, leader, child := startLSPOwner(t)
	supervised := false
	for _, pid := range (osProcessProber{}).Children(owner.Process.Pid, false) {
		if argv, err := (osProcessProber{}).Cmdline(pid); err == nil && IsOwnerDeathSupervisor(argv) {
			supervised = true
		}
	}
	if !supervised {
		t.Fatal("the owner should have started its owner death supervisor")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !lspPidAlive(leader) || !lspPidAlive(child) {
			t.Fatal("the supervisor killed a live owner's server tree")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
