package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Child mode that only loads the connectors packages and exits before any test runs
const connectorsLoadChild = "ICEBATH_MCP_CONNECTORS_LOAD_CHILD"

func TestPackageLoadDoesNotStartConnectorWork(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), connectorsLoadChild+"=1", "CONNECTOR_GATEWAY_URL="+srv.URL, "TOOL_GATEWAY_USER_TOKEN=t",
		"ICEBATH_PORTAL_BASE_URL="+srv.URL, "ICEBATH_HOME="+t.TempDir())
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child %v", err)
	}
	if strings.TrimSpace(string(out)) != "" || requests.Load() != 0 {
		t.Fatalf("eager connector work: stdout %q, %d gateway requests", out, requests.Load())
	}
}

func TestConcurrentRegistryAndPackageUseNeverDeadlocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	old := tools.NousAccount
	tools.NousAccount = nil
	tools.InvalidateCheckCache()
	t.Cleanup(func() {
		tools.NousAccount = old
		tools.InvalidateCheckCache()
	})
	const iterations = 25
	var mu sync.Mutex
	var failures []string
	fail := func(target, what string) {
		mu.Lock()
		failures = append(failures, target+": "+what)
		mu.Unlock()
	}
	run := func(target string, step func(env *tools.Env) string) func() {
		return func() {
			for range iterations {
				if what := step(&tools.Env{Home: home}); what != "" {
					fail(target, what)
				}
			}
		}
	}
	registry := run("manage_connections", func(env *tools.Env) string {
		out := tools.NewDispatcher(tools.Default).RegistryDispatch(context.Background(), &tools.Call{Name: "manage_connections", Args: map[string]any{"action": "status"}, Env: env})
		if !strings.Contains(out, "not available in this session") {
			return out
		}
		return ""
	})
	pkg := run("connectors", func(env *tools.Env) string {
		if tools.ConnectorsAvailable(env) || tools.LiveGet("s", "op", home) != nil {
			return "connectors open without an identity"
		}
		return ""
	})
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(pkg)
		wg.Go(registry)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(120 * time.Second):
		t.Fatal("alive after 120 seconds")
	}
	if len(failures) != 0 {
		t.Fatalf("errors %v", failures)
	}
}
