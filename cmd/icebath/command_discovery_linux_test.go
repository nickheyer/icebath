//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Discovery requests the in process methods answer, read from the first argument, results printed as one JSON line
const discoveryMethodsMode = "discovery-methods"

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
