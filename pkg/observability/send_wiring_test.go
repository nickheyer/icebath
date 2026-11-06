package observability

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

type senderFunc func(ctx context.Context)

func (f senderFunc) SendPending(ctx context.Context) SendOutcome {
	f(ctx)
	return SendOutcome{}
}

type senderRecord struct {
	mu        sync.Mutex
	passes    int
	endpoints []string
}

func (r *senderRecord) snapshot() (int, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.passes, slices.Clone(r.endpoints)
}

// Replaces the send pass constructor for the test, restoring it at cleanup
func withSender(t *testing.T, build func(endpoint string) pendingSender) {
	t.Helper()
	saved := newMetricsSender
	newMetricsSender = func(_ *SharedMetricsStore, endpoint string, _ ...SenderOption) pendingSender { return build(endpoint) }
	t.Cleanup(func() { newMetricsSender = saved })
}

func captureSender(t *testing.T) *senderRecord {
	t.Helper()
	record := &senderRecord{}
	withSender(t, func(endpoint string) pendingSender {
		record.mu.Lock()
		record.endpoints = append(record.endpoints, endpoint)
		record.mu.Unlock()
		return senderFunc(func(context.Context) {
			record.mu.Lock()
			record.passes++
			record.mu.Unlock()
		})
	})
	return record
}

// Runtime over a real store under a temp profile with the relay host left out
func wiringRuntime(t *testing.T) (*Runtime, context.Context) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	r := &Runtime{Subscriber: &SharedMetricsSubscriber{Store: openStore(t, metricsRoot(ctx))}}
	t.Cleanup(func() { r.joinSendThread(5 * time.Second) })
	return r, ctx
}

func setWiringConfig(t *testing.T, ctx context.Context, body string) {
	t.Helper()
	path := filepath.Join(home.Dir(ctx), "config.yaml")
	os.RemoveAll(path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
}

func sharedYAML(fields string) string { return "telemetry:\n  shared_metrics: {" + fields + "}\n" }

// Seeds one counter on a pinned day so the next export writes exactly one package
func seedExportable(t *testing.T, r *Runtime) {
	t.Helper()
	now := time.Date(2026, 7, 28, 9, 0, 0, 0, time.UTC)
	setClock(t, &now)
	if _, err := r.Subscriber.Store.RecordClientActive(context.Background(), testResource()); err != nil {
		t.Fatal(err)
	}
}

func slowSender(t *testing.T, started func(), release <-chan struct{}, wait time.Duration, finished func()) {
	withSender(t, func(string) pendingSender {
		return senderFunc(func(context.Context) {
			started()
			select {
			case <-release:
			case <-time.After(wait):
			}
			finished()
		})
	})
}

func TestNoSendWhenNothingIsConfigured(t *testing.T) {
	record := captureSender(t)
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, "{}\n")
	r.export(ctx)
	r.joinSendThread(time.Second)
	if passes, _ := record.snapshot(); passes != 0 {
		t.Fatalf("passes %d", passes)
	}
}

func TestSendsWhenBothAreOn(t *testing.T) {
	record := captureSender(t)
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true"))
	r.export(ctx)
	r.joinSendThread(2 * time.Second)
	if passes, _ := record.snapshot(); passes != 1 {
		t.Fatalf("passes %d", passes)
	}
}

func TestUsesTheResolvedEndpoint(t *testing.T) {
	record := captureSender(t)
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true, endpoint: 'https://staging.test/v1'"))
	r.export(ctx)
	r.joinSendThread(2 * time.Second)
	if _, endpoints := record.snapshot(); !slices.Equal(endpoints, []string{"https://staging.test/v1"}) {
		t.Fatalf("endpoints %v", endpoints)
	}
}

