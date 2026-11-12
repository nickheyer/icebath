package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Default root whose live multiplexer, this process, serves alpha and beta with a mirrored api_server
func servedRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(filepath.Join(root, "profiles", "alpha"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, "config.yaml"), "gateway: {multiplex_profiles: true}\n")
	pidRecord, _ := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "hermes_home", root))
	writeTestFile(t, filepath.Join(root, "gateway.pid"), pidRecord)
	state, _ := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "hermes_home", root, "gateway_state", "running",
		"served_profiles", []any{"default", "alpha", "beta"},
		"platforms", pyjson.ObjectOf(
			"api_server", pyjson.ObjectOf("state", "connected", "listener_base", "http://127.0.0.1:45719"),
			"webhook", pyjson.ObjectOf("state", "fatal", "error_code", "port_in_use"),
			"alpha:telegram", pyjson.ObjectOf("state", "connected"))))
	writeTestFile(t, filepath.Join(root, "gateway_state.json"), state)
	t.Setenv("ICEBATH_HOME", root)
	unsetTestEnv(t, "GATEWAY_MULTIPLEX_PROFILES")
	fakeGatewayCmdline(t, "icebath gateway run")
	return root
}

// Catalog entry of a token free platform with no env vars
func bareCatalogEntry(platformID, name string) *pyjson.Object {
	return pyjson.ObjectOf("id", platformID, "name", name, "description", "", "docs_url", "", "env_vars", []any{}, "required_env", []any{})
}

// Payload of one entry for a scoped dir, failing the test on error
func onePayload(t *testing.T, scopedDir string, entry *pyjson.Object) *pyjson.Object {
	t.Helper()
	payloads, err := platformPayloads(context.Background(), scopedDir, []*pyjson.Object{entry})
	if err != nil {
		t.Fatal(err)
	}
	return payloads[0].(*pyjson.Object)
}

// Enablement that always answers enabled and configured with no home channel
func forceEnablement(t *testing.T) {
	t.Helper()
	saved := messagingPlatformEnablement
	t.Cleanup(func() { messagingPlatformEnablement = saved })
	messagingPlatformEnablement = func(context.Context, string, *pyjson.Object, map[string]string, bool, map[string]bool) (bool, bool, any) {
		return true, true, nil
	}
}

func TestMessagingCardShowsMirroredApiServerEnabledWithoutLocalConfig(t *testing.T) {
	root := servedRoot(t)
	payload := onePayload(t, filepath.Join(root, "profiles", "alpha"), bareCatalogEntry("api_server", "API server"))
	if payload.GetOr("gateway_running", nil) != true || payload.GetOr("enabled", nil) != true || payload.GetOr("state", nil) != "connected" ||
		payload.GetOr("ingress_url", nil) != "http://127.0.0.1:45719/p/alpha/v1" {
		t.Fatalf("payload %v", payload)
	}
}

func TestServedProfileProjectsTheDefaultListenerMirrorsWithTheirUrl(t *testing.T) {
	root := servedRoot(t)
	live := gateway.ResolveGatewayLiveness(gateway.LivenessOptions{ProfileDir: filepath.Join(root, "profiles", "alpha"), NoCache: true})
	plats := gateway.ProfilePlatformsFromMultiplexer(live.Runtime, "alpha")
	api, _ := plats.GetOr("api_server", nil).(*pyjson.Object)
	if api == nil || api.GetOr("state", nil) != "connected" || api.GetOr("ingress_url", nil) != "http://127.0.0.1:45719/p/alpha/v1" {
		t.Fatalf("plats %v", plats)
	}
	if _, has := plats.Get("webhook"); has {
		t.Fatalf("dead listener mirrored %v", plats)
	}
	if text, _ := pyjson.Dumps(plats.GetOr("telegram", nil)); text != `{"state": "connected"}` {
		t.Fatalf("telegram %s", text)
	}
	defaultAPI, _ := gateway.ProfilePlatformsFromMultiplexer(live.Runtime, "default").GetOr("api_server", pyjson.NewObject()).(*pyjson.Object)
	if _, has := defaultAPI.Get("ingress_url"); has {
		t.Fatalf("default api_server mirrored %v", defaultAPI)
	}
}

