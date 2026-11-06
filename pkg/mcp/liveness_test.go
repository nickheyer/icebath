package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestLivenessParseContract(t *testing.T) {
	for _, kind := range []string{"static", "interactive_session"} {
		if live, err := ParseLiveness(map[string]any{"kind": kind}); err != nil || live.Kind != kind {
			t.Fatalf("%s %+v %v", kind, live, err)
		}
	}
	live, err := ParseLiveness(map[string]any{"kind": "server_json", "path": " /tmp/example.json ",
		"fields": map[string]any{"url": "endpoint", "token": "secret", "pid": "process"}})
	if err != nil || live != (Liveness{"server_json", "/tmp/example.json", "endpoint", "secret", "process"}) {
		t.Fatalf("server_json %+v %v", live, err)
	}
	defaulted, err := ParseLiveness(map[string]any{"kind": "server_json", "path": "/tmp/example.json"})
	if err != nil || defaulted.URLField != "http" || defaulted.TokenField != "token" || defaulted.PIDField != "pid" {
		t.Fatalf("defaulted %+v %v", defaulted, err)
	}
	partial, err := ParseLiveness(map[string]any{"kind": "server_json", "path": "/tmp/example.json", "fields": map[string]any{"url": "endpoint"}})
	if err != nil || partial.URLField != "endpoint" || partial.TokenField != "token" || partial.PIDField != "pid" {
		t.Fatalf("partial %+v %v", partial, err)
	}
	bad := []struct {
		raw  any
		text string
	}{
		{[]any{}, "liveness must be an object"},
		{map[string]any{"kind": "unknown"}, "unknown liveness kind: 'unknown'"},
		{map[string]any{}, "unknown liveness kind: None"},
		{map[string]any{"kind": "static", "path": "x"}, "static liveness only accepts 'kind'"},
		{map[string]any{"kind": "interactive_session", "x": 1}, "interactive_session liveness only accepts 'kind'"},
		{map[string]any{"kind": "server_json", "path": "/p", "port": 1}, "server_json liveness has unknown fields"},
		{map[string]any{"kind": "server_json", "path": "  "}, "server_json liveness requires a non-empty path"},
		{map[string]any{"kind": "server_json"}, "server_json liveness requires a non-empty path"},
		{map[string]any{"kind": "server_json", "path": "/p", "fields": map[string]any{"port": "p"}}, "server_json liveness fields may only override url, token, and pid"},
		{map[string]any{"kind": "server_json", "path": "/p", "fields": map[string]any{"pid": 3}}, "server_json liveness field names must be non-empty strings"},
		{map[string]any{"kind": "server_json", "path": "/p", "fields": "url"}, "'str' object is not a mapping"},
	}
	for _, c := range bad {
		if _, err := ParseLiveness(c.raw); err == nil || err.Error() != c.text {
			t.Errorf("%v: %v", c.raw, err)
		}
	}
}

func TestLivenessUnregisteredServerIsStatic(t *testing.T) {
	if live := livenessFor(context.Background(), "never-registered-liveness"); live != newLiveness("static") {
		t.Fatalf("unregistered %+v", live)
	}
	if livenessKind(context.Background(), "never-registered-liveness") != "static" {
		t.Fatal("kind")
	}
	if livenessStatus(context.Background(), "never-declared") != nil {
		t.Fatal("an undeclared server has no unavailable state")
	}
	if _, _, _, ok := unavailableDetails(context.Background(), "never-declared"); ok {
		t.Fatal("an undeclared server has no details")
	}
}

func TestLivenessAppDefinitionAndActions(t *testing.T) {
	def := AppDefinition{Platform: "app"}
	if got := newLiveness("static").AppDefinition(def); got != def {
		t.Fatalf("static leaves the definition alone %+v", got)
	}
	got := Liveness{"server_json", "/run/app.json", "endpoint", "secret", "process"}.AppDefinition(def)
	if got != (AppDefinition{"app", "server_json", "/run/app.json", "process", "endpoint", "secret"}) {
		t.Fatalf("server_json keys %+v", got)
	}
	want := map[string][2]string{
		"app_not_running":        {"Start App, then try again.", "after_user_action"},
		"icebath_not_connected":  {"Reconnect App in Icebath, then try again.", "after_user_action"},
		"endpoint_unavailable":   {"Open App and enable its local connection, then try again.", "after_user_action"},
		"no_interactive_session": {"Open an interactive desktop session and start App, then try again.", "never_here"},
		"version_too_old":        {"Update App, then try again.", "after_user_action"},
		"missing_app":            {"Install App, then try again.", "after_user_action"},
		"unsupported_gpu":        {"Use App on a machine with an NVIDIA GPU.", "never_here"},
	}
	for state, w := range want {
		if action, retry := livenessAction(state, "App", "an NVIDIA GPU"); action != w[0] || retry != w[1] {
			t.Errorf("%s: %q %q", state, action, retry)
		}
	}
	for state, problem := range map[string]string{"missing_app": "missing_app", "unsupported_os": "missing_app",
		"unsupported_gpu": "unsupported_gpu", "version_too_old": "version_too_old", "available": ""} {
		if got := availabilityProblem(AppAvailability{State: state}); got != problem {
			t.Errorf("%s -> %q", state, got)
		}
	}
}