func TestExportStillRunsWhenSendingIsOff(t *testing.T) {
	record := captureSender(t)
	r, ctx := wiringRuntime(t)
	seedExportable(t, r)
	setWiringConfig(t, ctx, sharedYAML("enabled: true"))
	r.export(ctx)
	r.joinSendThread(time.Second)
	if paths := outboxPackages(t, metricsRoot(ctx)); len(paths) != 1 {
		t.Fatalf("export did not run %v", paths)
	}
	if passes, _ := record.snapshot(); passes != 0 {
		t.Fatalf("passes %d", passes)
	}
}

func TestExportReturnsBeforeTheSendFinishes(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slowSender(t, func() { once.Do(func() { close(started) }) }, release, 5*time.Second, func() {})
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true"))
	began := time.Now()
	r.export(ctx)
	elapsed := time.Since(began)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the send should have started")
	}
	if elapsed >= time.Second {
		t.Fatalf("finish_task must not wait on the network %v", elapsed)
	}
	close(release)
	r.joinSendThread(5 * time.Second)
}

func TestTheSendThreadIsADaemon(t *testing.T) {
	captureSender(t)
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true"))
	r.export(ctx)
	r.sendMu.Lock()
	thread := r.sendThread
	r.sendMu.Unlock()
	if thread == nil {
		t.Fatal("no send pass running off the caller's path")
	}
	r.joinSendThread(2 * time.Second)
}

func TestOnlyOnePassRunsAtATime(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	starts := 0
	slowSender(t, func() { mu.Lock(); starts++; mu.Unlock() }, release, 5*time.Second, func() {})
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true"))
	for range 5 {
		r.export(ctx)
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	got := starts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("hook fires must not pile up send passes %d", got)
	}
	close(release)
	r.joinSendThread(5 * time.Second)
}

type consentWindow struct{ opened, closed *string }

