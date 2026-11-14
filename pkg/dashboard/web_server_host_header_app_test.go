package dashboard

import "testing"

// Sets an app state attribute for the case and restores or deletes it afterwards
func stateForCase(t *testing.T, app *FastAPI, name string, v any) {
	t.Helper()
	prev, had := app.State.Get(name), app.State.Get(name) != nil
	app.State.Set(name, v)
	t.Cleanup(func() {
		if had {
			app.State.Set(name, prev)
			return
		}
		app.State.Del(name)
	})
}

// test_web_server_host_header.py TestHostHeaderMiddleware test_rebinding_request_rejected
func TestHostHeaderMiddlewareRebindingRequestRejected(t *testing.T) {
	app := dashboardTestApp(t)
	stateForCase(t, app, "bound_host", "127.0.0.1")
	if r := newTestClient(t, app).get("/api/status", "Host", "evil.example"); r.Status != 400 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// test_web_server_host_header.py TestHostHeaderMiddleware test_trusted_public_host_request_accepted
func TestHostHeaderMiddlewareTrustedPublicHostRequestAccepted(t *testing.T) {
	app := dashboardTestApp(t)
	stateForCase(t, app, "bound_host", "127.0.0.1")
	stateForCase(t, app, "trusted_public_hosts", map[string]bool{"dashboard.example.test": true})
	if r := newTestClient(t, app).get("/api/status", "Host", "dashboard.example.test:9443"); r.Status == 400 {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
}

// test_web_server_host_header.py TestHostHeaderMiddleware test_no_bound_host_skips_validation
func TestHostHeaderMiddlewareNoBoundHostSkipsValidation(t *testing.T) {
	app := dashboardTestApp(t)
	app.State.Del("bound_host")
	if r := newTestClient(t, app).get("/api/status"); r.Status == 400 || r.AppErr != nil {
		t.Fatalf("%d %s %v", r.Status, r.Body, r.AppErr)
	}
}
