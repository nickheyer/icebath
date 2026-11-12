//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
)

// Session token the pooled desktop children of the idle proof case accept
const idleProofToken = "idle-proof-live-token"

// Holds a cron run in the running job ledger of the child home before the server starts
func init() {
	childModes["idle-proof-busy"] = func() { cron.HoldInflightRun("live-idle-proof-job", home.Dir(context.Background())) }
}

// Desktop shaped headless server start in its own home on an ephemeral port, busy holding a cron run first
func spawnDesktopChild(t *testing.T, tmp, name string, busy bool) (*exec.Cmd, string) {
	t.Helper()
	h := filepath.Join(tmp, name)
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0])
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "ICEBATH_PARENT_PID", "ICEBATH_PARENT_START_MARKER", "ICEBATH_PARENT_NONCE", "ICEBATH_HOME":
		default:
			child.Env = append(child.Env, kv)
		}
	}
	child.Env = append(child.Env, "ICEBATH_HOME="+h, "ICEBATH_SERVE_HEADLESS=1", "ICEBATH_DESKTOP=1",
		"ICEBATH_DASHBOARD_SESSION_TOKEN="+idleProofToken, startServerProbeEnv+"=0")
	if busy {
		child.Env = append(child.Env, childModeEnv+"=idle-proof-busy")
	}
	stdout := filepath.Join(tmp, name+".out")
	out, err := os.Create(stdout)
	if err != nil {
		t.Fatal(err)
	}
	child.Stdout = out
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		terminateServe(child)
		out.Close()
	})
	return child, stdout
}

// Port of the backend ready line in the output file, failing the case after two minutes
func awaitReadyPort(t *testing.T, name, stdout string) int {
	t.Helper()
	for deadline := time.Now().Add(120 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		raw, _ := os.ReadFile(stdout)
		for _, line := range strings.Split(string(raw), "\n") {
			if _, rest, found := strings.Cut(line, "ICEBATH_BACKEND_READY port="); found {
				port, err := strconv.Atoi(strings.TrimSpace(rest))
				if err != nil {
					t.Fatal(err)
				}
				return port
			}
		}
	}
	raw, _ := os.ReadFile(stdout)
	t.Fatalf("%s: no READY sentinel; output:\n%s", name, raw)
	return 0
}

// Status and body of the idle probe, the token header sent when one is given
func probeIdle(t *testing.T, port int, token string) (int, map[string]any) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/api/health/idle", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("X-Icebath-Session-Token", token)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := map[string]any{}
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, body
}

// Probes until accept holds or the window closes, returning the last verdict and every body seen
func probeIdleUntil(t *testing.T, port int, window time.Duration, accept func(map[string]any) bool) (int, map[string]any, []map[string]any) {
	t.Helper()
	var seen []map[string]any
	for deadline := time.Now().Add(window); ; time.Sleep(200 * time.Millisecond) {
		status, body := probeIdle(t, port, idleProofToken)
		seen = append(seen, body)
		if accept(body) || time.Now().After(deadline) {
			return status, body, seen
		}
	}
}

func TestLivePooledChildrenProveIdleOrBusyOverTheDesktopProbe(t *testing.T) {
	tmp := t.TempDir()
	names := []string{"resident-a", "resident-b", "cron-busy"}
	outputs := map[string]string{}
	for _, name := range names {
		_, outputs[name] = spawnDesktopChild(t, tmp, name, name == "cron-busy")
	}
	ports := map[string]int{}
	for _, name := range names {
		ports[name] = awaitReadyPort(t, name, outputs[name])
	}
	idle := map[string]any{"ok": true, "idle": true, "reason": nil}
	for _, name := range names[:2] {
		status, body, _ := probeIdleUntil(t, ports[name], 20*time.Second, func(b map[string]any) bool { return b["idle"] == true })
		if status != http.StatusOK || !reflect.DeepEqual(body, idle) {
			t.Fatalf("%s: %d %v", name, status, body)
		}
	}
	status, body, seen := probeIdleUntil(t, ports["cron-busy"], 60*time.Second, func(b map[string]any) bool {
		detail, _ := b["detail"].(string)
		return strings.HasPrefix(detail, "cron:")
	})
	busy := map[string]any{"ok": true, "idle": false, "reason": "turn_in_flight", "detail": "cron:live-idle-proof-job"}
	if status != http.StatusOK || !reflect.DeepEqual(body, busy) {
		t.Fatalf("cron-busy: %d %v", status, body)
	}
	for _, sample := range seen {
		if sample["idle"] != false {
			t.Fatalf("the busy child claimed idle: %v", seen)
		}
	}
	if status, body := probeIdle(t, ports["resident-a"], ""); status != http.StatusUnauthorized || body["idle"] != nil {
		t.Fatalf("unauthenticated probe: %d %v", status, body)
	}
}
