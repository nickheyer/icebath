package dashboard

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

func TestHTTPLifespanDoesNotStartOrStopRuntimeServices(t *testing.T) {
	app, enter, exit := bootLifespan(t)
	t.Setenv("ICEBATH_DESKTOP", "1")
	var mu sync.Mutex
	var calls []string
	record := func(name string) {
		mu.Lock()
		calls = append(calls, name)
		mu.Unlock()
	}
	recorded := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string{}, calls...)
	}
	swapVar(t, &lifespanWarmGatewayModule, func(context.Context) { record("_warm_gateway_module") })
	swapVar(t, &lifespanEagerReconcile, func(context.Context) { record("_eager_reconcile_own_session_db") })
	swapVar(t, &lifespanStartCronTicker, func(context.Context, <-chan struct{}, int) { record("_start_desktop_cron_ticker") })
	swapVar(t, &lifespanTerminateGateway, func() { record("_terminate_desktop_managed_gateway") })
	swapVar(t, &lifespanStartHostedRooms, func() (tuigateway.HostedRoomService, error) {
		record("start_hosted_room_service")
		return nil, nil
	})
	swapVar(t, &lifespanStopHostedRooms, func(float64) bool {
		record("stop_hosted_room_service")
		return true
	})
	app.State.Set("gateway_runner", struct{}{})
	ctx := context.Background()
	if err := enter(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("calls inside the lifespan = %v", got)
	}
	if err := exit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorded(); len(got) != 0 {
		t.Fatalf("calls after the lifespan = %v", got)
	}
}
