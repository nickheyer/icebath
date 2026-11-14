package tuiserver

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Waits for the queued marker writes of the single toucher to land
func wsTestDrainMarkerWrites(t *testing.T) {
	t.Helper()
	settled := wsTestSettle(func() bool {
		dashboardClientTouch.Lock()
		defer dashboardClientTouch.Unlock()
		return !dashboardClientTouch.running && len(dashboardClientTouch.queue) == 0
	}, 10*time.Second)
	if !settled {
		t.Fatal("marker writes never drained")
	}
}

// Fresh home with the marker throttle rewound for one case
func wsTestMarkerHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	wsTestDrainMarkerWrites(t)
	dashboardClientTouch.Lock()
	dashboardClientTouch.touchedAt = 0
	dashboardClientTouch.pending = map[string]bool{}
	dashboardClientTouch.Unlock()
	t.Cleanup(func() { wsTestDrainMarkerWrites(t) })
	return h
}

// Replaces the marker write for one case
func wsTestMarkerTouch(t *testing.T, touch func(path string) bool) {
	t.Helper()
	prev := wsTouchDashboardClient
	wsTouchDashboardClient = touch
	t.Cleanup(func() {
		wsTestDrainMarkerWrites(t)
		wsTouchDashboardClient = prev
	})
}

func TestHandleWsConnectTouchesMarker(t *testing.T) {
	wsTestMarkerHome(t)
	wsTestNoOrphanGrace(t)
	if seen := gateway.DashboardClientLastSeen("", nil); seen != nil {
		t.Fatalf("a marker exists before any client: %v", *seen)
	}
	sock := newWSTestSocket()
	sock.inbound <- nil
	if err := HandleWS(context.Background(), sock, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	wsTestDrainMarkerWrites(t)
	seen := gateway.DashboardClientLastSeen("", nil)
	if seen == nil || math.Abs(*seen-float64(time.Now().UnixNano())/1e9) >= 5 {
		t.Fatalf("marker last seen %v", seen)
	}
}

func TestHandleWsInboundFramesRefreshMarker(t *testing.T) {
	wsTestMarkerHome(t)
	wsTestNoOrphanGrace(t)
	prevInterval := dashboardClientTouchMinIntervalS
	dashboardClientTouchMinIntervalS = 0
	t.Cleanup(func() { dashboardClientTouchMinIntervalS = prevInterval })
	var touches atomic.Int64
	wsTestMarkerTouch(t, func(path string) bool {
		touches.Add(1)
		return gateway.TouchDashboardClientHeartbeat(path)
	})
	sock := newWSTestSocket()
	sock.onReceive = func() { wsTestDrainMarkerWrites(t) }
	ping := `{"jsonrpc":"2.0","method":"gateway.ping","id":1}`
	sock.inbound <- ping
	sock.inbound <- ping
	sock.inbound <- nil
	if err := HandleWS(context.Background(), sock, nil, nil, false); err != nil {
		t.Fatal(err)
	}
	wsTestDrainMarkerWrites(t)
	if got := touches.Load(); got != 3 {
		t.Fatalf("%d marker writes, want one on connect and one per frame", got)
	}
	if gateway.DashboardClientLastSeen("", nil) == nil {
		t.Fatal("no marker after the frames")
	}
}

func TestNoteActivityIsThrottled(t *testing.T) {
	wsTestMarkerHome(t)
	var calls atomic.Int64
	wsTestMarkerTouch(t, func(string) bool { calls.Add(1); return true })
	noteDashboardClientActivity(true)
	noteDashboardClientActivity(false)
	noteDashboardClientActivity(false)
	wsTestDrainMarkerWrites(t)
	if calls.Load() != 1 {
		t.Fatalf("%d writes inside the throttle window", calls.Load())
	}
	noteDashboardClientActivity(true)
	wsTestDrainMarkerWrites(t)
	if calls.Load() != 2 {
		t.Fatalf("%d writes after a forced touch", calls.Load())
	}
}

func TestSlowMarkerWriteDoesNotHoldTheConnection(t *testing.T) {
	wsTestMarkerHome(t)
	wsTestNoOrphanGrace(t)
	release, written := make(chan struct{}), make(chan struct{})
	var once sync.Once
	wsTestMarkerTouch(t, func(string) bool {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		once.Do(func() { close(written) })
		return true
	})
	sock := newWSTestSocket()
	sock.inbound <- nil
	done := wsTestServe(context.Background(), sock, nil, false)
	var served error
	select {
	case served = <-done:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the connection waited for the slow marker write")
	}
	close(release)
	if served != nil {
		t.Fatal(served)
	}
	ready := false
	for _, line := range sock.lines {
		ready = ready || strings.Contains(line, "gateway.ready")
	}
	if !ready {
		t.Fatalf("no ready frame in %v", sock.lines)
	}
	select {
	case <-written:
	case <-time.After(10 * time.Second):
		t.Fatal("the marker write never ran")
	}
}

func TestRefusedMarkerWriteDoesNotWedgeItsPath(t *testing.T) {
	wsTestMarkerHome(t)
	var calls atomic.Int64
	wsTestMarkerTouch(t, func(string) bool { calls.Add(1); return true })
	realSubmit := wsSubmitDashboardClient
	t.Cleanup(func() { wsSubmitDashboardClient = realSubmit })
	wsSubmitDashboardClient = func(string) error {
		return errors.New("cannot schedule new futures after interpreter shutdown")
	}
	noteDashboardClientActivity(true)
	dashboardClientTouch.Lock()
	wedged := dashboardClientTouch.pending[gateway.DashboardClientHeartbeatPath("")]
	dashboardClientTouch.Unlock()
	if wedged {
		t.Fatal("a refused write left its path pending")
	}
	wsSubmitDashboardClient = realSubmit
	noteDashboardClientActivity(true)
	wsTestDrainMarkerWrites(t)
	if calls.Load() != 1 {
		t.Fatalf("%d writes after the refusal", calls.Load())
	}
}

func TestStalledMarkerWriteCoalescesLaterTouches(t *testing.T) {
	wsTestMarkerHome(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	wsTestMarkerTouch(t, func(string) bool {
		calls.Add(1)
		once.Do(func() { close(started) })
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		return true
	})
	noteDashboardClientActivity(true)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("the first write never started")
	}
	for i := 0; i < 20; i++ {
		noteDashboardClientActivity(true)
	}
	close(release)
	wsTestDrainMarkerWrites(t)
	if calls.Load() != 2 {
		t.Fatalf("%d writes, want the stalled one and a single follow up", calls.Load())
	}
}

// The marker write takes the process home and no request scope, so a served profile override never moves it
func TestMarkerWriteLandsInTheLaunchHomeUnderAProfileOverride(t *testing.T) {
	h := wsTestMarkerHome(t)
	served := filepath.Join(h, "served-profile")
	noteDashboardClientActivity(true)
	wsTestDrainMarkerWrites(t)
	if gateway.DashboardClientLastSeen("", nil) == nil {
		t.Fatal("the launch home got no marker")
	}
	if _, err := os.Stat(gateway.DashboardClientHeartbeatPath(served)); !os.IsNotExist(err) {
		t.Fatalf("the served profile home got a marker: %v", err)
	}
}
