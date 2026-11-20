//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Loopback model whose chat requests never answer, reporting each one it takes and each one its client drops
type hangingModel struct {
	server  *httptest.Server
	started chan struct{}
	dropped chan struct{}
}

func newHangingModel(t *testing.T) *hangingModel {
	t.Helper()
	m := &hangingModel{started: make(chan struct{}, 16), dropped: make(chan struct{}, 16)}
	release := make(chan struct{})
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data": [{"id": "local-wire-stub", "object": "model"}]}`))
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if _, isChat := request["messages"]; !isChat {
			http.NotFound(w, r)
			return
		}
		m.started <- struct{}{}
		select {
		case <-r.Context().Done():
			m.dropped <- struct{}{}
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		m.server.Close()
	})
	return m
}

func TestStdioEntrySigtermDuringAHungTurnFinalizesAndExitsZero(t *testing.T) {
	peer := &modelPeer{}
	hung := newHangingModel(t)
	peer.server = hung.server
	h, user := serveTurnHomes(t, peer)
	child, stream := startStdioEntryChild(t, h, user, peer)
	stream.waitEvent(t, 0, 30*time.Second, func(event map[string]any) bool { return event["type"] == "gateway.ready" })
	created := stream.result(t, "session.create", map[string]any{"cols": 100})
	sid, _ := created["session_id"].(string)
	if submitted := stream.result(t, "prompt.submit", map[string]any{"session_id": sid, "text": "WS_SHARED hang"}); submitted["status"] != "streaming" {
		t.Fatalf("prompt.submit = %v", submitted)
	}
	select {
	case <-hung.started:
	case <-time.After(60 * time.Second):
		t.Fatalf("the turn never reached the model\n%s", child.logs())
	}
	if err := child.process.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-child.exited:
		child.stopped = true
		if err != nil {
			t.Fatalf("entry exit on SIGTERM = %v\n%s", err, child.logs())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the entry did not exit on SIGTERM\n%s", child.logs())
	}
	select {
	case <-hung.dropped:
	case <-time.After(10 * time.Second):
		t.Fatalf("the hung model request outlived the gateway\n%s", child.logs())
	}
	child.requireOnlyFrames(t)
	stderr, _ := os.ReadFile(child.stderr)
	if !strings.Contains(string(stderr), "[gateway-signal] SIGTERM") {
		t.Fatalf("no signal line on stderr:\n%s", stderr)
	}
	crash, _ := os.ReadFile(filepath.Join(h, "logs", "tui_gateway_crash.log"))
	if !strings.Contains(string(crash), "SIGTERM received") || !strings.Contains(string(crash), "main-thread stack at signal delivery") {
		t.Fatalf("crash log:\n%s", crash)
	}
	after := gateway.NewProfileOwnership()
	if err := after.Reserve(context.Background(), []string{h}, ""); err != nil {
		t.Fatalf("the home was not released at exit: %v", err)
	}
	after.Release(h)
}
