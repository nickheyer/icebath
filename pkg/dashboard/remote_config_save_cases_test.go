package dashboard

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/remote"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane/remoteplane"
)

// Dashboard client over a remote deployment served by a fresh plane
func remoteDashboard(t *testing.T) (*testClient, *configplane.Plane) {
	t.Helper()
	c := p2DashboardClient(t)
	p := remoteplane.Setup(t)
	config.ResetCache()
	return c, p
}

// Status and detail of a dashboard config save, the update_config call of the Python cases
func dashboardPut(t *testing.T, c *testClient, cfg *pyjson.Object) (int, string) {
	t.Helper()
	r := appJSON(t, c, "PUT", "/api/config", pyjson.ObjectOf("config", cfg))
	if r.Status == 200 {
		return 200, ""
	}
	obj, _ := r.JSON(t).(*pyjson.Object)
	if obj == nil {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	detail, _ := obj.Get("detail")
	s, _ := detail.(string)
	return r.Status, s
}

func readRemoteRaw(t *testing.T, p *configplane.Plane) {
	t.Helper()
	if _, err := config.ReadRawConfig(context.Background(), p.Home); err != nil {
		t.Fatal(err)
	}
}

// A lock that arrived after the last poll is refused by the server and answered 403 with the lock message
func TestDashboardSaveOfANewlyLockedKeyReturnsTheLockMessage(t *testing.T) {
	c, p := remoteDashboard(t)
	p.Upper = map[string]any{"display": map[string]any{"personality": "old"}}
	readRemoteRaw(t, p)
	p.Mu.Lock()
	p.UpperLocks = []map[string]any{{"path": "display.personality", "level": "tenant"}}
	p.Mu.Unlock()
	status, detail := dashboardPut(t, c, pyjson.ObjectOf("display", pyjson.ObjectOf("personality", "new")))
	if status != 403 || !strings.Contains(detail, "display.personality") || !strings.Contains(detail, "tenant") {
		t.Fatalf("%d %q", status, detail)
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	if len(p.Profile("default").Values) != 0 {
		t.Fatalf("values %v", p.Profile("default").Values)
	}
}

// A secret literal is refused client side with a 400 that never echoes the value
func TestDashboardSaveSecretLiteralIsA400(t *testing.T) {
	c, p := remoteDashboard(t)
	status, detail := dashboardPut(t, c, pyjson.ObjectOf("model", pyjson.ObjectOf("api_key", "sk-live-not-a-ref-1234567890")))
	if status != 400 || !strings.Contains(detail, "secret-shaped") || strings.Contains(detail, "sk-live") {
		t.Fatalf("%d %q", status, detail)
	}
	if n := len(p.Patches()); n != 0 {
		t.Fatalf("%d patches sent", n)
	}
}

// A plane failure that is no expected refusal stays an opaque 500
func TestDashboardUnexpectedConfigFailureStaysOpaque(t *testing.T) {
	c, p := remoteDashboard(t)
	readRemoteRaw(t, p)
	p.Mu.Lock()
	p.FailStatus = 500
	p.Mu.Unlock()
	status, detail := dashboardPut(t, c, pyjson.ObjectOf("display", pyjson.ObjectOf("personality", "new")))
	if status != 500 || detail != "Internal server error" {
		t.Fatalf("%d %q", status, detail)
	}
}

// The plane's own value refusal reaches the dashboard as a 400 with its reason and path
func TestDashboardSaveRefusedByPlaneValueCheckIsA400(t *testing.T) {
	c, p := remoteDashboard(t)
	readRemoteRaw(t, p)
	var value any = "leaf"
	for range 33 {
		value = []any{value}
	}
	status, detail := dashboardPut(t, c, pyjson.ObjectOf("display", pyjson.ObjectOf("custom", value)))
	if n := len(p.Patches()); n != 1 {
		t.Fatalf("%d patches sent", n)
	}
	if status != 400 || !strings.Contains(detail, "too_deep") || !strings.Contains(detail, "display") {
		t.Fatalf("%d %q", status, detail)
	}
}

// Write error of a plane PATCH answered with status and body
func planeWriteError(t *testing.T, status int, body map[string]any) error {
	t.Helper()
	p := remoteplane.Setup(t)
	config.ResetCache()
	readRemoteRaw(t, p)
	p.Mu.Lock()
	p.RefusePatch = &configplane.Refusal{Status: status, Body: body}
	p.Mu.Unlock()
	err := config.WriteConfigKey(context.Background(), filepath.Join(p.Home, "config.yaml"), "display.personality", "new")
	if err == nil {
		t.Fatalf("%d %v accepted", status, body)
	}
	return err
}

// Each plane refusal maps to its HTTP status or to none, keeping the plane code
func TestPlaneRefusalsMapToHttp(t *testing.T) {
	for _, c := range []struct {
		status   int
		code     string
		expected int
	}{
		{400, "config_value_invalid", 400}, {400, "config_path_invalid", 400}, {400, "config_path_reserved", 400},
		{400, "config_secret_literal", 400}, {413, "config_level_too_large", 413}, {413, "config_body_too_large", 413},
		{400, "config_request_invalid", 0}, {500, "internal", 0},
	} {
		t.Run(c.code, func(t *testing.T) {
			err := planeWriteError(t, c.status, map[string]any{"error": c.code, "message": "refused", "path": "a.b"})
			got := 0
			if http := configRefusalHTTP(err); http != nil {
				got = http.StatusCode
			}
			var werr *config.ConfigWriteError
			if got != c.expected || !errors.As(err, &werr) || werr.Code != c.code {
				t.Fatalf("%d %s -> %d %v", c.status, c.code, got, err)
			}
		})
	}
}

// A group lock refusal from the server maps to the locked error and the dashboard 403
func TestPlaneGroupLockRefusalMapsToLockedErrorAnd403(t *testing.T) {
	err := planeWriteError(t, 403, map[string]any{"error": "config_key_locked", "path": "terminal.backend", "lockedBy": "group",
		"lockedByGroupId": "g-sec", "message": "terminal.backend is locked by group g-sec"})
	var locked *config.ConfigLockedError
	if !errors.As(err, &locked) || !reflect.DeepEqual([]string{locked.Path, locked.LockedBy}, []string{"terminal.backend", "group"}) {
		t.Fatalf("err %v", err)
	}
	http := configRefusalHTTP(err)
	if http == nil || http.StatusCode != 403 || !strings.Contains(fmt.Sprint(http.Detail), "locked by group") {
		t.Fatalf("http %+v", http)
	}
}

var oldRoute = map[string]any{"default": "old", "provider": "custom:old", "base_url": "https://old.example/v1",
	"api_key": "${OLD_KEY}", "key_env": "OLD_KEY", "api_key_env": "OLD_KEY", "context_length": 123456}

// Dashboard main assignment to a custom endpoint with the code skew guard and the preparation step faked
func dashboardSwitch(t *testing.T, url string) (any, error) {
	t.Helper()
	fakeCodeSkew(t, false)
	realPrepare := prepareMainAssignment
	t.Cleanup(func() { prepareMainAssignment = realPrepare })
	prepareMainAssignment = func(context.Context, map[string]any, string, string, string, string) (*provider.PreparedMainAssignment, error) {
		return &provider.PreparedMainAssignment{BaseURL: url, Result: provider.ModelSwitchResult{Success: true, NewModel: "new",
			TargetProvider: "custom", ProviderChanged: true, BaseURL: url, APIMode: "chat_completions", IsGlobal: true}}, nil
	}
	body, set := modelRouteBody(t, modelAssignmentModel, map[string]any{"scope": "main", "provider": "custom", "model": "new",
		"base_url": url, "confirm_expensive_model": true})
	return setModelAssignment(context.Background(), body, set, "")
}

func switchOK(t *testing.T, got any, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if pyjson.Plain(got).(map[string]any)["ok"] != true {
		t.Fatalf("result %v", pyjson.Plain(got))
	}
}

func rawModel(t *testing.T, p *configplane.Plane) map[string]any {
	t.Helper()
	raw, err := config.ReadUserConfigRaw(context.Background(), filepath.Join(p.Home, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	model, _ := raw["model"].(map[string]any)
	return model
}

// The whole document save of a dashboard switch masks the endpoint bound keys it clears
func TestDashboardModelSwitchMasksInheritedEndpointCredentials(t *testing.T) {
	_, p := remoteDashboard(t)
	url := "https://new.example/v1"
	p.Upper = map[string]any{"model": config.Clone(oldRoute), "providers": map[string]any{"known-new": map[string]any{"base_url": url}}}
	got, err := dashboardSwitch(t, url)
	switchOK(t, got, err)
	model := rawModel(t, p)
	if model["base_url"] != url {
		t.Fatalf("model %v", model)
	}
	for _, key := range []string{"api_key", "key_env", "api_key_env", "context_length"} {
		if model[key] != nil {
			t.Fatalf("model.%s %v", key, model[key])
		}
	}
}

// A lock on one route key refuses the whole dashboard assignment and sends nothing
func TestDashboardModelSwitchRefusesWholeAssignmentOnALockedRouteKey(t *testing.T) {
	_, p := remoteDashboard(t)
	url := "https://new.example/v1"
	p.Upper = map[string]any{"model": config.Clone(oldRoute), "providers": map[string]any{"known-new": map[string]any{"base_url": url}}}
	p.UpperLocks = []map[string]any{{"path": "model.base_url", "level": "tenant"}}
	_, err := dashboardSwitch(t, url)
	var http *HTTPException
	if !errors.As(err, &http) || (http.StatusCode != 403 && http.StatusCode != 409) {
		t.Fatalf("err %v", err)
	}
	if n := len(p.Patches()); n != 0 {
		t.Fatalf("%d patches", n)
	}
}

// The target provider's key env replaces the inline key and the inherited key stays masked through the credential step
func TestDashboardModelSwitchKeepsMaskWhenTargetProviderCarriesKeyEnv(t *testing.T) {
	_, p := remoteDashboard(t)
	url := "https://new.example/v1"
	t.Setenv("OLD_KEY", "old-synthetic-key")
	t.Setenv("NEW_KEY", "new-synthetic-key")
	p.Upper = map[string]any{"model": config.Clone(oldRoute), "providers": map[string]any{"custom": map[string]any{"base_url": url, "key_env": "NEW_KEY"}}}
	got, err := dashboardSwitch(t, url)
	switchOK(t, got, err)
	patches := p.Patches()
	body := patches[len(patches)-1].Body
	set, _ := body["set"].(map[string]any)
	unset, _ := body["unset"].([]any)
	if v, has := set["model.api_key"]; !has || v != nil {
		t.Fatalf("body %v", body)
	}
	for _, u := range unset {
		if u == "model.api_key" {
			t.Fatalf("body %v", body)
		}
	}
	model := rawModel(t, p)
	if model["base_url"] != url || model["api_key"] != nil || model["key_env"] != "NEW_KEY" {
		t.Fatalf("model %v", model)
	}
	route, err := provider.ResolveRuntimeProvider(context.Background(), p.Home, "custom", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if route.BaseURL != url || route.APIKey != "new-synthetic-key" {
		t.Fatalf("route %+v", route)
	}
}

// A lock on providers, up front or landing before the save, refuses both the entry and the route naming it
func TestCustomEndpointMakeDefaultRefusesWhenItsProviderEntryIsLocked(t *testing.T) {
	for _, schedule := range []string{"locked", "late-poll", "late-retry"} {
		t.Run(schedule, func(t *testing.T) {
			_, p := remoteDashboard(t)
			p.Upper = map[string]any{"model": map[string]any{"default": "old-model", "provider": "custom:old", "base_url": "https://old.example/v1"},
				"providers": map[string]any{"custom:old": map[string]any{"name": "old", "base_url": "https://old.example/v1", "model": "old-model"}}}
			lock := []map[string]any{{"path": "providers", "level": "tenant"}}
			if schedule == "locked" {
				p.UpperLocks = lock
			}
			realRecord, realValidate, realSave := recordCustomEndpointSetup, validatedMainModelSelection, webSaveConfig
			t.Cleanup(func() {
				recordCustomEndpointSetup, validatedMainModelSelection, webSaveConfig = realRecord, realValidate, realSave
			})
			recordCustomEndpointSetup = func(context.Context, string) {}
			validatedMainModelSelection = func(_ context.Context, _ map[string]any, providerName, model, baseURL string) (provider.ModelSwitchResult, error) {
				return provider.ModelSwitchResult{Success: true, NewModel: model, TargetProvider: providerName, ProviderChanged: true,
					BaseURL: baseURL, APIMode: "chat_completions", IsGlobal: true}, nil
			}
			if schedule != "locked" {
				webSaveConfig = func(ctx context.Context, cfg map[string]any, opts config.SaveOptions) error {
					p.Mu.Lock()
					p.UpperLocks = lock
					if schedule == "late-retry" {
						p.Profile("default").Version++
					}
					p.Mu.Unlock()
					if schedule == "late-poll" {
						remote.GetRemoteBackend().(interface{ PollAll() }).PollAll()
					}
					return realSave(ctx, cfg, opts)
				}
			}
			_, err := upsertCustomEndpoint(context.Background(), "", pyjson.ObjectOf("id", "new-probe", "name", "new-probe",
				"base_url", "https://new.example/v1", "model", "new-model", "make_default", true))
			status := 0
			var http *HTTPException
			if errors.As(err, &http) {
				status = http.StatusCode
			} else if refusal := configRefusalHTTP(err); refusal != nil {
				status = refusal.StatusCode
			}
			if status != 403 {
				t.Fatalf("err %v", err)
			}
			p.Mu.Lock()
			model, has := p.Profile("default").Values["model"]
			p.Mu.Unlock()
			if m, _ := model.(map[string]any); has && model != nil && len(m) != 0 {
				t.Fatalf("route saved %v", model)
			}
		})
	}
}
