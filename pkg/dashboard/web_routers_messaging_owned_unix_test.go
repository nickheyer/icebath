//go:build unix

package dashboard

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Identity receipt of the process a pairing runs in, pid and start time
type pairingIdentity struct {
	pid     int
	started any
}

// Whether the process of an identity still runs and is no zombie
func (i pairingIdentity) alive() bool {
	if !proc.Exists(i.pid) {
		return false
	}
	started := gateway.GetProcessStartTime(i.pid)
	return started != nil && pyrepr.Equal(started, i.started) && !proc.Zombie(i.pid)
}

// Dashboard owner process whose onboarding pairing transport records the process it runs in, then idles
func TestHelperWhatsappPairingOwner(t *testing.T) {
	out := os.Getenv("ICEBATH_TEST_PAIRING_OWNER_OUT")
	if out == "" {
		t.Skip("runs only as the owner process the kill -9 case starts")
	}
	whatsappRunPairing = func(ctx context.Context, session, mode string, env map[string]string, w io.Writer, pairJSON bool) int {
		receipt, err := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "created", gateway.GetProcessStartTime(os.Getpid()), "session", session))
		tmp := filepath.Join(out, "pairing.tmp")
		if err == nil && os.WriteFile(tmp, []byte(receipt), 0o644) == nil {
			os.Rename(tmp, filepath.Join(out, "pairing.json"))
		}
		<-ctx.Done()
		return -15
	}
	body := pyjson.ObjectOf("mode", "bot", "allowed_users", "", "profile", nil, "reset_session", false)
	if _, err := startWhatsappOnboarding(context.Background(), body, ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Second)
}

// Starts one disposable owner and waits for the receipt of its pairing, returning its session dir
func startPairingOwner(t *testing.T, tmp, name, lockEnv string) (*exec.Cmd, pairingIdentity, string) {
	t.Helper()
	out := filepath.Join(tmp, name)
	h := filepath.Join(tmp, "home-"+name)
	for _, dir := range []string{out, h} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	owner := exec.Command(os.Args[0], "-test.run=^TestHelperWhatsappPairingOwner$")
	owner.Env = append(os.Environ(), "ICEBATH_TEST_PAIRING_OWNER_OUT="+out, "ICEBATH_HOME="+filepath.Join(h, ".icebath"), "HOME="+h, lockEnv)
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(out, "pairing.json")
	deadline := time.Now().Add(30 * time.Second)
	for {
		if raw, err := os.ReadFile(path); err == nil {
			receipt, err := pyjson.LoadsDict(string(raw))
			if err != nil {
				t.Fatal(err)
			}
			pid, _ := pyint.Int64(receipt.GetOr("pid", nil))
			session, _ := receipt.GetOr("session", "").(string)
			return owner, pairingIdentity{pid: int(pid), started: receipt.GetOr("created", nil)}, session
		}
		if time.Now().After(deadline) {
			t.Fatalf("receipt %s never written", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestASigkilledOwnerTakesItsChildTreeAndSparesALiveOwners(t *testing.T) {
	t.Run("whatsapp-pairing", func(t *testing.T) {
		tmp := t.TempDir()
		lockEnv := "ICEBATH_GATEWAY_LOCK_DIR=" + filepath.Join(tmp, "locks")
		owners := map[string]*exec.Cmd{}
		pairings := map[string]pairingIdentity{}
		sessions := map[string]string{}
		t.Cleanup(func() {
			for _, owner := range owners {
				owner.Process.Kill()
				owner.Wait()
			}
		})
		for _, name := range []string{"dead", "live"} {
			owners[name], pairings[name], sessions[name] = startPairingOwner(t, tmp, name, lockEnv)
		}
		for _, name := range []string{"dead", "live"} {
			if !pairings[name].alive() {
				t.Fatalf("%s pairing not alive", name)
			}
			if pairings[name].pid != owners[name].Process.Pid {
				t.Fatalf("%s pairing runs in pid %d outside its owner %d", name, pairings[name].pid, owners[name].Process.Pid)
			}
			if gatewayCanAcquireSession(t, sessions[name], lockEnv) {
				t.Fatalf("gateway took the session of the %s owner's pairing", name)
			}
		}
		owners["dead"].Process.Signal(syscall.SIGKILL)
		owners["dead"].Wait()
		deadline := time.Now().Add(15 * time.Second)
		for pairings["dead"].alive() {
			if time.Now().After(deadline) {
				t.Fatal("the whatsapp-pairing survived its kill -9'd owner")
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !gatewayCanAcquireSession(t, sessions["dead"], lockEnv) {
			t.Fatal("the whatsapp-pairing of the kill -9'd owner kept holding its session dir")
		}
		if !pairings["live"].alive() {
			t.Fatal("a live owner's whatsapp-pairing was killed")
		}
		if gatewayCanAcquireSession(t, sessions["live"], lockEnv) {
			t.Fatal("a live owner's whatsapp-pairing lost its session")
		}
	})
}
