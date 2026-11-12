//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Fails unless the path and its mode carry no group or other permission bits
func requirePrivateMode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("%s mode = %v", path, info.Mode())
	}
}

func TestOrdinaryDaemonKeepsControlAuthAndLoopWitness(t *testing.T) {
	for _, layout := range []string{"normal", "long", "unicode", "long-temp"} {
		t.Run(layout, func(t *testing.T) { ordinaryDaemonKeepsControlAuthAndLoopWitness(t, layout) })
	}
}

func ordinaryDaemonKeepsControlAuthAndLoopWitness(t *testing.T, layout string) {
	tmp := t.TempDir()
	short, err := os.MkdirTemp("", "hgw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	h := short
	switch layout {
	case "unicode":
		h = filepath.Join(short, strings.Repeat("界", 20))
	case "long", "long-temp":
		h = filepath.Join(tmp, strings.Repeat("x", 120))
	}
	if err := os.MkdirAll(h, 0o700); err != nil {
		t.Fatal(err)
	}
	user := filepath.Join(tmp, "user")
	if err := os.Mkdir(user, 0o755); err != nil {
		t.Fatal(err)
	}
	config, err := json.Marshal(map[string]any{
		"gateway":           map[string]any{"multiplex_profiles": false},
		"model":             map[string]any{"provider": "custom", "default": "local-wire-stub", "base_url": "http://127.0.0.1:1/v1"},
		"terminal":          map[string]any{"cwd": tmp},
		"platform_toolsets": map[string]any{"cli": []any{}},
		"auxiliary":         map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	var env []string
	for _, name := range []string{"PATH", "LANG", "TZ"} {
		if value, set := os.LookupEnv(name); set {
			env = append(env, name+"="+value)
		}
	}
	env = append(env, "HOME="+user, "USERPROFILE="+user, "ICEBATH_HOME="+h, "ICEBATH_DASHBOARD_SESSION_TOKEN="+token)
	if layout == "long-temp" {
		temp := filepath.Join(tmp, strings.Repeat("t", 120))
		if err := os.Mkdir(temp, 0o755); err != nil {
			t.Fatal(err)
		}
		env = append(env, "TMPDIR="+temp)
	}
	daemon := startGatewayChild(t, filepath.Join(tmp, "daemon.log"), env, gateway.GatewayRunModuleEntry)
	controlPath := ""
	t.Cleanup(func() {
		if controlPath != "" && filepath.Dir(controlPath) != h {
			_ = os.Remove(filepath.Dir(controlPath))
		}
	})
	var descriptor map[string]any
	deadline := time.Now().Add(35 * time.Second)
	for running := true; running && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		select {
		case err := <-daemon.exited:
			daemon.stopped, running = true, false
			daemon.exited <- err
		default:
		}
		descriptor = map[string]any{}
		if identified := gateway.IdentifyGateway(h, 2*time.Second); identified != nil {
			raw, _ := json.Marshal(identified)
			_ = json.Unmarshal(raw, &descriptor)
		}
		if descriptor["state"] == "ready" {
			break
		}
	}
	if descriptor["state"] != "ready" {
		t.Fatalf("descriptor = %v\n%s", descriptor, daemon.output())
	}
	if descriptor["pid"] != float64(daemon.process.Process.Pid) {
		t.Fatalf("pid = %v, want %d", descriptor["pid"], daemon.process.Process.Pid)
	}
	controlPath, err = gateway.ResolveClientSocketPath(h)
	if err != nil || controlPath == "" {
		t.Fatalf("control path = %q, %v", controlPath, err)
	}
	requirePrivateMode(t, controlPath)
	if filepath.Dir(controlPath) != h {
		requirePrivateMode(t, filepath.Dir(controlPath))
	}
	origin, _ := descriptor["api_origin"].(string)
	sid := createWithSessionToken(t, origin, token, "daemon-token", 15*time.Second)
	heartbeat := filepath.Join(h, "state", "gateway.heartbeat")
	for _, err := os.Stat(heartbeat); err != nil && time.Now().Before(deadline); _, err = os.Stat(heartbeat) {
		time.Sleep(50 * time.Millisecond)
	}
	raw, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	pid := daemon.process.Process.Pid
	tickPath := gateway.GetLoopTickSocketPath(context.Background(), h, &pid)
	receipt, _ := json.Marshal(map[string]any{"layout": layout, "home_bytes": len(h), "tick_chars": len([]rune(tickPath)),
		"tick_bytes": len(tickPath), "control": controlPath, "created_session": sid,
		"loop_tick_socket": payload["loop_tick_socket"], "loop_tick_tcp_port": payload["loop_tick_tcp_port"]})
	t.Log(string(receipt))
	if payload["loop_tick_socket"] != true {
		t.Fatal(string(receipt))
	}
	port, _ := payload["loop_tick_tcp_port"].(float64)
	network, address := "unix", tickPath
	if port != 0 {
		network, address = "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port)))
	}
	peer, err := net.DialTimeout(network, address, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = peer.SetDeadline(time.Now().Add(2 * time.Second))
	answer := make([]byte, 1)
	_, err = io.ReadFull(peer, answer)
	peer.Close()
	if err != nil || string(answer) != "1" {
		t.Fatalf("tick answer = %q, %v", answer, err)
	}
	stale := 0.0
	if liveness := gateway.ProbeGatewayLoopLiveness(context.Background(), pid, gateway.LoopLivenessOptions{StaleAfterS: &stale, Home: h}); liveness != "alive" {
		t.Fatalf("liveness = %s", liveness)
	}
	if err := daemon.process.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-daemon.exited:
		daemon.stopped = true
		if err != nil {
			t.Fatalf("exit = %v\n%s", err, daemon.output())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the daemon did not stop on SIGINT\n%s", daemon.output())
	}
	for _, gone := range []string{controlPath, filepath.Join(h, "gateway.sock.path")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s is still there: %v", gone, err)
		}
	}
	if port != 0 {
		if peer, err := net.DialTimeout("tcp4", address, 2*time.Second); err == nil {
			peer.Close()
			t.Fatal("the tick port still answers")
		}
	} else if _, err := os.Stat(tickPath); !os.IsNotExist(err) {
		t.Fatalf("%s is still there: %v", tickPath, err)
	}
}
