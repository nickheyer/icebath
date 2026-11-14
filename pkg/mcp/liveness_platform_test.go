package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Registers a declaration for the server until the test ends
func registerDeclaration(t *testing.T, server string, decl *tools.PlatformDeclaration) {
	t.Helper()
	tools.RegisterPlatformDeclaration(server, decl)
	t.Cleanup(func() { tools.UnregisterPlatformDeclaration(server) })
}

// Declaration of an app whose executable presence file is location
func executableDeclaration(t *testing.T, name, location string) *tools.PlatformDeclaration {
	t.Helper()
	decl, err := tools.ParsePlatformDeclaration(name, map[string]any{hostinfo.OSFamily(): map[string]any{"presence": "executable", "location": location}}, map[string]any{"app": true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	return decl
}

// Example App declaration over an existing presence file
func exampleDeclaration(t *testing.T, dir string) *tools.PlatformDeclaration {
	t.Helper()
	executable := filepath.Join(dir, "example-app")
	if err := os.WriteFile(executable, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	return executableDeclaration(t, "Example App", executable)
}

// Answers every plugin liveness lookup with raw until the test ends
func fakePortableLiveness(t *testing.T, raw map[string]any) {
	t.Helper()
	prev := portableLiveness
	portableLiveness = func(string) map[string]any { return raw }
	t.Cleanup(func() { portableLiveness = prev })
}

// Records every vault redaction registration until the test ends
func recordVaultRedactions(t *testing.T) *[]string {
	t.Helper()
	var calls []string
	prev := registerVaultRedaction
	registerVaultRedaction = func(_ context.Context, v string) { calls = append(calls, v) }
	t.Cleanup(func() { registerVaultRedaction = prev })
	return &calls
}

// Server json liveness reading url, token and pid from path
func serverJSONLiveness(path string) map[string]any {
	return map[string]any{"kind": "server_json", "path": path, "fields": map[string]any{"url": "http", "token": "token", "pid": "pid"}}
}

func writeRuntime(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLivenessInvalidRegisteredDeclarationDegradesToStatic(t *testing.T) {
	fakePortableLiveness(t, map[string]any{"kind": "server_json"})
	if got := livenessFor(context.Background(), "example-server").Kind; got != "static" {
		t.Fatal(got)
	}
}

func TestLivenessLiveEndpointReloadsFileAndRegistersTokenBeforeUse(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	runtime := filepath.Join(dir, "server.json")
	registerDeclaration(t, "example-server", exampleDeclaration(t, dir))
	fakePortableLiveness(t, serverJSONLiveness(runtime))
	calls := recordVaultRedactions(t)
	logs := captureLogs(t)
	writeRuntime(t, runtime, map[string]any{"http": "http://127.0.0.1:1111", "token": "first-secret", "pid": os.Getpid()})
	url1, h1, ok1, err1 := liveEndpoint(context.Background(), "example-server")
	writeRuntime(t, runtime, map[string]any{"http": "http://127.0.0.1:2222", "token": "second-secret", "pid": os.Getpid()})
	url2, h2, ok2, err2 := liveEndpoint(context.Background(), "example-server")
	if !ok1 || err1 != nil || url1 != "http://127.0.0.1:1111/mcp" || fmt.Sprint(h1) != "map[Authorization:Bearer first-secret]" {
		t.Fatalf("first %q %v %v %v", url1, h1, ok1, err1)
	}
	if !ok2 || err2 != nil || url2 != "http://127.0.0.1:2222/mcp" || fmt.Sprint(h2) != "map[Authorization:Bearer second-secret]" {
		t.Fatalf("second %q %v %v %v", url2, h2, ok2, err2)
	}
	if strings.Join(*calls, ",") != "first-secret,second-secret" {
		t.Fatalf("registered %q", *calls)
	}
	if text := logs.text(); strings.Contains(text, "first-secret") || strings.Contains(text, "second-secret") {
		t.Fatalf("token logged %q", text)
	}
}

func TestLivenessRuntimeFileWithoutTokenConnectsWithoutAuthorization(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	runtime := filepath.Join(dir, "server.json")
	registerDeclaration(t, "example-server", exampleDeclaration(t, dir))
	fakePortableLiveness(t, serverJSONLiveness(runtime))
	calls := recordVaultRedactions(t)
	writeRuntime(t, runtime, map[string]any{"http": "http://127.0.0.1:3333", "pid": os.Getpid()})
	url, headers, ok, err := liveEndpoint(context.Background(), "example-server")
	if !ok || err != nil || url != "http://127.0.0.1:3333/mcp" {
		t.Fatalf("%q %v %v", url, ok, err)
	}
	if _, has := headers["Authorization"]; has || len(*calls) != 0 {
		t.Fatalf("headers %v registered %q", headers, *calls)
	}
}

func TestLivenessMissingRuntimeFileNeverFallsBack(t *testing.T) {
	isolateHome(t)
	dir := t.TempDir()
	registerDeclaration(t, "example-server", exampleDeclaration(t, dir))
	fakePortableLiveness(t, serverJSONLiveness(filepath.Join(dir, "missing.json")))
	_, _, _, err := liveEndpoint(context.Background(), "example-server")
	var unavailable *LiveEndpointUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestLivenessHydratedErrorShapeForRegisteredDeclaration(t *testing.T) {
	_, env := managerHome(t)
	registerDeclaration(t, "example-server", exampleDeclaration(t, t.TempDir()))
	fakePortableLiveness(t, map[string]any{"kind": "static"})
	srv, errText := acquireCallServer(context.Background(), env, "example-server", 0)
	var payload map[string]any
	if err := json.Unmarshal([]byte(errText), &payload); err != nil || srv != nil {
		t.Fatalf("%v %q", err, errText)
	}
	app, _ := payload["app"].(map[string]any)
	if payload["server"] != "example-server" || payload["state"] != "icebath_not_connected" || app["name"] != "Example App" ||
		payload["user_action"] == nil || payload["user_action"] == "" || payload["retry"] != "after_user_action" {
		t.Fatalf("payload %v", payload)
	}
}

func TestLivenessConnectedInteractiveSessionServerIsOfferableFromAServiceSession(t *testing.T) {
	registerDeclaration(t, "example-server", exampleDeclaration(t, t.TempDir()))
	fakePortableLiveness(t, map[string]any{"kind": "interactive_session"})
	t.Cleanup(hostinfo.PinInteractiveSession(false))
	if !declaredAppOfferable("example-server") {
		t.Fatal("a connected interactive session server must stay offerable from a service session")
	}
}

// App platform whose local probe answers with a fixed verdict
type fixedProbePlatform struct {
	appPlatform
	probe AppProbe
}

func (p fixedProbePlatform) ProbeLocal(AppDefinition) AppProbe { return p.probe }

func TestLivenessRunningAppWithLiveEndpointReportsTheMissingConnection(t *testing.T) {
	dir := t.TempDir()
	running := true
	prev := platformApps
	platformApps = func() appPlatform {
		return fixedProbePlatform{prev(), AppProbe{Running: &running, EndpointPresent: true}}
	}
	t.Cleanup(func() { platformApps = prev })
	fakePortableLiveness(t, map[string]any{"kind": "server_json", "path": filepath.Join(dir, "runtime.json")})
	registerDeclaration(t, "example-server", exampleDeclaration(t, dir))
	current := livenessStatus(context.Background(), "example-server")
	if current == nil || current.State != "icebath_not_connected" {
		t.Fatalf("status %+v", current)
	}
	decl := platformApps().Declaration("example-server")
	sentence := describeUnavailable(decl, current.Availability, current.State, "")
	if !strings.Contains(sentence, "MCP connection is missing") || strings.Contains(sentence, "is not running") || !strings.HasPrefix(current.UserAction, "Reconnect") {
		t.Fatalf("%q %q", sentence, current.UserAction)
	}
}

func TestLivenessStaticLivenessCannotClaimTheAppIsNotRunning(t *testing.T) {
	fakePortableLiveness(t, map[string]any{"kind": "static"})
	registerDeclaration(t, "example-server", exampleDeclaration(t, t.TempDir()))
	current := livenessStatus(context.Background(), "example-server")
	if current == nil || current.State != "icebath_not_connected" {
		t.Fatalf("status %+v", current)
	}
	sentence := describeUnavailable(platformApps().Declaration("example-server"), current.Availability, current.State, "")
	if !strings.Contains(sentence, "MCP connection is missing") || strings.Contains(sentence, "is not running") {
		t.Fatal(sentence)
	}
}

func TestLivenessGPURequirementHidesAConnectedServerAndSaysWhichGPU(t *testing.T) {
	t.Cleanup(hostinfo.PinGPUClass("intel"))
	fakePortableLiveness(t, nil)
	decl, err := tools.ParsePlatformDeclaration("RTX Tool", nil, map[string]any{"gpu": "nvidia"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	registerDeclaration(t, "example-server", decl)
	offerable := declaredAppOfferable("example-server")
	current := livenessStatus(context.Background(), "example-server")
	if offerable || current == nil || current.State != "unsupported_gpu" || current.Retry != "never_here" {
		t.Fatalf("offerable %v status %+v", offerable, current)
	}
	sentence := describeUnavailable(platformApps().Declaration("example-server"), current.Availability, current.State, "")
	if !strings.Contains(sentence, "an NVIDIA GPU") || strings.Contains(sentence, "Install") {
		t.Fatal(sentence)
	}
}

func TestLivenessDescribePrefersThePluginTitleOverTheServerSlug(t *testing.T) {
	registerDeclaration(t, "example-server", exampleDeclaration(t, t.TempDir()))
	sentence := describeUnavailable(platformApps().Declaration("example-server"), AppAvailability{}, "icebath_not_connected", "Example Tools")
	if !strings.HasPrefix(sentence, "Example Tools") || strings.Contains(sentence, "Example App") || !strings.Contains(sentence, "MCP connection is missing") {
		t.Fatal(sentence)
	}
}

// Hidden MCP tool whose check always fails, deregistered when the test ends
func registerHiddenTool(t *testing.T, name, toolset string) {
	t.Helper()
	def := testToolDef(name, toolset)
	def.Schema = map[string]any{"name": name, "description": "fixture", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
	def.Check = func(*tools.Env) bool { return false }
	tools.Default.Register(def)
	t.Cleanup(func() { _ = tools.Default.Deregister(name, "", "") })
}

func TestHiddenDeclaredServerAppearsInListingAndEmptySummary(t *testing.T) {
	_, env := managerHome(t)
	server, toolName := "example-hidden", "mcp__example-hidden__inspect"
	registerDeclaration(t, server, executableDeclaration(t, "Example App", filepath.Join(t.TempDir(), "missing-app")))
	fakePortableLiveness(t, map[string]any{"kind": "static"})
	registerHiddenTool(t, toolName, "mcp-"+server)
	ctx := context.Background()
	first, firstForm := tools.BuildCatalogListingWithForm(ctx, env, nil, 4000)
	second, secondForm := tools.BuildCatalogListingWithForm(ctx, env, nil, 4000)
	cfg := &tools.ToolSearchConfig{Enabled: "on", ThresholdPct: 50, SearchDefaultLimit: 10, MaxSearchLimit: 50, DeferTools: map[string]bool{toolName: true}}
	raw := tools.DispatchToolSearch(ctx, env, map[string]any{"queries": []any{"inspect"}}, nil, cfg, nil)
	if first != second || firstForm != secondForm || firstForm != "full" || !strings.Contains(first, "example-hidden") {
		t.Fatalf("listing %q %q / %q %q", first, firstForm, second, secondForm)
	}
	var result struct {
		Results []struct {
			AvailableSources []struct {
				Name        string `json:"name"`
				ToolCount   int    `json:"tool_count"`
				Unavailable string `json:"unavailable"`
			} `json:"available_sources"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || len(result.Results) == 0 || len(result.Results[0].AvailableSources) == 0 {
		t.Fatalf("%v %s", err, raw)
	}
	source := result.Results[0].AvailableSources[0]
	if source.Name != server || source.ToolCount != 1 || !strings.Contains(source.Unavailable, "Example App is not installed.") {
		t.Fatalf("source %+v", source)
	}
}

func TestHiddenUndeclaredServerIsAbsent(t *testing.T) {
	_, env := managerHome(t)
	registerHiddenTool(t, "mcp__plain-hidden__inspect", "mcp-plain-hidden")
	listing, form := tools.BuildCatalogListingWithForm(context.Background(), env, nil, 4000)
	if listing != "" || form != "none" {
		t.Fatalf("%q %q", listing, form)
	}
}

func TestAppGateCheckFnComposesConnectionAndRegisteredRequirement(t *testing.T) {
	for _, connection := range []string{"live", "recycled", "lazy", "absent"} {
		t.Run(connection, func(t *testing.T) {
			resetLedgers(t)
			tools.ClearPlatformDeclarations()
			t.Cleanup(tools.ClearPlatformDeclarations)
			name := "declaration-test-server"
			key := ServerKey{Name: name}
			switch connection {
			case "lazy":
				mcpMu.Lock()
				lazyServerConfigs[key] = map[string]any{}
				mcpMu.Unlock()
			case "live":
				handlerServer(t, name, nil)
			case "recycled":
				recycled := newServerTask(name, &tools.Env{})
				recycled.mu.Lock()
				recycled.recycledReason = "idle"
				recycled.mu.Unlock()
				mcpMu.Lock()
				mcpServers[key] = recycled
				mcpMu.Unlock()
			}
			check := makeCheckFn(name)
			connected := connection != "absent"
			env := &tools.Env{}
			if check(env) != connected {
				t.Fatal("connection verdict")
			}
			location := filepath.Join(t.TempDir(), "application.exe")
			decl := executableDeclaration(t, name, location)
			tools.RegisterPlatformDeclaration(name, decl)
			if check(env) {
				t.Fatal("a missing application must hide the server")
			}
			if err := os.WriteFile(location, []byte("presence fixture"), 0o644); err != nil {
				t.Fatal(err)
			}
			if check(env) != connected {
				t.Fatal("installed application verdict")
			}
			os.Remove(location)
			if check(env) {
				t.Fatal("an uninstalled application must hide the server again")
			}
			tools.ClearPlatformDeclarations()
			if check(env) != connected {
				t.Fatal("cleared declarations verdict")
			}
		})
	}
}