func TestLivenessDescribeSentences(t *testing.T) {
	decl := &AppDeclaration{Name: "example-server"}
	cases := []struct {
		available AppAvailability
		state     string
		display   string
		want      string
	}{
		{AppAvailability{}, "icebath_not_connected", "Example Tools", "Example Tools's MCP connection is missing. Reconnect Example Tools in Icebath, then try again."},
		{AppAvailability{State: "unsupported_os"}, "missing_app", "", "example-server is not supported on this operating system. Use a supported operating system."},
		{AppAvailability{State: "missing_app", MinVersion: "2.1"}, "missing_app", "", "example-server is not installed. Install example-server 2.1 or newer, then try again."},
		{AppAvailability{State: "missing_app"}, "missing_app", "", "example-server is not installed. Install example-server, then try again."},
		{AppAvailability{State: "version_too_old", Version: "1.0", MinVersion: "2.1"}, "version_too_old", "", "example-server version 1.0 is too old; version 2.1 or newer is required. Update example-server, then try again."},
		{AppAvailability{State: "version_too_old"}, "version_too_old", "", "example-server is too old. Update example-server, then try again."},
		{AppAvailability{}, "no_interactive_session", "", "example-server needs an interactive desktop session. Open an interactive desktop session and start example-server, then try again."},
		{AppAvailability{}, "endpoint_unavailable", "", "example-server's local endpoint is unavailable. Open example-server and enable its local connection, then try again."},
		{AppAvailability{}, "app_not_running", "", "example-server is not running. Start example-server, then try again."},
		{AppAvailability{State: "unsupported_gpu"}, "unsupported_gpu", "", "example-server needs a supported GPU; none was found on this machine. Use example-server on a machine with a supported GPU."},
	}
	for _, c := range cases {
		if got := describeUnavailable(decl, c.available, c.state, c.display); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.state, got, c.want)
		}
	}
	gpu := describeUnavailable(&AppDeclaration{Name: "x", RequiredGPU: "nvidia"}, AppAvailability{State: "unsupported_gpu"}, "unsupported_gpu", "")
	if !strings.Contains(gpu, "an NVIDIA GPU") || strings.Contains(gpu, "Install") {
		t.Fatalf("gpu sentence %q", gpu)
	}
}

func TestLivenessLiveEndpointOnlyForRuntimeFiles(t *testing.T) {
	url, headers, ok, err := liveEndpoint(context.Background(), "never-registered-liveness")
	if url != "" || headers != nil || ok || err != nil {
		t.Fatalf("a static server has no live endpoint %q %v %v %v", url, headers, ok, err)
	}
	e := &LiveEndpointUnavailableError{Msg: "MCP server 'x' has no usable live endpoint"}
	if e.Error() != e.Msg || classifyMCPFailure(e) != "transient" {
		t.Fatal("an unavailable live endpoint is a transient connection error")
	}
}

// Plugins services parser converts each parsed declaration field for field and keeps the parse error text
func TestLivenessConvertsToThePluginsServerLiveness(t *testing.T) {
	var parse func(raw any) (plugins.ServerLiveness, error) = func(raw any) (plugins.ServerLiveness, error) {
		l, err := ParseLiveness(raw)
		return plugins.ServerLiveness(l), err
	}
	cases := []struct {
		raw  any
		want plugins.ServerLiveness
	}{
		{map[string]any{"kind": "static"}, plugins.ServerLiveness{Kind: "static", URLField: "http", TokenField: "token", PIDField: "pid"}},
		{map[string]any{"kind": "interactive_session"}, plugins.ServerLiveness{Kind: "interactive_session", URLField: "http", TokenField: "token", PIDField: "pid"}},
		{map[string]any{"kind": "server_json", "path": " ~/.app/server.json ", "fields": map[string]any{"url": " endpoint ", "pid": "process"}},
			plugins.ServerLiveness{Kind: "server_json", Path: "~/.app/server.json", URLField: "endpoint", TokenField: "token", PIDField: "process"}},
	}
	for _, c := range cases {
		got, err := parse(c.raw)
		if err != nil || got != c.want {
			t.Fatalf("parse(%v) = %+v, %v want %+v", c.raw, got, err, c.want)
		}
	}
	if got, err := parse(map[string]any{"kind": "server_json", "path": "  "}); err == nil || err.Error() != "server_json liveness requires a non-empty path" || got != (plugins.ServerLiveness{}) {
		t.Fatalf("blank path parsed to %+v, %v", got, err)
	}
	if _, err := parse("static"); err == nil || err.Error() != "liveness must be an object" {
		t.Fatalf("non object liveness error %v", err)
	}
}
