package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type deviceWireRow struct {
	path string
	data map[string]any
	at   time.Time
}

// Virtual clock whose sleeps advance time instantly
func fakeDeviceClock(t *testing.T) {
	t.Helper()
	var mu sync.Mutex
	now := time.Unix(1_000_000, 0)
	deviceNow = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	deviceSleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
		return nil
	}
	t.Cleanup(func() { deviceNow, deviceSleep = time.Now, sleepCtx })
}

// RFC 8628 fixture authorization server with the eval's failure modes
func deviceFixture(t *testing.T, mode string) (string, func() []deviceWireRow) {
	t.Helper()
	var mu sync.Mutex
	var wire []deviceWireRow
	var base string
	reply := func(w http.ResponseWriter, status int, v any, h map[string]string) {
		for k, val := range h {
			w.Header().Set(k, val)
		}
		writeJSON(w, status, v)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch {
			case r.URL.Path == "/mcp":
				if r.Header.Get("Authorization") == "Bearer fixture-access" {
					reply(w, 200, map[string]any{"authenticated": true}, nil)
					return
				}
				reply(w, 401, map[string]any{}, map[string]string{"WWW-Authenticate": `Bearer resource_metadata="` + base + `/prm"`})
			case r.URL.Path == "/prm" || strings.Contains(r.URL.Path, "oauth-protected-resource"):
				resource, servers := base+"/mcp", []string{base}
				if mode == "resource" {
					resource = base + "/wrong"
				}
				if mode == "multi_issuer" {
					servers = []string{base + "/wrong", base}
				}
				reply(w, 200, map[string]any{"resource": resource, "authorization_servers": servers}, nil)
			case strings.Contains(r.URL.Path, "oauth-authorization-server"):
				md := map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token",
					"response_types_supported": []string{"code"}, "code_challenge_methods_supported": []string{"S256"},
					"grant_types_supported": []string{deviceCodeGrant, "refresh_token"}}
				if mode == "issuer" {
					md["issuer"] = base + "/wrong"
				}
				if mode != "unsupported" {
					md["device_authorization_endpoint"] = base + "/device"
				}
				if mode != "preregistered" {
					md["registration_endpoint"] = base + "/register"
				} else {
					delete(md, "authorization_endpoint")
				}
				reply(w, 200, md, nil)
			default:
				reply(w, 404, map[string]any{}, nil)
			}
			return
		}
		raw, _ := io.ReadAll(r.Body)
		data := map[string]any{}
		if strings.Contains(r.Header.Get("Content-Type"), "json") {
			json.Unmarshal(raw, &data)
		} else {
			q, _ := url.ParseQuery(string(raw))
			for k, v := range q {
				data[k] = v[0]
			}
		}
		mu.Lock()
		wire = append(wire, deviceWireRow{r.URL.Path, data, deviceNow()})
		polls := 0
		for _, row := range wire {
			if row.path == "/token" {
				polls++
			}
		}
		mu.Unlock()
		switch r.URL.Path {
		case "/mcp":
			if r.Header.Get("Authorization") != "Bearer fixture-access" {
				reply(w, 401, map[string]any{}, map[string]string{"WWW-Authenticate": `Bearer resource_metadata="` + base + `/prm"`})
				return
			}
			id, hasID := data["id"]
			if !hasID {
				reply(w, 202, map[string]any{}, nil)
				return
			}
			params, _ := data["params"].(map[string]any)
			results := map[string]any{"initialize": map[string]any{"protocolVersion": params["protocolVersion"], "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fixture", "version": "1"}},
				"tools/list": map[string]any{"tools": []any{map[string]any{"name": "fixture_echo", "description": "Fixture tool",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}}}}
			method, _ := data["method"].(string)
			result, ok := results[method]
			if !ok {
				result = map[string]any{}
			}
			reply(w, 200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result}, nil)
		case "/register":
			grants, _ := data["grant_types"].([]any)
			if len(grants) == 0 || grants[0] != deviceCodeGrant {
				reply(w, 400, map[string]any{"error": "invalid_client_metadata"}, nil)
				return
			}
			data["client_id"] = "fixture-client"
			reply(w, 201, data, nil)
		case "/device":
			expires := 30.0
			if mode == "expiry" {
				expires = 0.1
			}
			reply(w, 200, map[string]any{"device_code": "fixture-device-secret", "user_code": "TEST-CODE",
				"verification_uri": base + "/verify", "interval": 0.01, "expires_in": expires}, nil)
		case "/token":
			switch {
			case mode == "preregistered" && data["client_secret"] != "fixture-client-secret":
				reply(w, 401, map[string]any{"error": "invalid_client"}, nil)
			case mode == "malformed":
				reply(w, 200, map[string]any{"access_token": map[string]any{"secret": "fixture-device-secret"}}, nil)
			case mode == "denied" || mode == "expiry":
				code := "authorization_pending"
				if mode == "denied" {
					code = "access_denied"
				}
				reply(w, 400, map[string]any{"error": code, "error_description": "fixture-device-secret MUST NOT BE PRINTED"}, nil)
			case polls <= 2 && mode == "success":
				code := "slow_down"
				if polls == 1 {
					code = "authorization_pending"
				}
				reply(w, 400, map[string]any{"error": code}, nil)
			default:
				reply(w, 200, map[string]any{"access_token": "fixture-access", "refresh_token": "fixture-refresh",
					"token_type": "Bearer", "expires_in": 3600}, nil)
			}
		default:
			reply(w, 404, map[string]any{}, nil)
		}
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	return base, func() []deviceWireRow { mu.Lock(); defer mu.Unlock(); return append([]deviceWireRow(nil), wire...) }
}

