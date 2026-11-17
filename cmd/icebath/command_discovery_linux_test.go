//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/pluginsdk"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Discovery requests the in process methods answer, read from the first argument, results printed as one JSON line
const discoveryMethodsMode = "discovery-methods"

// External program of the probe plugin, the register function of the hermes fixture plugin
const probePluginMode = "probe-plugin"

func init() {
	childModes[discoveryMethodsMode] = runDiscoveryMethods
	childModes[probePluginMode] = runProbePlugin
}

// Serves the plugin wire as a plugin registering the probe-plugin command that answers its arguments
func runProbePlugin() {
	pluginsdk.Register(func(host pluginsdk.Host) error {
		_, err := host.RegisterCommand(pluginsdk.Command{Name: "probe-plugin", Description: "Disposable plugin discovery",
			Handler: func(_ context.Context, args string) (string, error) { return args, nil }})
		return err
	})
	pluginsdk.Main()
}

// Writes the probe plugin under the home, a program plugin whose entry re-executes this binary as the plugin
func writeProbePlugin(t *testing.T, h string) {
	t.Helper()
	dir := filepath.Join(h, "plugins", "probe_plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name: probe_plugin\nversion: 0.1.0\ndescription: Discovery fixture\nruntime: program\nentry:\n  - probe-plugin\n"
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	entry := "#!/bin/sh\n" + childModeEnv + "=" + probePluginMode + " exec '" + os.Args[0] + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "probe-plugin"), []byte(entry), 0o755); err != nil {
		t.Fatal(err)
	}
}

// One method and its params of the compared discovery requests
type discoveryRequest struct {
	method string
	params map[string]any
}

func (r discoveryRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal([]any{r.method, r.params})
}

func (r *discoveryRequest) UnmarshalJSON(raw []byte) error {
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return fmt.Errorf("request %s is not a method and params pair: %v", raw, err)
	}
	if err := json.Unmarshal(pair[0], &r.method); err != nil {
		return err
	}
	return json.Unmarshal(pair[1], &r.params)
}

// Calls each request on the server methods in this process and prints their results, exit 1 when one has none
func runDiscoveryMethods() {
	ctx := context.Background()
	startProcess(ctx)
	ctx = tuigateway.BindTransport(ctx, tuigateway.ModuleStdioTransport())
	var requests []discoveryRequest
	if err := json.Unmarshal([]byte(os.Args[1]), &requests); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	results := []any{}
	for i, request := range requests {
		handler := tuiserver.Method(request.method)
		if handler == nil {
			fmt.Fprintf(os.Stderr, "KeyError: '%s'\n", request.method)
			os.Exit(1)
		}
		reply := handler(ctx, i, request.params)
		result, has := reply["result"]
		if !has {
			raw, _ := json.Marshal(reply)
			fmt.Fprintf(os.Stderr, "KeyError: 'result' in %s\n", raw)
			os.Exit(1)
		}
		results = append(results, result)
	}
	raw, err := json.Marshal(results)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, string(raw))
	os.Exit(0)
}

// Result of the reply with the id, skipping other frames for twenty seconds
func discoveryReply(t *testing.T, ws *websocket.Conn, id any) map[string]any {
	t.Helper()
	if err := ws.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var reply map[string]any
		if err := ws.ReadJSON(&reply); err != nil {
			t.Fatal(err)
		}
		if reply["id"] == id {
			return reply
		}
	}
}

// Names of the skill entries of a catalog that differ between the two answers
func skillsDelta(got, baseline map[string]any) map[string][2]any {
	a, _ := got["skills"].(map[string]any)
	b, _ := baseline["skills"].(map[string]any)
	delta := map[string][2]any{}
	for _, side := range []map[string]any{a, b} {
		for key := range side {
			if !reflect.DeepEqual(a[key], b[key]) {
				delta[key] = [2]any{a[key], b[key]}
			}
		}
	}
	return delta
}

