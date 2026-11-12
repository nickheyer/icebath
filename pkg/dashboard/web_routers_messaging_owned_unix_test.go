//go:build unix

package dashboard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestWhatsappPairingTerminateReleasesTheProcessItStopped(t *testing.T) {
	var released [][]int
	saved := tools.ReleaseProcessGroupsFn
	t.Cleanup(func() { tools.ReleaseProcessGroupsFn = saved })
	tools.ReleaseProcessGroupsFn = func(pgids []int) { released = append(released, slices.Clone(pgids)) }
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pairing := watchedPairingProc(cmd, nil)
	t.Cleanup(func() {
		if pairing.Poll() == nil {
			pairing.Kill()
		}
		<-pairing.done
	})
	terminateWhatsappPairing(pairing)
	if pairing.Poll() == nil {
		t.Fatal("terminate left the process running")
	}
	if len(released) != 1 || !slices.Equal(released[0], []int{cmd.Process.Pid}) {
		t.Fatalf("released %v want [[%d]]", released, cmd.Process.Pid)
	}
}

// Identity receipt of one process of an owned tree, pid and start time
type treeIdentity struct {
	pid     int
	started any
}

// Whether the process of an identity still runs and is no zombie
func (i treeIdentity) alive() bool {
	if !proc.Exists(i.pid) {
		return false
	}
	started := gateway.GetProcessStartTime(i.pid)
	return started != nil && pyrepr.Equal(started, i.started) && !proc.Zombie(i.pid)
}

// Child and grandchild process of an owned tree, recording itself then idling
func TestHelperOwnedTreeProcess(t *testing.T) {
	out, role := os.Getenv("ICEBATH_TEST_OWNED_TREE_OUT"), os.Getenv("ICEBATH_TEST_OWNED_TREE_ROLE")
	if out == "" {
		t.Skip("runs only as a process of the owned tree the kill -9 case starts")
	}
	receipt, _ := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "created", gateway.GetProcessStartTime(os.Getpid())))
	tmp := filepath.Join(out, role+".tmp")
	if err := os.WriteFile(tmp, []byte(receipt), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(out, role+".json")); err != nil {
		t.Fatal(err)
	}
	if role == "child" {
		grandchild := exec.Command(os.Args[0], "-test.run=^TestHelperOwnedTreeProcess$")
		grandchild.Env = append(os.Environ(), "ICEBATH_TEST_OWNED_TREE_ROLE=grandchild")
		if err := grandchild.Start(); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(120 * time.Second)
}

// Owner process whose pairing bridge, run by sh in place of node, execs the owned tree child
func TestHelperWhatsappPairingOwner(t *testing.T) {
	out := os.Getenv("ICEBATH_TEST_PAIRING_OWNER_OUT")
	if out == "" {
		t.Skip("runs only as the owner process the kill -9 case starts")
	}
	bridge := filepath.Join(out, "bridge")
	script := "exec " + strings.Join([]string{shellQuote(os.Args[0]), "-test.run=^TestHelperOwnedTreeProcess$"}, " ") + "\n"
	writeTestFile(t, filepath.Join(bridge, "bridge.js"), script)
	whatsappBridgeDir = func(context.Context) string { return bridge }
	whatsappFindNode = func(context.Context, string) (string, error) { return "/bin/sh", nil }
	whatsappEnsureBridgeDependencies = func(context.Context, string) error { return nil }
	os.Setenv("ICEBATH_TEST_OWNED_TREE_OUT", out)
	os.Setenv("ICEBATH_TEST_OWNED_TREE_ROLE", "child")
	if _, err := spawnWhatsappPairingBridge(context.Background(), filepath.Join(out, "session"), "bot"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Second)
}

// Single quoted shell word
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Starts one disposable owner and waits for its child and grandchild receipts
func startPairingOwner(t *testing.T, tmp, name string) (*exec.Cmd, map[string]treeIdentity) {
	t.Helper()
	out := filepath.Join(tmp, name)
	h := filepath.Join(tmp, "home-"+name)
	for _, dir := range []string{out, h} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	owner := exec.Command(os.Args[0], "-test.run=^TestHelperWhatsappPairingOwner$")
	owner.Env = append(os.Environ(), "ICEBATH_TEST_PAIRING_OWNER_OUT="+out, "ICEBATH_HOME="+filepath.Join(h, ".icebath"), "HOME="+h)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	ids := map[string]treeIdentity{}
	for _, role := range []string{"child", "grandchild"} {
		path := filepath.Join(out, role+".json")
		deadline := time.Now().Add(30 * time.Second)
		for {
			if raw, err := os.ReadFile(path); err == nil {
				receipt, err := pyjson.LoadsDict(string(raw))
				if err != nil {
					t.Fatal(err)
				}
				pid, _ := pyint.Int64(receipt.GetOr("pid", nil))
				ids[role] = treeIdentity{pid: int(pid), started: receipt.GetOr("created", nil)}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("receipt %s never written", path)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return owner, ids
}

func TestASigkilledOwnerTakesItsChildTreeAndSparesALiveOwners(t *testing.T) {
	t.Run("whatsapp-pairing", func(t *testing.T) {
		tmp := t.TempDir()
		owners := map[string]*exec.Cmd{}
		trees := map[string]map[string]treeIdentity{}
		t.Cleanup(func() {
			for _, tree := range trees {
				for _, identity := range tree {
					if identity.alive() {
						syscall.Kill(identity.pid, syscall.SIGKILL)
					}
				}
			}
			for _, owner := range owners {
				owner.Process.Kill()
				owner.Wait()
			}
		})
		for _, name := range []string{"dead", "live"} {
			owners[name], trees[name] = startPairingOwner(t, tmp, name)
		}
		for name, tree := range trees {
			for role, identity := range tree {
				if !identity.alive() {
					t.Fatalf("%s %s not alive", name, role)
				}
			}
		}
		owners["dead"].Process.Kill()
		owners["dead"].Wait()
		deadline := time.Now().Add(15 * time.Second)
		for trees["dead"]["child"].alive() || trees["dead"]["grandchild"].alive() {
			if time.Now().After(deadline) {
				t.Fatal("the whatsapp-pairing tree survived its kill -9'd owner")
			}
			time.Sleep(50 * time.Millisecond)
		}
		for role, identity := range trees["live"] {
			if !identity.alive() {
				t.Fatalf("a live owner's whatsapp-pairing %s was killed", role)
			}
		}
	})
}