func TestDefaultProfileKeepsItsFlatAdaptersWhenRekeyed(t *testing.T) {
	standalone := pyjson.ObjectOf("gateway_state", "running", "served_profiles", []any{},
		"platforms", pyjson.ObjectOf("feishu", pyjson.ObjectOf("state", "connected")))
	if text, _ := pyjson.Dumps(gateway.ProfilePlatformsFromMultiplexer(standalone, "default")); text != `{"feishu": {"state": "connected"}}` {
		t.Fatalf("standalone rekey %s", text)
	}
	root := servedRoot(t)
	live := gateway.ResolveGatewayLiveness(gateway.LivenessOptions{ProfileDir: filepath.Join(root, "profiles", "alpha"), NoCache: true})
	plats := gateway.ProfilePlatformsFromMultiplexer(live.Runtime, "default")
	api, _ := plats.GetOr("api_server", nil).(*pyjson.Object)
	webhook, _ := plats.GetOr("webhook", nil).(*pyjson.Object)
	if api == nil || api.GetOr("state", nil) != "connected" || webhook == nil || webhook.GetOr("state", nil) != "fatal" {
		t.Fatalf("plats %v", plats)
	}
	if _, has := api.Get("listener_base"); !has {
		t.Fatalf("api_server %v is no listener entry", api)
	}
	for _, k := range plats.Keys {
		if entry, ok := plats.Vals[k].(*pyjson.Object); ok {
			if _, has := entry.Get("ingress_url"); has {
				t.Fatalf("%s mirrored onto default", k)
			}
		}
	}
	if _, has := plats.Get("telegram"); has {
		t.Fatalf("alpha's telegram leaked into default %v", plats)
	}
}

func TestMessagingCardForTheDefaultHomeRekeysByTheProfileNotTheDirname(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	writeTestFile(t, filepath.Join(root, "config.yaml"), "gateway: {multiplex_profiles: true}\n")
	record := pyjson.ObjectOf("pid", os.Getpid(), "hermes_home", root, "gateway_state", "running",
		"served_profiles", []any{"default", "route-runner"},
		"platforms", pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "connected")))
	t.Setenv("ICEBATH_HOME", root)
	savedRead := messagingReadRuntimeStatus
	t.Cleanup(func() { messagingReadRuntimeStatus = savedRead })
	messagingReadRuntimeStatus = func(string) *pyjson.Object { return nil }
	t.Cleanup(gateway.SwapMultiplexerLivenessForProfile(func(string) (int, *pyjson.Object, bool, error) {
		return os.Getpid(), record, true, nil
	}))
	fakeGatewayCmdline(t, "icebath gateway run")
	forceEnablement(t)
	payload := onePayload(t, "", bareCatalogEntry("telegram", "Telegram"))
	if payload.GetOr("gateway_running", nil) != true || payload.GetOr("state", nil) != "connected" {
		t.Fatalf("payload %v", payload)
	}
}

func TestMessagingCardForAServedProfileReadsConnectedNotRestartNeeded(t *testing.T) {
	root := servedRoot(t)
	forceEnablement(t)
	payload := onePayload(t, filepath.Join(root, "profiles", "alpha"), bareCatalogEntry("api_server", "API server"))
	if payload.GetOr("gateway_running", nil) != true || payload.GetOr("state", nil) != "connected" ||
		payload.GetOr("ingress_url", nil) != "http://127.0.0.1:45719/p/alpha/v1" {
		t.Fatalf("payload %v", payload)
	}
}

func TestMessagingCardIgnoresAServedProfilesStaleOwnRuntimeRecord(t *testing.T) {
	root := servedRoot(t)
	forceEnablement(t)
	entry := bareCatalogEntry("telegram", "Telegram")
	alpha := filepath.Join(root, "profiles", "alpha")
	writeTestFile(t, filepath.Join(alpha, "gateway_state.json"), `{"gateway_state": "stopped", "platforms": {}}`)
	payload := onePayload(t, alpha, entry)
	if payload.GetOr("gateway_running", nil) != true || payload.GetOr("state", nil) != "connected" {
		t.Fatalf("alpha payload %v", payload)
	}
	beta := filepath.Join(root, "profiles", "beta")
	writeTestFile(t, filepath.Join(beta, "gateway_state.json"), `{"gateway_state": "stopped", "platforms": {}}`)
	if state := onePayload(t, beta, entry).GetOr("state", nil); state != "pending_restart" {
		t.Fatalf("beta state %v", state)
	}
}

func TestMessagingCardKeepsALiveOwnGatewayRecordOverTheMultiplexer(t *testing.T) {
	root := servedRoot(t)
	forceEnablement(t)
	alpha := filepath.Join(root, "profiles", "alpha")
	ownPid := 1<<22 - 1
	record, _ := pyjson.Dumps(pyjson.ObjectOf("pid", ownPid, "hermes_home", alpha, "gateway_state", "running",
		"platforms", pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "retrying", "error_code", "network"))))
	writeTestFile(t, filepath.Join(alpha, "gateway_state.json"), record)
	t.Cleanup(gateway.SwapPidExists(func(pid int) bool { return pid == ownPid || proc.Exists(pid) }))
	t.Cleanup(gateway.SwapProcessCmdlineReader(func(pid int) (string, bool) {
		if pid == ownPid {
			return "icebath -p alpha gateway run", true
		}
		return "icebath gateway run", true
	}))
	if _, _, ok, err := gateway.MultiplexerLivenessForProfile(alpha); err != nil || !ok {
		t.Fatalf("multiplexer liveness %v %v", ok, err)
	}
	payload := onePayload(t, alpha, bareCatalogEntry("telegram", "Telegram"))
	if payload.GetOr("gateway_running", nil) != true || payload.GetOr("state", nil) != "retrying" {
		t.Fatalf("payload %v", payload)
	}
}
