package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Session header of the application the hermes TestClient sends
func sessionHeaders() []string { return []string{sessionHeaderName, SessionToken()} }

// JSON request through the test client with the session header
func appJSON(t *testing.T, c *testClient, method, target string, payload any) *testResponse {
	t.Helper()
	body, err := pyjson.Dumps(payload)
	if err != nil {
		t.Fatal(err)
	}
	return c.request(method, target, []byte(body), append(sessionHeaders(), "content-type", "application/json")...)
}

// Object body of a response
func appObject(t *testing.T, r *testResponse) *pyjson.Object {
	t.Helper()
	if r.Status != 200 {
		t.Fatalf("status %d body %s err %v", r.Status, r.Body, r.AppErr)
	}
	obj, ok := r.JSON(t).(*pyjson.Object)
	if !ok {
		t.Fatalf("body %s is not an object", r.Body)
	}
	return obj
}

// GET /api/env rows with the scoped .env holding exactly envOnDisk
func envRows(t *testing.T, envOnDisk string) *pyjson.Object {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	if err := os.WriteFile(filepath.Join(home.Root(), ".env"), []byte(envOnDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	return appObject(t, c.get("/api/env", sessionHeaders()...))
}

func TestUnknownEnvKeySurfacesAsCustom(t *testing.T) {
	rows := envRows(t, "MY_CUSTOM_THING=s3cret-value\n")
	raw, ok := rows.Get("MY_CUSTOM_THING")
	if !ok {
		t.Fatal("unknown .env key not surfaced by /api/env")
	}
	row := raw.(*pyjson.Object)
	if row.GetOr("custom", nil) != true || row.GetOr("category", nil) != "custom" || row.GetOr("is_set", nil) != true {
		t.Fatalf("row %v", row)
	}
}

func TestCustomKeyIsPasswordMasked(t *testing.T) {
	rows := envRows(t, "MY_CUSTOM_THING=s3cret-value\n")
	raw, _ := rows.Get("MY_CUSTOM_THING")
	row := raw.(*pyjson.Object)
	if row.GetOr("is_password", nil) != true || row.GetOr("redacted_value", nil) == "s3cret-value" {
		t.Fatalf("row %v", row)
	}
	if strings.Contains(pyrepr.Repr(row), "s3cret-value") {
		t.Fatal("raw value rides in the listing payload")
	}
}

// Saved raw config of the test home
func rawHomeConfig(t *testing.T) map[string]any {
	t.Helper()
	raw, err := config.ReadRawConfig(context.Background(), home.Root())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRoundTripPreservesSchemaInvisibleNestedKeys(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	if err := config.SaveConfig(context.Background(), home.Root(), map[string]any{"agent": map[string]any{
		"max_turns": 50, "x_dashboard_invisible_test_key": map[string]any{"nested": "value"}}}, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	webConfig := appObject(t, c.get("/api/config", sessionHeaders()...))
	agent, ok := webConfig.GetOr("agent", nil).(*pyjson.Object)
	if !ok {
		agent = pyjson.NewObject()
		webConfig.Set("agent", agent)
	}
	agent.Set("max_turns", int64(75))
	agent.Delete("x_dashboard_invisible_test_key")
	resp := appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", webConfig))
	if resp.Status != 200 {
		t.Fatalf("status %d body %s err %v", resp.Status, resp.Body, resp.AppErr)
	}
	onDisk, _ := rawHomeConfig(t)["agent"].(map[string]any)
	if !pyjson.Equal(configJSON(onDisk["max_turns"]), int64(75)) && !pyjson.Equal(onDisk["max_turns"], 75) {
		t.Fatalf("max_turns %v", onDisk["max_turns"])
	}
	if !pyjson.Equal(configJSON(onDisk["x_dashboard_invisible_test_key"]), pyjson.ObjectOf("nested", "value")) {
		t.Fatalf("shallow merge regression: %v", onDisk["x_dashboard_invisible_test_key"])
	}
}

// Nested value of a dotted path in a JSON object, nil when any step is missing
func nestedJSON(obj any, path string) any {
	cur := obj
	for _, p := range strings.Split(path, ".") {
		o, ok := cur.(*pyjson.Object)
		if !ok || o == nil {
			return nil
		}
		cur = o.GetOr(p, nil)
	}
	return cur
}

func TestSchemaTypesMatchConfigValues(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	cfg := appObject(t, c.get("/api/config", sessionHeaders()...))
	schema := appObject(t, c.get("/api/config/schema", sessionHeaders()...)).GetOr("fields", nil).(*pyjson.Object)
	var mismatches []string
	for _, key := range schema.Keys {
		val := nestedJSON(cfg, key)
		if val == nil {
			continue
		}
		expected := schema.Vals[key].(*pyjson.Object).GetOr("type", "").(string)
		_, isStr := val.(string)
		_, isBool := val.(bool)
		_, isList := val.([]any)
		_, isInt := pyrepr.Int(val)
		_, isFloat := val.(float64)
		switch {
		case (expected == "string" || expected == "select") && !isStr:
			mismatches = append(mismatches, key+": expected str, got "+pyrepr.TypeName(val))
		case expected == "number" && !(isInt || isFloat || isBool):
			mismatches = append(mismatches, key+": expected number, got "+pyrepr.TypeName(val))
		case expected == "boolean" && !isBool:
			mismatches = append(mismatches, key+": expected bool, got "+pyrepr.TypeName(val))
		case expected == "list" && !isList:
			mismatches = append(mismatches, key+": expected list, got "+pyrepr.TypeName(val))
		}
	}
	if len(mismatches) > 0 {
		t.Fatalf("Type mismatches:\n%s", strings.Join(mismatches, "\n"))
	}
}

func TestDesktopTerminalFontRoundTripPreservesTerminalConfig(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	webConfig := appObject(t, c.get("/api/config", sessionHeaders()...))
	terminal, ok := webConfig.GetOr("terminal", nil).(*pyjson.Object)
	if !ok {
		terminal = pyjson.NewObject()
		webConfig.Set("terminal", terminal)
	}
	before := terminal.Copy()
	terminal.Set("font_family", "MesloLGS NF")
	resp := appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", webConfig))
	if resp.Status != 200 {
		t.Fatalf("status %d body %s err %v", resp.Status, resp.Body, resp.AppErr)
	}
	loaded, err := webLoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	persisted := configJSON(loaded["terminal"]).(*pyjson.Object)
	if persisted.GetOr("font_family", nil) != "MesloLGS NF" {
		t.Fatalf("font_family %v", persisted.GetOr("font_family", nil))
	}
	for _, key := range before.Keys {
		if key != "font_family" && !pyjson.Equal(persisted.GetOr(key, nil), before.Vals[key]) {
			t.Fatalf("terminal.%s %v, was %v", key, persisted.GetOr(key, nil), before.Vals[key])
		}
	}
	reloaded := appObject(t, c.get("/api/config", sessionHeaders()...))
	if nestedJSON(reloaded, "terminal.font_family") != "MesloLGS NF" {
		t.Fatalf("reloaded %v", nestedJSON(reloaded, "terminal.font_family"))
	}
}