func deviceOAuthConfig(mode string) map[string]any {
	cfg := map[string]any{"flow": "device", "cimd": false, "scope": "fixture.read", "timeout": 15}
	if mode == "preregistered" {
		cfg["client_id"], cfg["client_secret"] = "fixture-client", "fixture-client-secret"
	}
	return cfg
}

func TestDeviceLoginRegistersAuthorizesAndPersists(t *testing.T) {
	for _, mode := range []string{"success", "preregistered", "multi_issuer"} {
		t.Run(mode, func(t *testing.T) {
			dir := freshManager(t)
			fakeDeviceClock(t)
			base, wire := deviceFixture(t, mode)
			ctx := context.Background()
			if err := LoginDevice(ctx, "fixture", base+"/mcp", deviceOAuthConfig(mode), HTTPOptions{}); err != nil {
				t.Fatalf("login %v", err)
			}
			if !strings.Contains(testStderr.String(), "TEST-CODE") {
				t.Fatalf("instructions %q", testStderr.String())
			}
			tokenPath := writeTokenFile(t, dir, "probe", "{}")
			tokenPath = strings.TrimSuffix(tokenPath, "probe") + "fixture.json"
			tokens := readJSONFile(t, tokenPath)
			tokens["expires_at"] = float64(time.Now().Unix() - 60)
			writeTokenFile(t, dir, "fixture.json", tokens)
			p, err := getManager().getOrBuildProvider(SuppressInteractiveOAuth(ctx), "fixture", base+"/mcp", deviceOAuthConfig(mode), HTTPOptions{})
			if err != nil {
				t.Fatal(err)
			}
			resp, err := authedDo(ctx, p, http.DefaultTransport, http.MethodGet, base+"/mcp")
			if err != nil || resp.StatusCode != 200 {
				t.Fatalf("refreshed request %v %v", resp, err)
			}
			var polls []deviceWireRow
			refreshed := false
			for _, row := range wire() {
				if row.path == "/token" {
					if row.data["grant_type"] == "refresh_token" {
						refreshed = true
						continue
					}
					polls = append(polls, row)
				}
				if row.path == "/register" && mode == "preregistered" {
					t.Fatal("preregistered client registered")
				}
			}
			if !refreshed {
				t.Fatal("no refresh grant after login")
			}
			for _, row := range polls {
				if row.data["grant_type"] != deviceCodeGrant || !strings.HasSuffix(row.data["resource"].(string), "/mcp") {
					t.Fatalf("poll %v", row.data)
				}
			}
			if mode == "success" && polls[2].at.Sub(polls[1].at) < 5*time.Second {
				t.Fatalf("slow_down ignored %v", polls[2].at.Sub(polls[1].at))
			}
		})
	}
}

func TestDeviceLoginFailureDoesNotPersistOrDiscloseCredentials(t *testing.T) {
	for _, mode := range []string{"denied", "expiry", "unsupported", "issuer", "resource", "malformed", "persistence"} {
		t.Run(mode, func(t *testing.T) {
			dir := freshManager(t)
			fakeDeviceClock(t)
			base, _ := deviceFixture(t, mode)
			previous := map[string]string{}
			if mode == "persistence" {
				previous = map[string]string{"fixture.json": `{"access_token":"old-fixture","token_type":"Bearer"}`,
					"fixture.client.json": `{"client_id":"old-client"}`, "fixture.meta.json": `{"issuer":"https://old.example"}`}
				for name, v := range previous {
					writeTokenFile(t, dir, name, v)
				}
				deviceSetTokens = func(*TokenStorage, *OAuthToken) error { return errors.New("fixture disk failure") }
				t.Cleanup(func() { deviceSetTokens = (*TokenStorage).SetTokens })
			}
			err := LoginDevice(context.Background(), "fixture", base+"/mcp", deviceOAuthConfig(mode), HTTPOptions{})
			if err == nil {
				t.Fatal("login succeeded")
			}
			if strings.Contains(err.Error()+testStderr.String(), "fixture-device-secret") {
				t.Fatalf("credential disclosed %v", err)
			}
			for name, v := range previous {
				if raw, _ := os.ReadFile(dir + "/mcp-tokens/" + name); string(raw) != v {
					t.Fatalf("%s not restored %q", name, raw)
				}
			}
			if _, statErr := os.Stat(dir + "/mcp-tokens/fixture.json"); (statErr == nil) != (mode == "persistence") {
				t.Fatalf("token persisted %v", statErr)
			}
		})
	}
}