func TestOrdinaryDaemonDiscoveryMatchesLegacyExtensions(t *testing.T) {
	tmp, h, user := gatewayCaseHomes(t, map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "discovery-fixture", "base_url": "http://127.0.0.1:1/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
		"quick_commands": map[string]any{"probe-quick": map[string]any{"type": "alias", "target": "/help",
			"description": "Disposable quick discovery"}},
		"plugins": map[string]any{"enabled": []any{"probe_plugin"}},
	})
	skill := filepath.Join(h, "skills", "probe-skill")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: probe-skill\ndescription: Disposable skill discovery\n---\n# Probe\nOnly a discovery fixture.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProbePlugin(t, h)
	env := gatewayChildEnv(h, user)
	requests := []discoveryRequest{{"commands.catalog", map[string]any{}}}
	for _, text := range []string{"/", "/probe", "/probe-quick", "/help", "/details c", "plain"} {
		requests = append(requests, discoveryRequest{"complete.slash", map[string]any{"text": text}})
	}
	daemon := startGatewayChild(t, filepath.Join(tmp, "gateway.log"), env, gateway.GatewayRunModuleEntry)
	descriptor := daemon.waitReady(t, h, 40*time.Second)
	encoded, err := json.Marshal(requests)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCLIProgram(t, cliProgramBinary(t),
		append(append([]string{}, env...), childModeEnv+"="+discoveryMethodsMode), 45*time.Second, string(encoded))
	if code != 0 {
		t.Fatalf("legacy methods exit %d\n%s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var expected []any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &expected); err != nil || len(expected) != len(requests) {
		t.Fatalf("legacy results %v\n%s", err, stdout)
	}
	catalog, _ := expected[0].(map[string]any)
	pairs := map[string]any{}
	rawPairs, _ := catalog["pairs"].([]any)
	for _, raw := range rawPairs {
		if pair, isPair := raw.([]any); isPair && len(pair) == 2 {
			pairs[fmt.Sprint(pair[0])] = pair[1]
		}
	}
	for _, name := range []string{"/probe-skill", "/probe-quick", "/probe-plugin"} {
		if _, listed := pairs[name]; !listed {
			t.Fatalf("%s is not in the catalog, warning %v", name, catalog["warning"])
		}
	}
	completed := map[string]bool{}
	probeItems, _ := expected[2].(map[string]any)
	rawItems, _ := probeItems["items"].([]any)
	for _, raw := range rawItems {
		row, _ := raw.(map[string]any)
		text, _ := row["text"].(string)
		completed[strings.TrimLeft(strings.TrimSpace(text), "/")] = true
	}
	if !completed["probe-skill"] || !completed["probe-plugin"] {
		t.Fatalf("/probe completions = %v", expected[2])
	}
	receipt := map[string]any{}
	origin, _ := descriptor["api_origin"].(string)
	url := wsOrigin(origin)
	requireUpgradeRefused(t, url, nil)
	resolved, err := filepath.EvalSymlinks(h)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := dialSession(t, h, url, map[string]any{"profile_id": resolved, "instance_id": descriptor["instance_id"], "purpose": "interactive"})
	rpc := func(method string, params map[string]any) map[string]any {
		if err := ws.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": method, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		return discoveryReply(t, ws, method)
	}
	for i, request := range requests {
		reply := rpc(request.method, request.params)
		got := reply["result"]
		if reflect.DeepEqual(got, expected[i]) {
			continue
		}
		gotDict, isDict := got.(map[string]any)
		baseline, _ := expected[i].(map[string]any)
		if _, hasSkills := gotDict["skills"]; isDict && hasSkills {
			t.Fatalf("skills delta daemon vs legacy: %v", skillsDelta(gotDict, baseline))
		}
		t.Fatalf("%s %v answered %v, want %v", request.method, request.params, reply, expected[i])
	}
	for _, method := range []string{"commands.catalog", "complete.slash"} {
		params := map[string]any{"profile": "foreign-profile"}
		if method == "complete.slash" {
			params["text"] = "/probe"
		}
		reply := rpc(method, params)
		if refusal, _ := reply["error"].(map[string]any); refusal["message"] != "profile_mismatch" {
			t.Fatalf("%s for a foreign profile = %v", method, reply)
		}
	}
	ws.Close()
	exited, err := daemon.interrupt(20 * time.Second)
	if !exited || err != nil {
		t.Fatalf("daemon exit = %v, %v\n%s", exited, err, daemon.output())
	}
	receipt["legacy_pid_exit"], receipt["catalog_pairs"], receipt["skill_count"] = code, len(pairs), catalog["skill_count"]
	receipt["compared_requests"], receipt["unauthenticated_ws"], receipt["foreign_profile"] = len(requests), "rejected", "rejected"
	receipt["pid"], receipt["daemon_exit"] = daemon.process.Process.Pid, daemon.process.ProcessState.ExitCode()
	receipt["instance_id"], receipt["api_origin"] = descriptor["instance_id"], descriptor["api_origin"]
	raw, _ := json.Marshal(receipt)
	t.Log(string(raw))
}
