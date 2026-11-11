package commands

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Namespace a gateway invocation hands its handler
func gatewayNamespace(t *testing.T, argv ...string) map[string]any {
	t.Helper()
	c := NewCLI()
	var got map[string]any
	for _, path := range c.Unbound() {
		if path == "gateway" || strings.HasPrefix(path, "gateway ") {
			c.Bind(path, func(cmd *cobra.Command, args []string) error {
				got = c.Namespace(cmd, args)
				return nil
			})
		}
	}
	if err := c.Execute(append([]string{"gateway"}, argv...)); err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	if got == nil {
		t.Fatalf("%v: handler never ran", argv)
	}
	return got
}

func TestGatewayParserNamespaceDestsAndDefaults(t *testing.T) {
	cases := []struct {
		argv []string
		want map[string]any
	}{
		{[]string{"run"}, map[string]any{"command": "gateway", "gateway_command": "run", "verbose": 0, "quiet": false, "replace": false,
			"force": false, "no_supervise": false, "external_supervisor": false, "idle_exit": false, "accept_hooks": false}},
		{[]string{"run", "-vv", "-q", "--replace", "--force", "--no-supervise", "--external-supervisor", "--idle-exit"},
			map[string]any{"verbose": 2, "quiet": true, "replace": true, "force": true, "no_supervise": true, "external_supervisor": true, "idle_exit": true}},
		{[]string{"ensure"}, map[string]any{"gateway_command": "ensure", "json": false, "timeout": "60"}},
		{[]string{"ensure", "--timeout", "5", "--json"}, map[string]any{"json": true, "timeout": "5"}},
		{[]string{"start"}, map[string]any{"gateway_command": "start", "system": false, "all": false, "force": false, "platform": nil}},
		{[]string{"start", "--all", "--system", "--platform", "telegram"}, map[string]any{"all": true, "system": true, "platform": "telegram"}},
		{[]string{"stop"}, map[string]any{"gateway_command": "stop", "system": false, "all": false, "interrupt": false}},
		{[]string{"stop", "-y"}, map[string]any{"interrupt": true}},
		{[]string{"stop", "--yes"}, map[string]any{"interrupt": true}},
		{[]string{"restart"}, map[string]any{"gateway_command": "restart", "system": false, "all": false, "force": false, "interrupt": false, "detach": false, "platform": nil}},
		{[]string{"restart", "--yes", "--detach", "--force"}, map[string]any{"interrupt": true, "detach": true, "force": true}},
		{[]string{"status"}, map[string]any{"gateway_command": "status", "deep": false, "full": false, "system": false, "platform": nil}},
		{[]string{"status", "-l", "--deep"}, map[string]any{"full": true, "deep": true}},
		{[]string{"install"}, map[string]any{"gateway_command": "install", "force": false, "system": false, "run_as_user": nil, "start_now": nil,
			"start_on_login": nil, "if_missing": false, "force_unit_path": false, "elevated_handoff": false}},
		{[]string{"install", "--start-now", "--start-on-login", "--run-as-user", "svc"}, map[string]any{"start_now": true, "start_on_login": true, "run_as_user": "svc"}},
		{[]string{"install", "--no-start-now", "--no-start-on-login"}, map[string]any{"start_now": false, "start_on_login": false}},
		{[]string{"uninstall"}, map[string]any{"gateway_command": "uninstall", "system": false}},
		{[]string{"list"}, map[string]any{"gateway_command": "list"}},
		{[]string{"setup"}, map[string]any{"gateway_command": "setup"}},
		{[]string{"migrate-legacy"}, map[string]any{"gateway_command": "migrate-legacy", "dry_run": false, "yes": false}},
		{[]string{"migrate-legacy", "--dry-run", "-y"}, map[string]any{"dry_run": true, "yes": true}},
		{[]string{"migrate"}, map[string]any{"gateway_command": "migrate", "multiplex": false, "dry_run": false, "yes": false}},
		{[]string{"migrate", "--multiplex", "--dry-run", "--yes"}, map[string]any{"multiplex": true, "dry_run": true, "yes": true}},
		{[]string{"enroll"}, map[string]any{"gateway_command": "enroll", "token": nil, "connector_url": nil, "gateway_id": nil, "wake_url": nil}},
	}
	for _, tc := range cases {
		ns := gatewayNamespace(t, tc.argv...)
		for key, want := range tc.want {
			if got, ok := ns[key]; !ok || !reflect.DeepEqual(got, want) {
				t.Errorf("%v: ns[%q] = %#v (present %v), want %#v", tc.argv, key, got, ok, want)
			}
		}
	}
}

func TestGatewayEnrollDispatch(t *testing.T) {
	ns := gatewayNamespace(t, "enroll", "--token", "tok", "--connector-url", "wss://connector.example.com/relay", "--gateway-id", "gw-1")
	want := map[string]any{"command": "gateway", "gateway_command": "enroll", "token": "tok",
		"connector_url": "wss://connector.example.com/relay", "gateway_id": "gw-1"}
	for key, value := range want {
		if ns[key] != value {
			t.Fatalf("ns[%q] = %#v, want %#v", key, ns[key], value)
		}
	}
}

func TestGatewaySubparserHasNoServiceSubcommand(t *testing.T) {
	c := NewCLI()
	for _, path := range c.Unbound() {
		if path == "gateway" || strings.HasPrefix(path, "gateway ") {
			c.Bind(path, func(*cobra.Command, []string) error { return nil })
		}
	}
	for _, path := range c.Unbound() {
		if path == "gateway service" {
			t.Fatal("`icebath gateway service` must stay removed")
		}
	}
	if found, _, err := c.Root.Find([]string{"gateway", "service", "on"}); err == nil && found.Name() == "service" {
		t.Fatal("`icebath gateway service` must stay removed")
	}
}

func TestGatewayRunHasNoServiceFlag(t *testing.T) {
	if _, ok := gatewayNamespace(t, "run")["service"]; ok {
		t.Fatal("gateway run --service must stay removed")
	}
}
