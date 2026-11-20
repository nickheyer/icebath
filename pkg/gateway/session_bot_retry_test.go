package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

const botRetryKey = "cccccccccccccccccccccccccccccccc"

// Bot Chat owner over a fresh home whose finite turns answer pong unless a queued error fails them
type botRetryFixture struct {
	runner *botRetryRunner
	ctx    context.Context
	a      *SessionAuthority
	actor  Principal
	chat   string
	home   string
	mu     sync.Mutex
	errors []string
}

func newBotRetryFixture(t *testing.T) *botRetryFixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("platform_toolsets:\n  cli: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(ctx, filepath.Join(dir, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := NewSessionStore(ctx, filepath.Join(dir, "sessions"), DefaultGatewayConfig(ctx), nil)
	store.PinDB(db)
	runner := &botRetryRunner{recoverTestRunner: &recoverTestRunner{store: store, adapters: pydict.New[Platform, PlatformAdapter](),
		profiles: pydict.New[string, *pydict.Dict[Platform, PlatformAdapter]]()}}
	a, err := InitializeSessionAuthority(ctx, runner, "default", "fixture", db, false)
	if err != nil {
		t.Fatal(err)
	}
	runner.launch = a
	installPrefillLoaders(t)
	home, err := pypath.Resolve(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := &botRetryFixture{runner: runner, ctx: ctx, a: a, home: home, actor: Principal{Subject: "uid:1000", ProfileID: "default",
		Capabilities: map[string]bool{"session:create": true, "session:read": true, "session:submit": true}, TransportID: "native"}}
	chat, err := createLocalSession(ctx, a, f.actor, map[string]any{"request_id": "bot", "source": "gui", "cwd": dir,
		"model": "fixture", "toolsets": []any{}}, localCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := titleNewSession(ctx, a, chat, "Bot Chat"); err != nil {
		t.Fatal(err)
	}
	f.chat = chat.SessionID
	replaceExecution(t, func(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (any, error) {
		f.mu.Lock()
		var failure string
		if len(f.errors) > 0 {
			failure, f.errors = f.errors[0], f.errors[1:]
		}
		f.mu.Unlock()
		if failure == "" {
			return "pong", nil
		}
		a.mu.Lock()
		a.PendingResults[rowString(row, "admission_id")] = pyjson.ObjectOf("result", pyjson.ObjectOf(
			"final_response", "", "messages", []any{}, "failed", true, "error", failure), "usage", pyjson.NewObject())
		a.mu.Unlock()
		return "", nil
	})
	return f
}

// Fixture runner whose drain flag flips while drains read it
type botRetryRunner struct {
	*recoverTestRunner
	shuttingDown atomic.Bool
}

func (r *botRetryRunner) Draining() bool { return r.shuttingDown.Load() }

// Queues the errors the next finite turns fail with
func (f *botRetryFixture) fail(errors ...string) {
	f.mu.Lock()
	f.errors = append([]string(nil), errors...)
	f.mu.Unlock()
}

// Every admission of the Bot Chat in FIFO order
func (f *botRetryFixture) admissions(t *testing.T) []*pyjson.Object {
	t.Helper()
	rows, err := f.a.DB.ListSessionAdmissions(f.ctx, f.chat, false)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Request ids of the admissions from index from on
func (f *botRetryFixture) requestIDs(t *testing.T, from int) []string {
	var ids []string
	for _, row := range f.admissions(t)[from:] {
		ids = append(ids, rowString(row, "request_id"))
	}
	return ids
}

// Delivers one message as the fixture's owner principal
func (f *botRetryFixture) deliver(t *testing.T, key, message string) map[string]any {
	t.Helper()
	answer, err := BotDeliver(f.ctx, f.a, f.actor, map[string]any{"id": key, "profile": "default", "message": message})
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

// Receipt of a delivery projected from its stored file
func (f *botRetryFixture) result(t *testing.T, key string) map[string]any {
	t.Helper()
	stored, err := ReadDeliveryResult(f.ctx, f.home, key)
	if err != nil {
		t.Fatal(err)
	}
	result, err := botResult(f.ctx, f.a, stored)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// Receipt once count admissions all settled and the receipt task observed it
func (f *botRetryFixture) settled(t *testing.T, key string, count int) map[string]any {
	t.Helper()
	for range 200 {
		time.Sleep(20 * time.Millisecond)
		rows := f.admissions(t)
		done := len(rows) >= count
		for _, row := range rows {
			done = done && rowString(row, "status") == AdmissionTerminal
		}
		if done {
			time.Sleep(50 * time.Millisecond)
			return f.result(t, key)
		}
	}
	t.Fatalf("admissions never settled: %v", f.admissions(t))
	return nil
}

// Waits until no receipt task, mailbox operation or drain runs
func (f *botRetryFixture) settleQuietly(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		f.a.mu.Lock()
		quiet := len(f.a.mutationTasks) == 0
		for task := range f.a.botReceiptTasks {
			quiet = quiet && task.finished()
		}
		for _, live := range f.a.Sessions {
			if live.drain != nil {
				select {
				case <-live.drain:
				default:
					quiet = false
				}
			}
		}
		f.a.mu.Unlock()
		if quiet {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("receipt tasks and drains never went idle")
}

func TestTransientFailureRetriesExactlyOnceAndTheReceiptFollowsTheRetry(t *testing.T) {
	f := newBotRetryFixture(t)
	f.fail("Error code: 429 - rate limit exceeded")
	first := f.deliver(t, botRetryKey, "ping")
	receipt := f.settled(t, botRetryKey, 2)
	if receipt["status"] != "settled" || receipt["reply"] != "pong" {
		t.Fatalf("receipt %v", receipt)
	}
	if !pyjson.Equal(receipt["admission_id"], first["admission_id"]) || !pyTruthy(receipt["retry_admission_id"]) {
		t.Fatalf("receipt %v first %v", receipt, first)
	}
	if ids := f.requestIDs(t, 0); strings.Join(ids, ",") != "bot:"+botRetryKey+",bot:"+botRetryKey+":retry" {
		t.Fatalf("ids %v", ids)
	}
	again := f.deliver(t, botRetryKey, "ping")
	if again["status"] != "settled" || again["reply"] != "pong" {
		t.Fatalf("again %v", again)
	}
	if err := recoverBotDeliveries(f.ctx, f.a); err != nil {
		t.Fatal(err)
	}
	f.settleQuietly(t)
	if n := len(f.admissions(t)); n != 2 {
		t.Fatalf("admissions %d", n)
	}
}

func TestRetryThatFailsAgainAndNonTransientFailuresAreNeverReplayed(t *testing.T) {
	f := newBotRetryFixture(t)
	f.fail("HTTP 503 server error", "HTTP 503 server error")
	f.deliver(t, botRetryKey, "ping")
	receipt := f.settled(t, botRetryKey, 2)
	if receipt["status"] != "failed" || !pyTruthy(receipt["retry_admission_id"]) {
		t.Fatalf("receipt %v", receipt)
	}
	if receipt["reason"] != "provider_server_error" || !strings.Contains(routeStr(receipt["error"]), "503") {
		t.Fatalf("receipt %v", receipt)
	}
	f.deliver(t, botRetryKey, "ping")
	if err := recoverBotDeliveries(f.ctx, f.a); err != nil {
		t.Fatal(err)
	}
	f.settleQuietly(t)
	if n := len(f.admissions(t)); n != 2 {
		t.Fatalf("admissions %d", n)
	}
	other := strings.Repeat("d", 32)
	f.fail("Error code: 401 - invalid api key")
	f.deliver(t, other, "auth")
	for range 100 {
		time.Sleep(20 * time.Millisecond)
		rows := f.admissions(t)
		done := len(rows) == 3
		for _, row := range rows {
			done = done && rowString(row, "status") == AdmissionTerminal
		}
		if done {
			break
		}
	}
	if err := recoverBotDeliveries(f.ctx, f.a); err != nil {
		t.Fatal(err)
	}
	f.settleQuietly(t)
	if ids := f.requestIDs(t, 2); len(ids) != 1 || ids[0] != "bot:"+other {
		t.Fatalf("ids %v", ids)
	}
	auth := f.result(t, other)
	if auth["status"] != "failed" || auth["reason"] != "provider_auth_or_access" {
		t.Fatalf("auth %v", auth)
	}
}

func TestContextOverflowRetriesOnceUnlessTheFailedDmIsStillAnOpenTail(t *testing.T) {
	f := newBotRetryFixture(t)
	f.fail("This model's maximum context length is 200000 tokens")
	f.deliver(t, botRetryKey, "ping")
	receipt := f.settled(t, botRetryKey, 2)
	if receipt["status"] != "settled" || receipt["reply"] != "pong" {
		t.Fatalf("receipt %v", receipt)
	}
	other := strings.Repeat("e", 32)
	f.fail("This model's maximum context length is 200000 tokens")
	if _, err := f.a.DB.AppendMessage(f.ctx, f.chat, map[string]any{"role": "user", "content": "overflowed dm"}, state.AppendGuards{}); err != nil {
		t.Fatal(err)
	}
	f.deliver(t, other, "overflowed dm")
	var record map[string]any
	for range 100 {
		time.Sleep(20 * time.Millisecond)
		var err error
		if record, err = ReadDeliveryResult(f.ctx, f.home, other); err != nil {
			t.Fatal(err)
		}
		if retry, _ := record["retry"].(map[string]any); retry != nil && pyTruthy(retry["refused"]) {
			break
		}
	}
	retry, _ := record["retry"].(map[string]any)
	if retry == nil || retry["refused"] != "open_user_tail" || len(f.admissions(t)) != 3 {
		t.Fatalf("record %v", record)
	}
	if record["status"] != "failed" || record["reason"] != "context_overflow" {
		t.Fatalf("record %v", record)
	}
}

func TestReceiptFileStaysPendingWhileADrainingOwnerLeavesTheRetryToItsSuccessor(t *testing.T) {
	f := newBotRetryFixture(t)
	started, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	execute := executeFiniteAdmission
	replaceExecution(t, func(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object) (any, error) {
		if !strings.HasSuffix(rowString(row, "request_id"), ":retry") {
			once.Do(func() { close(started) })
			<-gate
		}
		return execute(ctx, a, ref, row)
	})
	f.fail("Error code: 429 - rate limit exceeded")
	f.deliver(t, botRetryKey, "ping")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the original never started")
	}
	f.runner.shuttingDown.Store(true)
	close(gate)
	for range 200 {
		time.Sleep(20 * time.Millisecond)
		if rowString(f.admissions(t)[0], "status") == AdmissionTerminal {
			break
		}
	}
	f.settleQuietly(t)
	short := 200 * time.Millisecond
	interim, err := AwaitDelivery(f.ctx, f.home, botRetryKey, &short, nil)
	if err != nil {
		t.Fatal(err)
	}
	if interim["status"] != "claimed" || len(f.admissions(t)) != 1 {
		t.Fatalf("interim %v", interim)
	}
	type awaited struct {
		receipt map[string]any
		err     error
	}
	waiting := make(chan awaited, 1)
	go func() {
		long := 10 * time.Second
		receipt, err := AwaitDelivery(f.ctx, f.home, botRetryKey, &long, nil)
		waiting <- awaited{receipt, err}
	}()
	f.runner.shuttingDown.Store(false)
	if err := recoverBotDeliveries(f.ctx, f.a); err != nil {
		t.Fatal(err)
	}
	var got awaited
	select {
	case got = <-waiting:
	case <-time.After(10 * time.Second):
		t.Fatal("the wait never got its answer")
	}
	if got.err != nil || got.receipt["status"] != "settled" || got.receipt["reply"] != "pong" {
		t.Fatalf("receipt %v %v", got.receipt, got.err)
	}
	if ids := f.requestIDs(t, 0); strings.Join(ids, ",") != "bot:"+botRetryKey+",bot:"+botRetryKey+":retry" {
		t.Fatalf("ids %v", ids)
	}
}
