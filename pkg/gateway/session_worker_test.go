//go:build !windows

package gateway

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Live direct child of the test process that reads stdin until it closes, wrapped as a worker handle
func stdinReaderChild(t *testing.T) *workerProcess {
	t.Helper()
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cat)
	cmd.Stdin = r
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	p := &workerProcess{cmd: cmd, Pid: cmd.Process.Pid, stdin: w, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		w.Close()
		p.wait(5)
	})
	return p
}

// Kernel birth of a live pid
func birthOf(t *testing.T, pid int) float64 {
	t.Helper()
	birth, err := proc.WorkerBirth(pid)
	if err != nil {
		t.Fatal(err)
	}
	return birth
}

// Worker scope params of a pid with the register kind
func registerScope(t *testing.T, pid int) *pyjson.Object {
	return pyjson.ObjectOf("profile_id", "p", "session_id", "owned", "execution_id", "compute-"+itoa(pid), "generation", int64(0),
		"pid", int64(pid), "birth", birthOf(t, pid), "secret", strings.Repeat("w", 32), "kind", "compute")
}

// Decimal text of an int
func itoa(n int) string { return pyStrOf(int64(n)) }

func TestWorkerRegisterRequiresTheOwnersLaunchedWorker(t *testing.T) {
	ctx, db := newTestStateDB(t)
	if err := db.CreateSession(state.NewSession{ID: "owned", Source: "telegram"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, "p", "owner", db, epoch)
	human := "human"
	a.Sessions["owned"] = NewLiveSession(&SessionSource{Platform: PlatformTelegram, UserID: &human}, "route")
	ref := SessionRef{ProfileID: "p", SessionID: "owned"}
	child := stdinReaderChild(t)
	interactive := &AuthorityConnection{authority: a, actor: Principal{Subject: "human", ProfileID: "p",
		Capabilities: map[string]bool{"session:create": true, "session:read": true, "session:submit": true, "session:control": true}, TransportID: "t"}}
	worker := &AuthorityConnection{authority: a, actor: Principal{Subject: "human", ProfileID: "p",
		Capabilities: map[string]bool{"worker:adopt": true}, TransportID: "t"}}
	if _, err := workerRequest(ctx, interactive, ref, registerScope(t, child.Pid), "register"); !isRuntimeRefusal(err, "permission_denied") {
		t.Fatalf("interactive register: %v", err)
	}
	if n := workerExecutionCount(t, ctx, db); n != 0 {
		t.Fatalf("worker rows after refused register: %d", n)
	}
	registered, err := workerRequest(ctx, worker, ref, registerScope(t, child.Pid), "register")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := registered.Get("status"); status != "registered" {
		t.Fatalf("status %v", status)
	}
}

// Rows of the worker execution ledger
func workerExecutionCount(t *testing.T, ctx context.Context, db *state.SessionDB) int64 {
	t.Helper()
	var n int64
	err := db.ReadCtx(ctx, func(conn state.SQLiteConn) error {
		row, err := state.ConnRow(ctx, conn, "SELECT COUNT(*) AS n FROM worker_executions")
		if err == nil {
			n = row["n"].(int64)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Principal of a started automation admission, a route rather than a ticket subject
const automationPrincipal = "automation:agent:main:local:dm:owned"

// Started automation admission and the owner's live direct child over a fresh store
func reservedAutomation(t *testing.T) (context.Context, *SessionAuthority, *pyjson.Object, *workerProcess) {
	t.Helper()
	ctx, db := newTestStateDB(t)
	process := stdinReaderChild(t)
	if err := db.CreateSession(state.NewSession{ID: "owned", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	a := NewSessionAuthority(&authorityRunnerDouble{}, t.TempDir(), "owner", db, epoch)
	admitted, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: automationPrincipal, SessionID: "owned",
		RequestID: "bot:dm", Payload: pyjson.NewObject()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.ClaimSessionInput(ctx, state.ClaimInput{Epoch: epoch, SessionID: "owned"}); err != nil {
		t.Fatal(err)
	}
	return ctx, a, admitted, process
}

// Hello frame of a direct child as the worker introduces itself
func childHello(t *testing.T, process *workerProcess) *pyjson.Object {
	return pyjson.ObjectOf("type", "hello", "pid", int64(process.Pid), "birth", birthOf(t, process.Pid), "ancestors", []any{int64(os.Getpid())})
}

// Worker identity of a reservation scope, its epoch dropped
func scopeIdentity(scope *pyjson.Object) *pyjson.Object {
	identity := scope.Copy()
	identity.Delete("epoch")
	return identity
}

func TestWorkerAdoptsAnAutomationAdmissionAsTheOwnerUser(t *testing.T) {
	ctx, a, admitted, process := reservedAutomation(t)
	scope, err := reserveAdmissionWorker(ctx, a, rowString(admitted, "admission_id"), process, automationPrincipal,
		childHello(t, process), ownerWorkerSubject())
	if err != nil {
		t.Fatal(err)
	}
	connection := &AuthorityConnection{authority: a, actor: Principal{Subject: ownerWorkerSubject(), ProfileID: a.ProfileID,
		Capabilities: map[string]bool{"worker:adopt": true}}}
	receipt, err := workerRequest(ctx, connection, SessionRef{}, scopeIdentity(scope), "adopt")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := receipt.Get("execution_id"); got != scope.Vals["execution_id"] {
		t.Fatalf("execution_id %v want %v", got, scope.Vals["execution_id"])
	}
}

func TestTheClaimStillRefusesAnyOtherSubject(t *testing.T) {
	ctx, a, admitted, process := reservedAutomation(t)
	scope, err := reserveAdmissionWorker(ctx, a, rowString(admitted, "admission_id"), process, automationPrincipal,
		childHello(t, process), ownerWorkerSubject())
	if err != nil {
		t.Fatal(err)
	}
	for _, subject := range []string{automationPrincipal, "uid:999999"} {
		connection := &AuthorityConnection{authority: a, actor: Principal{Subject: subject, ProfileID: a.ProfileID,
			Capabilities: map[string]bool{"worker:adopt": true}}}
		if _, err := workerRequest(ctx, connection, SessionRef{}, scopeIdentity(scope), "adopt"); err == nil || !strings.Contains(err.Error(), "permission_denied") {
			t.Fatalf("subject %s: %v", subject, err)
		}
	}
}
