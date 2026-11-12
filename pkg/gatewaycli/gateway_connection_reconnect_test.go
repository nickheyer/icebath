package gatewaycli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

const reconnectSID = "local-abc"

type gatewayLife struct {
	number int
	owner  *fakeGateway
	events *gateway.EventQueue
	mu     sync.Mutex
	calls  []fakeCall
}

func (l *gatewayLife) RPCTimeout(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	l.mu.Lock()
	l.calls = append(l.calls, fakeCall{method, params})
	l.mu.Unlock()
	if method == "session.resume" {
		if err := l.owner.nextResumeError(); err != nil {
			return nil, err
		}
		return map[string]any{"stored_session_id": params["session_id"], "subscription_id": fmt.Sprintf("sub-%d", l.number),
			"revision": 1, "execution_generation": 7, "messages": []any{}}, nil
	}
	return map[string]any{"status": "ok", "life": l.number}, nil
}

func (l *gatewayLife) EventQueue() *gateway.EventQueue { return l.events }
func (l *gatewayLife) SendFrame(string) error          { return nil }
func (l *gatewayLife) Close() error                    { return nil }

func (l *gatewayLife) all() []fakeCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]fakeCall{}, l.calls...)
}

func (l *gatewayLife) resumes() []any {
	out := []any{}
	for _, call := range l.all() {
		if call.method == "session.resume" {
			out = append(out, call.params["session_id"])
		}
	}
	return out
}

type fakeGateway struct {
	mu           sync.Mutex
	lives        []*gatewayLife
	refusals     int
	resumeErrors []error
	dialing      chan struct{}
	hold         chan struct{}
}

func (g *fakeGateway) nextResumeError() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.resumeErrors) == 0 {
		return nil
	}
	err := g.resumeErrors[0]
	g.resumeErrors = g.resumeErrors[1:]
	return err
}

func (g *fakeGateway) dial(context.Context) (ConnectionClient, error) {
	g.mu.Lock()
	dialing, hold := g.dialing, g.hold
	g.mu.Unlock()
	if dialing != nil {
		select {
		case dialing <- struct{}{}:
		default:
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-time.After(5 * time.Second):
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.refusals > 0 {
		g.refusals--
		return nil, &gateway.GatewayClientError{Msg: "Gateway starting: deadline. Your message was not sent."}
	}
	life := &gatewayLife{number: len(g.lives) + 1, owner: g, events: gateway.NewEventQueue(0)}
	g.lives = append(g.lives, life)
	return life, nil
}

func (g *fakeGateway) life(index int) *gatewayLife {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lives[index]
}

func (g *fakeGateway) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.lives)
}

func (g *fakeGateway) hangUp(t *testing.T, conn *GatewayConnection) {
	t.Helper()
	conn.mu.Lock()
	thread := conn.thread
	conn.mu.Unlock()
	g.life(g.count() - 1).events.PutNowait(&gateway.GatewayClientError{Msg: "Gateway disconnected"})
	thread.Join(5 * time.Second)
	if thread.Alive() {
		t.Fatal("the loop goroutine outlived its gateway")
	}
}

func reconnectFixture(t *testing.T) (*fakeGateway, *GatewayConnection) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	gw := &fakeGateway{}
	conn := NewGatewayConnection(gw.dial)
	if err := conn.Start(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })
	snapshot, err := conn.Call(ctx, "session.resume", map[string]any{"session_id": reconnectSID})
	if err != nil {
		t.Fatal(err)
	}
	conn.State(reconnectSID).AdoptSnapshot(snapshot)
	return gw, conn
}

func reconnectStore(t *testing.T, conn *GatewayConnection) *GatewaySessionStore {
	t.Helper()
	db := NewGatewaySessionStore(conn)
	if _, err := db.Attach(context.Background(), reconnectSID, false); err != nil {
		t.Fatal(err)
	}
	return db
}

func wantStatus(t *testing.T, conn *GatewayConnection, life int) {
	t.Helper()
	result, err := conn.Call(context.Background(), "status", map[string]any{})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"status": "ok", "life": life}) {
		t.Fatalf("status %v %v, want life %d", result, err, life)
	}
}

