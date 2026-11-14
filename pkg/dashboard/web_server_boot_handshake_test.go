package dashboard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Seconds the slow fixtures of the boot handshake cases take, the Defender worst case scaled down
const bootSlowSeconds = 1

// Sets a package variable for the case and restores it after
func swapVar[T any](t *testing.T, target *T, value T) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// Lifespan hooks of a test app under a temp home
func bootLifespan(t *testing.T) (*FastAPI, func(context.Context) error, func(context.Context) error) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	app := dashboardTestApp(t)
	enter, exit := appLifespan(func() *FastAPI { return app })
	return app, enter, exit
}

func TestLifespanWarmupIsSynchronous(t *testing.T) {
	_, enter, exit := bootLifespan(t)
	var done atomic.Bool
	swapVar(t, &lifespanWarmGatewayModule, func(context.Context) {
		time.Sleep(200 * time.Millisecond)
		done.Store(true)
	})
	ctx := context.Background()
	if err := enter(ctx); err != nil {
		t.Fatal(err)
	}
	defer exit(ctx)
	if !done.Load() {
		t.Fatal("startup completed before the gateway warmup ran, the warmup was moved off the lifespan")
	}
}

func TestLifespanEntersAndExitsAsAnAsyncContextManager(t *testing.T) {
	_, enter, exit := bootLifespan(t)
	swapVar(t, &lifespanWarmGatewayModule, func(context.Context) {})
	swapVar(t, &lifespanEagerReconcile, func(context.Context) {})
	ctx := context.Background()
	var entered []bool
	if err := enter(ctx); err != nil {
		t.Fatal(err)
	}
	entered = append(entered, true)
	if err := exit(ctx); err != nil {
		t.Fatal(err)
	}
	if len(entered) != 1 || !entered[0] {
		t.Fatalf("entered = %v", entered)
	}
}

func TestHostedRoomRecoveryCannotBlockOrAbortBackendStartup(t *testing.T) {
	_, enter, exit := bootLifespan(t)
	ctx := context.Background()
	swapVar(t, &lifespanWarmGatewayModule, func(context.Context) {})
	swapVar(t, &lifespanStopHostedRooms, func(float64) bool { return true })
	swapVar(t, &lifespanStartHostedRooms, func() (tuigateway.HostedRoomService, error) { return nil, nil })
	if err := enter(ctx); err != nil {
		t.Fatal(err)
	}
	if err := exit(ctx); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	swapVar(t, &lifespanStartHostedRooms, func() (tuigateway.HostedRoomService, error) {
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-time.After(2 * time.Second):
		}
		return nil, errors.New("state.db is locked")
	})
	before := time.Now()
	if err := enter(ctx); err != nil {
		t.Fatal(err)
	}
	defer exit(ctx)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("hosted room recovery never started")
	}
	if elapsed := time.Since(before); elapsed >= time.Second {
		t.Fatalf("startup waited %v on hosted room recovery", elapsed)
	}
	close(release)
}

func TestLifespanShutdownJoinsStatedbReconcileWorker(t *testing.T) {
	_, enter, exit := bootLifespan(t)
	ctx := context.Background()
	started, finished := make(chan struct{}), make(chan struct{})
	swapVar(t, &lifespanWarmGatewayModule, func(context.Context) {})
	swapVar(t, &lifespanEagerReconcile, func(context.Context) {
		close(started)
		time.Sleep(bootSlowSeconds * time.Second)
		close(finished)
	})
	before := time.Now()
	if err := enter(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("the reconcile worker never started")
	}
	if elapsed := time.Since(before); elapsed >= bootSlowSeconds*800*time.Millisecond {
		t.Fatalf("startup waited %v on the reconcile worker", elapsed)
	}
	if err := exit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("lifespan shutdown returned before the reconcile worker finished")
	}
}

func TestGetStatusDoesNotBlockEventLoop(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	app := dashboardTestApp(t)
	swapVar(t, &statusRestartDrainTimeout, func(context.Context) (float64, error) {
		time.Sleep(bootSlowSeconds * time.Second)
		return 180.0, nil
	})
	client := newTestClient(t, app)
	var wg sync.WaitGroup
	var statusCode, versionCode int
	var statusTook, versionTook time.Duration
	wg.Add(2)
	go func() {
		defer wg.Done()
		begun := time.Now()
		statusCode = client.get("/api/status").Status
		statusTook = time.Since(begun)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		begun := time.Now()
		versionCode = client.get("/api/version").Status
		versionTook = time.Since(begun)
	}()
	wg.Wait()
	if versionCode == 0 || statusCode == 0 {
		t.Fatalf("status %d version %d", statusCode, versionCode)
	}
	if versionTook >= bootSlowSeconds*time.Second {
		t.Fatalf("/api/version took %v while /api/status waited %v for the slow drain timeout", versionTook, statusTook)
	}
	if statusCode != 200 {
		t.Fatalf("/api/status returned %d", statusCode)
	}
}
