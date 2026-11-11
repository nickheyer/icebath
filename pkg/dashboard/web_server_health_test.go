package dashboard

import (
	"context"
	"testing"
)

func TestDashboardComponentHealthSelftestRecordsFailureOn500(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	saved := DashboardHealthState
	DashboardHealthState = NewDashboardHealth(dashboardHealthWindowSeconds)
	t.Cleanup(func() { DashboardHealthState = saved })
	app := NewFastAPI("Icebath Agent", "0", nil, nil)
	app.AddAPIRoute("/api/sessions", []string{"GET"}, &Endpoint{Name: "list_sessions",
		Handle: func(context.Context, *Call) (any, error) { return MustJSONResponse(map[string]any{}, 500), nil }})
	dashboardSelftestOnce(context.Background(), app)
	status, httpStatus, _ := DashboardHealthState.Selftest()
	if status != "failing" || httpStatus == nil || *httpStatus != 500 {
		t.Fatalf("selftest %s %v", status, httpStatus)
	}
	if DashboardHealthState.Snapshot().Vals["status"] != "degraded" {
		t.Fatal("a failing self test must degrade the component")
	}
}
