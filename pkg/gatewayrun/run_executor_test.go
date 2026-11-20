package gatewayrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Waits until the unbounded executor has n parked workers
func waitParked(t *testing.T, ex *unboundedThreadExecutor, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ex.lock.Lock()
		parked := len(ex.idle)
		ex.lock.Unlock()
		if parked >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never parked")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Bare runner with the executor attributes the pool cases set
func executorRunner() *GatewayRunner {
	r := allocGatewayRunner()
	r.executorLock = &sync.Mutex{}
	return r
}

// Submits fn and waits up to five seconds for its result
func submitWait(t *testing.T, ex gatewayExecutor, fn func(ctx context.Context) (any, error)) any {
	t.Helper()
	fut, err := ex.submit(fn)
	if err != nil {
		t.Fatal(err)
	}
	out, err := fut.resultWithin(5)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAReusedWorkerSeesNoneOfThePreviousTurnsThreadState(t *testing.T) {
	freshHome := home.Dir(context.Background())
	ex := newUnboundedThreadExecutor("reuse-probe", turnExecutorIdleTimeoutS)
	defer ex.shutdown(true, false)
	first := submitWait(t, ex, func(ctx context.Context) (any, error) {
		tools.SetInterrupt(tools.WorkerOf(ctx), true, "user pressed stop")
		_ = home.WithOverride(ctx, "/profiles/session-a")
		tools.SetThreadSudoPassword(ctx, "session-a sudo password")
		return currentExecutorThread(ctx), nil
	})
	waitParked(t, ex, 1)
	type seen struct {
		thread      *executorThread
		interrupted bool
		reason      string
		home        string
		sudo        string
	}
	got := submitWait(t, ex, func(ctx context.Context) (any, error) {
		return seen{currentExecutorThread(ctx), tools.IsInterrupted(ctx), tools.InterruptReason(ctx), home.Dir(ctx), tools.ThreadSudoPassword(ctx)}, nil
	}).(seen)
	if got.thread != first {
		t.Fatal("the parked worker was not reused")
	}
	if got.interrupted || got.reason != "" || got.home != freshHome || got.sudo != "" {
		t.Fatalf("reused worker saw %+v", got)
	}
}

func TestSubmitToAParkedWorkerStartsNoThreadAndShutdownReleasesIt(t *testing.T) {
	r := executorRunner()
	pool, err := r.getExecutor()
	if err != nil {
		t.Fatal(err)
	}
	ex, ok := pool.(*unboundedThreadExecutor)
	if !ok {
		t.Fatalf("executor is %T", pool)
	}
	submitWait(t, ex, func(context.Context) (any, error) { return nil, nil })
	waitParked(t, ex, 1)
	ex.lock.Lock()
	parked := ex.idle[0].thread
	started := ex.n
	ex.lock.Unlock()
	if out := submitWait(t, ex, func(context.Context) (any, error) { return "ran", nil }); out != "ran" {
		t.Fatalf("got %v", out)
	}
	ex.lock.Lock()
	if ex.n != started {
		t.Fatal("submit started a thread on the event loop while a worker was parked")
	}
	ex.lock.Unlock()
	waitParked(t, ex, 1)
	if live := r.shutdownExecutor(5); live != 0 {
		t.Fatalf("live = %d", live)
	}
	parked.join(5)
	if parked.isAlive() {
		t.Fatal("parked worker still alive")
	}
}

func TestAReusedWorkerDoesNotLogAsThePreviousTurnsSession(t *testing.T) {
	ex := newUnboundedThreadExecutor("reuse-probe", turnExecutorIdleTimeoutS)
	defer ex.shutdown(true, false)
	first := submitWait(t, ex, func(ctx context.Context) (any, error) {
		logging.SetSessionContext("session-a")
		return currentExecutorThread(ctx), nil
	})
	waitParked(t, ex, 1)
	type tagged struct {
		thread *executorThread
		tag    string
	}
	got := submitWait(t, ex, func(ctx context.Context) (any, error) {
		rec := logging.MakeRecord(ctx, "gateway.run", logging.LevelInfo, "msg")
		return tagged{currentExecutorThread(ctx), rec.SessionTag}, nil
	}).(tagged)
	if got.thread != first {
		t.Fatal("the parked worker was not reused")
	}
	if got.tag != "" {
		t.Fatalf("the reused thread logs as %q", got.tag)
	}
}

func TestANewTurnStartsWhileManyAreParked(t *testing.T) {
	r := executorRunner()
	release := make(chan struct{})
	parked := make(chan struct{}, 40)
	ex, err := r.getExecutor()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(release)
		r.shutdownExecutor(5)
	}()
	for range 40 {
		if _, err := ex.submit(func(context.Context) (any, error) {
			parked <- struct{}{}
			select {
			case <-release:
			case <-time.After(30 * time.Second):
				return nil, errors.New("release never set")
			}
			return nil, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for range 40 {
		select {
		case <-parked:
		case <-time.After(10 * time.Second):
			t.Fatal("a parked body never started")
		}
	}
	started := make(chan struct{})
	if _, err := ex.submit(func(context.Context) (any, error) { close(started); return nil, nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the 41st turn body queued behind 40 parked ones")
	}
}

func TestAbandonedHousekeepingStaysOffTheTurnPool(t *testing.T) {
	wedge := make(chan struct{})
	var mu sync.Mutex
	var ranOn []string
	wedgedCleanup := func(ctx context.Context) (any, error) {
		mu.Lock()
		ranOn = append(ranOn, currentExecutorThread(ctx).name)
		mu.Unlock()
		select {
		case <-wedge:
		case <-time.After(60 * time.Second):
			return nil, errors.New("wedge never released")
		}
		return nil, nil
	}
	r := executorRunner()
	defer func() {
		close(wedge)
		r.shutdownExecutor(0)
	}()
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_, _ = r.runHousekeepingInExecutor(ctx, wedgedCleanup)
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if len(ranOn) == 0 {
		t.Fatal("no housekeeping item started")
	}
	for _, name := range ranOn {
		if !strings.HasPrefix(name, "icebath-gateway-hk") {
			t.Fatalf("ran on %v", ranOn)
		}
	}
	if r.executor != nil {
		t.Fatal("housekeeping created or used the turn pool")
	}
}

func TestShutdownStopsTheHousekeepingPool(t *testing.T) {
	wedge := make(chan struct{})
	entered := make(chan struct{})
	r := executorRunner()
	hk, err := r.getHousekeepingExecutor()
	if err != nil {
		t.Fatal(err)
	}
	if hk.isShutdown() {
		t.Fatal("fresh pool is shut down")
	}
	if _, err := hk.submit(func(context.Context) (any, error) {
		close(entered)
		select {
		case <-wedge:
		case <-time.After(60 * time.Second):
			return nil, errors.New("wedge never released")
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wedged housekeeping item never started")
	}
	began := time.Now()
	live := r.shutdownExecutor(0.2)
	elapsed := time.Since(began)
	close(wedge)
	if live != 1 {
		t.Fatalf("wedged housekeeping worker not reported as live (got %d)", live)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("drain deadline not honoured for the housekeeping pool (%v)", elapsed)
	}
	if !hk.isShutdown() {
		t.Fatal("housekeeping pool was left running at shutdown")
	}
	if r.housekeepingExecutor != nil {
		t.Fatal("housekeeping executor slot kept")
	}
	var rte *pyRuntimeError
	if _, err := r.getHousekeepingExecutor(); !errors.As(err, &rte) {
		t.Fatalf("expected RuntimeError, got %v", err)
	}
}

// Runner holding a two worker plain pool as its turn executor like the fake gateway
func quiesceRunner() *GatewayRunner {
	r := executorRunner()
	r.executor = newThreadPoolExecutor(2, "quiesce-test")
	return r
}

func TestShutdownExecutorDefaultsToNoWait(t *testing.T) {
	r := quiesceRunner()
	release := make(chan struct{})
	started := make(chan struct{})
	fut, err := r.executor.submit(func(context.Context) (any, error) {
		close(started)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("work never started")
	}
	began := time.Now()
	stillLive := r.shutdownExecutor(0)
	elapsed := time.Since(began)
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("default call waited %v", elapsed)
	}
	if stillLive != 1 {
		t.Fatalf("still live %d", stillLive)
	}
	close(release)
	if _, err := fut.resultWithin(5); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownExecutorReportsAStuckWorker(t *testing.T) {
	r := quiesceRunner()
	release := make(chan struct{})
	started := make(chan struct{})
	fut, err := r.executor.submit(func(context.Context) (any, error) {
		close(started)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("work never started")
	}
	began := time.Now()
	stillLive := r.shutdownExecutor(0.2)
	elapsed := time.Since(began)
	if stillLive != 1 {
		t.Fatalf("still live %d", stillLive)
	}
	if elapsed < 150*time.Millisecond || elapsed >= 2*time.Second {
		t.Fatalf("budget not honoured: %v", elapsed)
	}
	close(release)
	if _, err := fut.resultWithin(5); err != nil {
		t.Fatal(err)
	}
}
