//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Discovery requests the in process methods answer, read from the first argument, results printed as one JSON line
const discoveryMethodsMode = "discovery-methods"

// Site whose plugin host children run on the system python3, the interpreter every hermes process runs plugins in
const systemPythonSite = "system-python"

func init() {
	childModes[discoveryMethodsMode] = runDiscoveryMethods
	siteHooks[systemPythonSite] = useSystemPythonPlugins
}

// Points every plugin host child of this process at the system python3, exit 2 when none is installed
func useSystemPythonPlugins() {
	python, err := exec.LookPath("python3")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	plugins.SwapHostInterpreter(func(context.Context) (string, error) { return python, nil })
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

// Copies a directory tree with its file modes
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		target := filepath.Join(to, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if d.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Results of the requests through a separate serve backend over its token socket, on a copy of the home
func legacyWSSnapshot(t *testing.T, h string, env []string, requests []discoveryRequest) ([]any, map[string]any) {
	t.Helper()
	legacy := filepath.Join(filepath.Dir(h), "legacy-state")
	if err := os.Mkdir(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "config.yaml"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"skills", "plugins"} {
		copyTree(t, filepath.Join(h, dir), filepath.Join(legacy, dir))
	}
	legacyEnv := []string{}
	for _, kv := range env {
		if !strings.HasPrefix(kv, "ICEBATH_HOME=") {
			legacyEnv = append(legacyEnv, kv)
		}
	}
	legacyEnv = append(legacyEnv, "ICEBATH_HOME="+legacy, "ICEBATH_DASHBOARD_SESSION_TOKEN=disposable-discovery-token", runMainEnv+"=1")
	serve := startGatewayChild(t, filepath.Join(filepath.Dir(h), "legacy-serve.log"), legacyEnv,
		"serve", "--host", "127.0.0.1", "--port", "0", "--skip-build", "--isolated")
	ready := regexp.MustCompile(`ICEBATH_BACKEND_READY port=(\d+)`)
	port := ""
	for deadline := time.Now().Add(40 * time.Second); port == "" && time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		select {
		case err := <-serve.exited:
			serve.stopped = true
			t.Fatalf("serve exited before it was ready: %v\n%s", err, serve.output())
		default:
		}
		if match := ready.FindStringSubmatch(serve.output()); match != nil {
			port = match[1]
		}
	}
	if port == "" {
		t.Fatal(serve.output())
	}
	ws, _, err := websocket.DefaultDialer.Dial("ws://127.0.0.1:"+port+"/api/ws?token=disposable-discovery-token", nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, serve.output())
	}
	var results []any
	for rid, request := range requests {
		if err := ws.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": rid, "method": request.method, "params": request.params}); err != nil {
			t.Fatal(err)
		}
		reply := discoveryReply(t, ws, float64(rid))
		result, has := reply["result"]
		if !has {
			t.Fatalf("serve %s: %v", request.method, reply)
		}
		results = append(results, result)
	}
	ws.Close()
	exited, err := serve.interrupt(20 * time.Second)
	if !exited || err != nil {
		t.Fatalf("serve exit = %v, %v\n%s", exited, err, serve.output())
	}
	return results, map[string]any{"legacy_ws_pid": serve.process.Process.Pid, "legacy_ws_port": port,
		"legacy_ws_exit": serve.process.ProcessState.ExitCode()}
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
	plugin := filepath.Join(h, "plugins", "probe_plugin")
	for path, text := range map[string]string{
		filepath.Join(skill, "SKILL.md"):     "---\nname: probe-skill\ndescription: Disposable skill discovery\n---\n# Probe\nOnly a discovery fixture.\n",
		filepath.Join(plugin, "plugin.yaml"): "name: probe_plugin\nversion: 0.1.0\ndescription: Discovery fixture\n",
		filepath.Join(plugin, "__init__.py"): "def register(ctx):\n    ctx.register_command('probe-plugin', lambda args: args, description='Disposable plugin discovery')\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := gatewayChildEnv(h, user, testSiteEnv+"="+systemPythonSite)
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	legacy := exec.CommandContext(ctx, os.Args[0], string(encoded))
	legacy.Env = append(append([]string{}, env...), childModeEnv+"="+discoveryMethodsMode)
	var stdout, stderr strings.Builder
	legacy.Stdout, legacy.Stderr = &stdout, &stderr
	if err := legacy.Run(); err != nil {
		t.Fatalf("legacy methods: %v\n%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var expected []any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &expected); err != nil || len(expected) != len(requests) {
		t.Fatalf("legacy methods printed %s: %v", stdout.String(), err)
	}
	catalog, _ := expected[0].(map[string]any)
	pairs := map[string]bool{}
	rawPairs, _ := catalog["pairs"].([]any)
	for _, raw := range rawPairs {
		if pair, isList := raw.([]any); isList && len(pair) > 0 {
			name, _ := pair[0].(string)
			pairs[name] = true
		}
	}
	for _, name := range []string{"/probe-skill", "/probe-quick", "/probe-plugin"} {
		if !pairs[name] {
			t.Fatalf("%s missing from the catalog, warning %v", name, catalog["warning"])
		}
	}
	completed := map[string]bool{}
	probeCompletion, _ := expected[2].(map[string]any)
	items, _ := probeCompletion["items"].([]any)
	for _, raw := range items {
		if row, isDict := raw.(map[string]any); isDict {
			text, _ := row["text"].(string)
			completed[strings.TrimLeft(strings.TrimSpace(text), "/")] = true
		}
	}
	if !completed["probe-skill"] || !completed["probe-plugin"] {
		t.Fatalf("/probe completions = %v", items)
	}
	wsBaseline, legacyReceipt := legacyWSSnapshot(t, h, env, requests)
	if !reflect.DeepEqual(wsBaseline, expected) {
		got, _ := json.Marshal(wsBaseline)
		want, _ := json.Marshal(expected)
		t.Fatalf("serve socket results differ from the in process methods\n%s\n%s", got, want)
	}
	origin, _ := descriptor["api_origin"].(string)
	url := strings.Replace(origin, "http:", "ws:", 1) + "/api/ws"
	if ws, response, err := websocket.DefaultDialer.Dial(url, nil); err == nil {
		ws.Close()
		t.Fatal("an unauthenticated session socket was accepted")
	} else if !errors.Is(err, websocket.ErrBadHandshake) || response == nil {
		t.Fatalf("unauthenticated dial = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(h)
	if err != nil {
		t.Fatal(err)
	}
	ws, _ := dialSession(t, h, url, map[string]any{"profile_id": resolved, "instance_id": descriptor["instance_id"], "purpose": "interactive"})
	for i, request := range requests {
		reply := sessionRPC(t, ws, request.method, request.params)
		got := reply["result"]
		if !reflect.DeepEqual(got, expected[i]) {
			gotDict, isDict := got.(map[string]any)
			if _, hasSkills := gotDict["skills"]; isDict && hasSkills {
				baseline, _ := expected[i].(map[string]any)
				t.Fatalf("skills delta daemon vs legacy: %v", skillsDelta(gotDict, baseline))
			}
			t.Fatalf("%s %v = %v, want %v", request.method, request.params, reply, expected[i])
		}
	}
	for _, method := range []string{"commands.catalog", "complete.slash"} {
		params := map[string]any{"profile": "foreign-profile"}
		if method == "complete.slash" {
			params["text"] = "/probe"
		}
		sessionRefusal(t, ws, method, "profile_mismatch", params)
	}
	if err := daemon.process.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-daemon.exited:
		daemon.stopped = true
		if err != nil {
			t.Fatalf("daemon exit = %v\n%s", err, daemon.output())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the daemon did not stop on SIGINT\n%s", daemon.output())
	}
	receipt, _ := json.Marshal(map[string]any{"legacy": legacyReceipt, "catalog_pairs": len(pairs),
		"skill_count": catalog["skill_count"], "compared_requests": len(requests), "unauthenticated_ws": "rejected",
		"foreign_profile": "rejected", "pid": daemon.process.Process.Pid, "instance_id": descriptor["instance_id"],
		"api_origin": origin})
	t.Log(string(receipt))
}