func TestTheNextCallAfterARestartReconnectsAndResubscribes(t *testing.T) {
	gw, conn := reconnectFixture(t)
	if conn.Epoch() != 1 {
		t.Fatalf("epoch %d", conn.Epoch())
	}
	gw.hangUp(t, conn)
	wantStatus(t, conn, 2)
	if conn.Epoch() != 2 {
		t.Fatalf("epoch %d", conn.Epoch())
	}
	first := gw.life(1).all()[0]
	if first.method != "session.resume" || !reflect.DeepEqual(first.params, map[string]any{"session_id": reconnectSID}) {
		t.Fatalf("first call of the new life %v", first)
	}
	wantEqual(t, conn.State(reconnectSID).SubscriptionID, "sub-2")
	lost, ok := conn.Events.GetNowait()
	lostErr, isErr := lost.(error)
	if !ok || !isErr || !isClientError(lostErr) || !LostBefore(lost, conn.Epoch()) {
		t.Fatalf("queued loss %v", lost)
	}
}

func TestCloseEndsTheViewerForGood(t *testing.T) {
	gw, conn := reconnectFixture(t)
	conn.Close(context.Background())
	_, err := conn.Call(context.Background(), "status", map[string]any{})
	wantClientError(t, err, "disconnected")
	if gw.count() != 1 {
		t.Fatalf("lives %d", gw.count())
	}
}

func TestAFailedRedialIsRetriedOnTheNextCall(t *testing.T) {
	gw, conn := reconnectFixture(t)
	gw.hangUp(t, conn)
	gw.mu.Lock()
	gw.refusals = 1
	gw.mu.Unlock()
	_, err := conn.Call(context.Background(), "status", map[string]any{})
	wantClientError(t, err, "Gateway starting")
	wantStatus(t, conn, 2)
}

// Loop body that runs the connection's own and stays alive after ready until released
type lingeringRunner struct {
	conn     *GatewayConnection
	released chan struct{}
}

func (r lingeringRunner) run(ctx context.Context) {
	r.conn.run(ctx)
	select {
	case <-r.released:
	case <-time.After(5 * time.Second):
	}
}

// Loop thread whose join releases the lingering body before it waits
type releasingThread struct {
	loopThread
	release func()
}

func (t releasingThread) Join(timeout time.Duration) {
	t.release()
	t.loopThread.Join(timeout)
}

func TestAFailedRedialIsRetriedEvenWhileItsLoopThreadIsStillExiting(t *testing.T) {
	gw, conn := reconnectFixture(t)
	gw.hangUp(t, conn)
	gw.mu.Lock()
	gw.refusals = 1
	gw.mu.Unlock()
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	defer release()
	conn.mu.Lock()
	conn.runner = lingeringRunner{conn: conn, released: released}
	conn.mu.Unlock()
	_, err := conn.Call(context.Background(), "status", map[string]any{})
	wantClientError(t, err, "Gateway starting")
	conn.mu.Lock()
	old := conn.thread
	conn.thread = releasingThread{loopThread: old, release: release}
	conn.mu.Unlock()
	if !old.Alive() {
		t.Fatal("the failed dial's loop thread ended before the next call")
	}
	wantStatus(t, conn, 2)
}

func TestAFirstConnectFailureIsStillFinal(t *testing.T) {
	gw := &fakeGateway{refusals: 2}
	conn := NewGatewayConnection(gw.dial)
	for range 2 {
		_, err := conn.Call(context.Background(), "status", map[string]any{})
		wantClientError(t, err, "Gateway starting")
	}
	if gw.refusals != 1 {
		t.Fatalf("refusals left %d", gw.refusals)
	}
}

func TestEndingASessionAfterAReconnectDetachesItsCurrentSubscription(t *testing.T) {
	gw, conn := reconnectFixture(t)
	store := reconnectStore(t, conn)
	gw.hangUp(t, conn)
	wantStatus(t, conn, 2)
	if err := store.EndSession(context.Background(), reconnectSID, "new_session"); err != nil {
		t.Fatal(err)
	}
	calls := gw.life(1).all()
	last := calls[len(calls)-1]
	if last.method != "session.detach" || !reflect.DeepEqual(last.params, map[string]any{"session_id": reconnectSID,
		"subscription_id": "sub-2", "end_reason": "new_session"}) {
		t.Fatalf("last call %v", last)
	}
	gw.hangUp(t, conn)
	wantStatus(t, conn, 3)
	if resumes := gw.life(2).resumes(); len(resumes) != 0 {
		t.Fatalf("resumed %v", resumes)
	}
}