func consentWindows(t *testing.T, s *SharedMetricsStore) []consentWindow {
	t.Helper()
	var out []consentWindow
	err := s.connection(context.Background(), busyTimeoutMS, func(conn state.SQLiteConn) error {
		rows, err := conn.QueryContext(context.Background(), "SELECT opened_at, closed_at FROM send_consent_windows ORDER BY opened_at")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var w consentWindow
			if err := rows.Scan(&w.opened, &w.closed); err != nil {
				return err
			}
			out = append(out, w)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Drives the consent edge detector once per configured send value in order
func observeSends(t *testing.T, r *Runtime, ctx context.Context, runs ...any) {
	t.Helper()
	for i := 0; i+1 < len(runs); i += 2 {
		setWiringConfig(t, ctx, sharedYAML("enabled: true, send: "+runs[i].(string)))
		for range runs[i+1].(int) {
			r.sendExportedPackages(ctx)
			r.joinSendThread(2 * time.Second)
		}
	}
}

func TestRevokingWhileIdleClosesTheWindow(t *testing.T) {
	captureSender(t)
	r, ctx := wiringRuntime(t)
	observeSends(t, r, ctx, "true", 1, "false", 6)
	windows := consentWindows(t, r.Subscriber.Store)
	if len(windows) == 0 {
		t.Fatal("no window recorded")
	}
	for _, w := range windows {
		if w.closed == nil {
			t.Fatalf("revoking while idle left a window open: %v", windows)
		}
	}
}

func TestReplayedObservationsCreateNoJunkWindows(t *testing.T) {
	captureSender(t)
	r, ctx := wiringRuntime(t)
	observeSends(t, r, ctx, "true", 4, "false", 4, "true", 4)
	if windows := consentWindows(t, r.Subscriber.Store); len(windows) != 2 {
		t.Fatalf("windows %v", windows)
	}
}

func TestANeverConsentedUserGetsNoWindow(t *testing.T) {
	captureSender(t)
	r, ctx := wiringRuntime(t)
	observeSends(t, r, ctx, "false", 5)
	if windows := consentWindows(t, r.Subscriber.Store); len(windows) != 0 {
		t.Fatalf("windows %v", windows)
	}
}

func TestReEnablingOpensANewWindowAfterTheRefusal(t *testing.T) {
	captureSender(t)
	r, ctx := wiringRuntime(t)
	observeSends(t, r, ctx, "true", 1, "false", 1, "true", 1)
	windows := consentWindows(t, r.Subscriber.Store)
	if len(windows) != 2 {
		t.Fatalf("windows %v", windows)
	}
	first, second := windows[0], windows[1]
	if first.closed == nil || second.closed != nil {
		t.Fatalf("first window must be closed and second open: %v", windows)
	}
	if *second.opened < *first.closed {
		t.Fatalf("new window may not overlap the refused gap: %s < %s", *second.opened, *first.closed)
	}
}

func TestReconcileRunsEvenWhenCollectionIsDisabled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "home")
	mustMkdir(t, dir)
	t.Setenv("ICEBATH_HOME", dir)
	ctx := context.Background()
	store := openStore(t, metricsRoot(ctx))
	storeTxn(t, store, func(conn state.SQLiteConn) error { return ReconcileSendConsent(ctx, conn, true, utcNow()) })
	setWiringConfig(t, ctx, sharedYAML("enabled: false, send: false"))
	consentReconcileMu.Lock()
	consentReconcileDone = false
	consentReconcileMu.Unlock()
	t.Cleanup(resetForTests)
	relayObserveLifecycle(ctx, nil, "finish_task", map[string]any{})
	open := 0
	for _, w := range consentWindows(t, store) {
		if w.closed == nil {
			open++
		}
	}
	if open != 0 {
		t.Fatalf("enabled:false made the consent reconciler unreachable (D1) %d", open)
	}
}

func TestASenderCrashDoesNotPropagate(t *testing.T) {
	withSender(t, func(string) pendingSender { return senderFunc(func(context.Context) { panic("boom") }) })
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true"))
	r.export(ctx)
	r.joinSendThread(2 * time.Second)
}

func TestAnUnreadableConfigDoesNotBreakExport(t *testing.T) {
	record := captureSender(t)
	r, ctx := wiringRuntime(t)
	seedExportable(t, r)
	if err := os.Mkdir(filepath.Join(home.Dir(ctx), "config.yaml"), 0o700); err != nil {
		t.Fatal(err)
	}
	config.ResetCache()
	r.export(ctx)
	if paths := outboxPackages(t, metricsRoot(ctx)); len(paths) != 1 {
		t.Fatalf("export did not run %v", paths)
	}
	if passes, _ := record.snapshot(); passes != 0 {
		t.Fatalf("passes %d", passes)
	}
}

func TestJoinIsSafeWithNoThread(t *testing.T) {
	r, _ := wiringRuntime(t)
	r.joinSendThread(100 * time.Millisecond)
}

func TestJoinWaitsForAnInFlightSend(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var finished []bool
	slowSender(t, func() {}, release, 3*time.Second, func() { mu.Lock(); finished = append(finished, true); mu.Unlock() })
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true"))
	r.export(ctx)
	close(release)
	r.joinSendThread(3 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(finished, []bool{true}) {
		t.Fatalf("finished %v", finished)
	}
}

func TestShutdownJoinsTheSendThread(t *testing.T) {
	released := make(chan struct{})
	var mu sync.Mutex
	var finished []bool
	slowSender(t, func() {}, released, 3*time.Second, func() { mu.Lock(); finished = append(finished, true); mu.Unlock() })
	r, ctx := wiringRuntime(t)
	setWiringConfig(t, ctx, sharedYAML("enabled: true, send: true"))
	r.export(ctx)
	r.sendMu.Lock()
	thread := r.sendThread
	r.sendMu.Unlock()
	if thread == nil {
		t.Fatal("no send thread")
	}
	close(released)
	r.joinSendThread(2 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(finished, []bool{true}) {
		t.Fatalf("shutdown returned while a send was in flight %v", finished)
	}
}
