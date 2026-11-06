package gateway

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func drainHome(t *testing.T) context.Context {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	return context.Background()
}

func withEpoch(t *testing.T, epoch string) {
	prev := CurrentInstantiationEpoch
	CurrentInstantiationEpoch = func() string { return epoch }
	t.Cleanup(func() { CurrentInstantiationEpoch = prev })
}

func writeDrainMarker(t *testing.T, ctx context.Context, body *pyjson.Object) {
	raw, err := pyjson.Dumps(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DrainRequestPath(ctx, ""), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func expiredStamp(offset float64) string {
	return pydatetime.ISOFormat(time.Now().UTC().Add(-time.Duration((DrainRequestMaxAgeSeconds + offset) * float64(time.Second))))
}

func TestDrainMarkerContract(t *testing.T) {
	ctx := drainHome(t)
	if DrainRequested(ctx, "") || ReadDrainRequest(ctx, "") != nil {
		t.Fatal("marker present by default")
	}
	payload, err := WriteDrainRequest(ctx, "nas", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if !DrainRequested(ctx, "") || payload.Vals["action"] != "drain" || payload.Vals["principal"] != "nas" {
		t.Fatalf("payload %v", payload.Vals)
	}
	if body := ReadDrainRequest(ctx, ""); body == nil || body.Vals["principal"] != "nas" {
		t.Fatalf("body %v", body)
	}
	if !ClearDrainRequest(ctx, "") || ClearDrainRequest(ctx, "") || DrainRequested(ctx, "") {
		t.Fatal("clear is not idempotent")
	}
}

func TestDrainSuppressNotification(t *testing.T) {
	ctx := drainHome(t)
	payload, _ := WriteDrainRequest(ctx, "nas", false, "")
	if payload.Vals["suppress_notification"] != false || DrainNotificationSuppressed(ctx, "") {
		t.Fatal("default suppression")
	}
	payload, _ = WriteDrainRequest(ctx, "nas", true, "")
	if payload.Vals["suppress_notification"] != true || ReadDrainRequest(ctx, "").Vals["suppress_notification"] != true ||
		!DrainNotificationSuppressed(ctx, "") {
		t.Fatal("suppression did not round trip")
	}
}

func TestDrainInstantiationEpoch(t *testing.T) {
	ctx := drainHome(t)
	payload, _ := WriteDrainRequest(ctx, "nas", false, "")
	if payload.Vals["epoch"] != CurrentInstantiationEpoch() || ReadDrainRequest(ctx, "").Vals["epoch"] != CurrentInstantiationEpoch() {
		t.Fatal("epoch not stamped")
	}
	withEpoch(t, "epoch-OLD")
	WriteDrainRequest(ctx, "nas", false, "")
	if !DrainRequested(ctx, "") {
		t.Fatal("same epoch marker inactive")
	}
	CurrentInstantiationEpoch = func() string { return "epoch-NEW" }
	if _, err := os.Stat(DrainRequestPath(ctx, "")); err != nil || DrainRequested(ctx, "") {
		t.Fatal("prior instantiation marker honoured")
	}
}

func TestDrainEpochEmptyWhenProcUnreadable(t *testing.T) {
	prev := procReadFile
	procReadFile = func(name string) ([]byte, error) {
		if strings.HasPrefix(name, "/proc/") {
			return nil, errors.New("no /proc")
		}
		return prev(name)
	}
	instantiationEpochCacheClear()
	defer func() { procReadFile = prev; instantiationEpochCacheClear() }()
	if got := CurrentInstantiationEpoch(); got != "" {
		t.Fatalf("epoch %q", got)
	}
}

func TestDrainMarkerMaxAge(t *testing.T) {
	ctx := drainHome(t)
	WriteDrainRequest(ctx, "nas", true, "")
	body := ReadDrainRequest(ctx, "")
	body.Set("requested_at", expiredStamp(60))
	writeDrainMarker(t, ctx, body)
	if DrainRequested(ctx, "") || DrainNotificationSuppressed(ctx, "") {
		t.Fatal("expired marker honoured")
	}
	legacy := pyjson.NewObject()
	legacy.Set("action", "drain")
	legacy.Set("epoch", CurrentInstantiationEpoch())
	writeDrainMarker(t, ctx, legacy)
	if !DrainRequested(ctx, "") {
		t.Fatal("marker without timestamp ignored")
	}
	legacy.Set("requested_at", "not-a-timestamp")
	writeDrainMarker(t, ctx, legacy)
	if !DrainRequested(ctx, "") {
		t.Fatal("unparseable timestamp ignored")
	}
	naive := time.Now().UTC().Add(-time.Duration((DrainRequestMaxAgeSeconds + 60) * float64(time.Second)))
	legacy.Set("requested_at", strings.TrimSuffix(pydatetime.ISOFormat(naive), "+00:00"))
	writeDrainMarker(t, ctx, legacy)
	if DrainRequested(ctx, "") {
		t.Fatal("naive stale timestamp honoured")
	}
}

func TestDrainExpiryWarningLoggedOncePerMarker(t *testing.T) {
	ctx := drainHome(t)
	writeExpired := func(offset float64) {
		WriteDrainRequest(ctx, "nas", false, "")
		body := ReadDrainRequest(ctx, "")
		body.Set("requested_at", expiredStamp(offset))
		writeDrainMarker(t, ctx, body)
	}
	writeExpired(60)
	logs := captureLogs(t)
	for range 3 {
		if DrainRequested(ctx, "") {
			t.Fatal("expired marker honoured")
		}
	}
	if n := strings.Count(logs.String(), "expired drain marker"); n != 1 {
		t.Fatalf("%d warnings", n)
	}
	logs.Reset()
	writeExpired(120)
	DrainRequested(ctx, "")
	if n := strings.Count(logs.String(), "expired drain marker"); n != 1 {
		t.Fatalf("%d warnings after refresh", n)
	}
}

func TestDrainRewriteRefreshesTheClock(t *testing.T) {
	ctx := drainHome(t)
	WriteDrainRequest(ctx, "nas", false, "")
	body := ReadDrainRequest(ctx, "")
	body.Set("requested_at", expiredStamp(60))
	writeDrainMarker(t, ctx, body)
	if DrainRequested(ctx, "") {
		t.Fatal("expired marker honoured")
	}
	WriteDrainRequest(ctx, "nas", false, "")
	if !DrainRequested(ctx, "") {
		t.Fatal("keep alive rewrite ignored")
	}
}

type fakeDrainRunner struct {
	descriptor        map[string]any
	draining, running bool
}

func (r *fakeDrainRunner) SessionRuntimeDescriptor() map[string]any { return r.descriptor }
func (r *fakeDrainRunner) Draining() bool                           { return r.draining }
func (r *fakeDrainRunner) Running() bool                            { return r.running }

func TestRestartWaitGatesReadsTicketsAndVerbs(t *testing.T) {
	r := &fakeDrainRunner{descriptor: map[string]any{"drain_reason": "operator"}, draining: true, running: true}
	OpenRestartWait(r)
	if RestartWaitOpen(r) {
		t.Fatal("restart wait overrode a named drain")
	}
	r.descriptor["drain_reason"] = ""
	OpenRestartWait(r)
	if !RestartWaitOpen(r) {
		t.Fatal("restart wait not open")
	}
	for path, want := range map[string]bool{"/api/health": true, "/api/sessions/abc/messages/around": true,
		"/api/profiles/sessions/sidebar": true, "/api/sessions/a/b": false, "/api/config": false} {
		if got := RestartWaitRead(r, "GET", path); got != want {
			t.Errorf("read %s = %v", path, got)
		}
	}
	if RestartWaitRead(r, "POST", "/api/health") || !RestartWaitSocketTicket(r, "POST", "/api/webapp/socket-ticket", "socket=gateway") ||
		RestartWaitSocketTicket(r, "POST", "/api/webapp/socket-ticket", "socket=gateway&socket=gateway") {
		t.Fatal("ticket gate")
	}
	if WSMethodRefusedWhileDraining(r, "prompt.submit") || !WSMethodRefusedWhileDraining(r, "config.set") {
		t.Fatal("ws verb gate")
	}
	r.running = false
	if RestartWaitOpen(r) {
		t.Fatal("stopping runner still in restart wait")
	}
}