func TestADeletedSessionIsNotResumedByLaterReconnects(t *testing.T) {
	gw, conn := reconnectFixture(t)
	store := reconnectStore(t, conn)
	gw.hangUp(t, conn)
	wantStatus(t, conn, 2)
	if _, err := store.DeleteSession(context.Background(), reconnectSID); err != nil {
		t.Fatal(err)
	}
	gw.hangUp(t, conn)
	wantStatus(t, conn, 3)
	if resumes := gw.life(2).resumes(); len(resumes) != 0 {
		t.Fatalf("resumed %v", resumes)
	}
}

func TestASessionGoneDuringTheRestartIsSkippedAndNotRetried(t *testing.T) {
	gw, conn := reconnectFixture(t)
	gw.resumeErrors = append(gw.resumeErrors, &gateway.GatewayClientError{Msg: "not_found"})
	gw.hangUp(t, conn)
	wantStatus(t, conn, 2)
	if sub := conn.State(reconnectSID).SubscriptionID; sub != nil {
		t.Fatalf("subscription %v", sub)
	}
	gw.hangUp(t, conn)
	wantStatus(t, conn, 3)
	if resumes := gw.life(2).resumes(); len(resumes) != 0 {
		t.Fatalf("resumed %v", resumes)
	}
}

func TestATransientResumeFailureFailsTheReconnectAndTheNextCallRetries(t *testing.T) {
	failures := map[string]error{"storage_unavailable": &gateway.GatewayClientError{Msg: "storage_unavailable"},
		"timeout": &pyrepr.FormatError{Class: "TimeoutError"}}
	for name, failure := range failures {
		t.Run(name, func(t *testing.T) {
			gw, conn := reconnectFixture(t)
			gw.resumeErrors = append(gw.resumeErrors, failure)
			gw.hangUp(t, conn)
			_, err := conn.Call(context.Background(), "status", map[string]any{})
			wantClientError(t, err, "")
			wantStatus(t, conn, 3)
			if resumes := gw.life(2).resumes(); !reflect.DeepEqual(resumes, []any{reconnectSID}) {
				t.Fatalf("resumed %v", resumes)
			}
			wantEqual(t, conn.State(reconnectSID).SubscriptionID, "sub-3")
		})
	}
}

func TestCloseAfterAFailedResumeIsStillFinal(t *testing.T) {
	ctx := context.Background()
	gw, conn := reconnectFixture(t)
	gw.resumeErrors = append(gw.resumeErrors, &gateway.GatewayClientError{Msg: "storage_unavailable"})
	gw.hangUp(t, conn)
	_, err := conn.Call(ctx, "status", map[string]any{})
	wantClientError(t, err, "")
	conn.Close(ctx)
	_, err = conn.Call(ctx, "status", map[string]any{})
	wantClientError(t, err, "")
	if gw.count() != 2 {
		t.Fatalf("lives %d", gw.count())
	}
}

type unansweredSocket struct {
	sent   chan struct{}
	once   sync.Once
	closed chan struct{}
	shut   sync.Once
}

func (s *unansweredSocket) Recv() (string, error) {
	<-s.closed
	return "", errors.New("closed")
}

func (s *unansweredSocket) Send(string) error {
	s.once.Do(func() { close(s.sent) })
	return nil
}

func (s *unansweredSocket) Open() bool          { return true }
func (s *unansweredSocket) Subprotocol() string { return "" }

func (s *unansweredSocket) Close() error {
	s.shut.Do(func() { close(s.closed) })
	return nil
}

func TestACancelledReconnectCancelsItsSessionResume(t *testing.T) {
	conn := NewGatewayConnection(nil)
	conn.State(reconnectSID).SubscriptionID = "sub-1"
	socket := &unansweredSocket{sent: make(chan struct{}), closed: make(chan struct{})}
	client := gateway.NewGatewayClient(socket).Start()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- conn.resubscribe(ctx, client) }()
	select {
	case <-socket.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the resume was never sent")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("resubscribe error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled resubscribe kept waiting")
	}
	if pending := client.PendingCount(); pending != 0 {
		t.Fatalf("pending requests %d", pending)
	}
}

