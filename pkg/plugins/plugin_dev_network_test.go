package plugins

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A Doctor host refuses guest HTTP with the Doctor RuntimeError and hands its home override to guest env reads
func TestDoctorHostDeniesGuestNetwork(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	installHostEnvHooks()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer srv.Close()
	host, err := newPluginHost(NewManager(h))
	if err != nil {
		t.Fatal(err)
	}
	host.contexts["probe"] = &Context{}
	host.guest = &guestSpec{name: "probe", grants: guestGrants{net: []string{"*"}, env: map[string]bool{"ICEBATH_BUNDLED_PLUGINS": true}}}
	request := map[string]any{"plugin": "probe", "op": "http", "url": srv.URL}
	if _, err := host.serveIO(context.Background(), request); err != nil {
		t.Fatalf("plain host refused granted HTTP %v", err)
	}
	host.envOverride = map[string]string{"ICEBATH_BUNDLED_PLUGINS": "/doctor/bundled", HostDenyNetworkEnv: "1"}
	_, err = host.serveIO(context.Background(), request)
	var refusal *HostBuiltinError
	if !errors.As(err, &refusal) || refusal.Type != "RuntimeError" || refusal.Message != "network access is disabled while Plugin Doctor runs" {
		t.Fatalf("doctor host answered %v", err)
	}
	if hits != 1 {
		t.Fatalf("server saw %d requests", hits)
	}
	got, err := host.serveIO(context.Background(), map[string]any{"plugin": "probe", "op": "env", "name": "ICEBATH_BUNDLED_PLUGINS"})
	if err != nil || got.(map[string]any)["value"] != "/doctor/bundled" {
		t.Fatalf("env read %v %v", got, err)
	}
}