func TestAHungUpStoreStaysOpenAndItsNextCallRedials(t *testing.T) {
	gw, conn := reconnectFixture(t)
	store := reconnectStore(t, conn)
	gw.hangUp(t, conn)
	if !store.Bool() {
		t.Fatal("a hung up store reads as closed")
	}
	result, err := store.Call(context.Background(), "status", map[string]any{})
	if err != nil || !reflect.DeepEqual(result, map[string]any{"status": "ok", "life": 2}) {
		t.Fatal(result, err)
	}
	if !store.Bool() {
		t.Fatal("the store closed after its redial")
	}
}

func TestSharedKeepsAHungUpConnectionInsteadOfOpeningASecond(t *testing.T) {
	gw, conn := reconnectFixture(t)
	sharedConnMu.Lock()
	previous := sharedConn
	sharedConn = conn
	sharedConnMu.Unlock()
	t.Cleanup(func() {
		sharedConnMu.Lock()
		sharedConn = previous
		sharedConnMu.Unlock()
	})
	gw.hangUp(t, conn)
	if SharedGatewayConnection() != conn {
		t.Fatal("a hung up connection was replaced")
	}
}

func TestNewAfterAHangUpRedialsAndDetachesWithTheEndReason(t *testing.T) {
	gw, conn := reconnectFixture(t)
	store := reconnectStore(t, conn)
	gw.hangUp(t, conn)
	if err := store.EndSession(context.Background(), reconnectSID, "new_session"); err != nil {
		t.Fatal(err)
	}
	want := []fakeCall{{"session.resume", map[string]any{"session_id": reconnectSID}}, {"session.detach", map[string]any{
		"session_id": reconnectSID, "subscription_id": "sub-2", "end_reason": "new_session"}}}
	if calls := gw.life(1).all(); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	gw.hangUp(t, conn)
	wantStatus(t, conn, 3)
	if resumes := gw.life(2).resumes(); len(resumes) != 0 {
		t.Fatalf("resumed %v", resumes)
	}
}

func TestLostBefore(t *testing.T) {
	failure := &gateway.GatewayClientError{Msg: "Gateway disconnected"}
	if LostBefore(failure, 2) {
		t.Fatal("an untagged loss read as an older connection's")
	}
	tagged := &ConnectionLoss{Err: failure, ConnectionEpoch: 1}
	if !LostBefore(tagged, 2) || LostBefore(tagged, 1) || LostBefore(tagged, 0) {
		t.Fatal("epoch comparison")
	}
	if LostBefore(map[string]any{"connection_epoch": 1}, 2) {
		t.Fatal("a frame read as a loss")
	}
	if !strings.Contains(tagged.Error(), "disconnected") {
		t.Fatal(tagged.Error())
	}
}

func TestACallRacingARedialWaitsForItAndIsSent(t *testing.T) {
	gw, conn := reconnectFixture(t)
	gw.hangUp(t, conn)
	gw.mu.Lock()
	gw.dialing, gw.hold = make(chan struct{}, 1), make(chan struct{})
	dialing, hold := gw.dialing, gw.hold
	gw.mu.Unlock()
	ctx := context.Background()
	type outcome struct {
		result any
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		result, err := conn.Call(ctx, "status", nil)
		first <- outcome{result, err}
	}()
	select {
	case <-dialing:
	case <-time.After(5 * time.Second):
		t.Fatal("the redial never started")
	}
	second := make(chan outcome, 1)
	go func() {
		result, err := conn.Call(ctx, "status", nil)
		second <- outcome{result, err}
	}()
	select {
	case got := <-second:
		t.Fatalf("the racing call answered before the redial finished: %v %v", got.result, got.err)
	case <-time.After(200 * time.Millisecond):
	}
	close(hold)
	want := map[string]any{"status": "ok", "life": 2}
	for _, ch := range []chan outcome{second, first} {
		select {
		case got := <-ch:
			if got.err != nil || !reflect.DeepEqual(got.result, want) {
				t.Fatalf("call %v %v", got.result, got.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("a call never returned")
		}
	}
	if gw.count() != 2 {
		t.Fatalf("lives %d", gw.count())
	}
}
