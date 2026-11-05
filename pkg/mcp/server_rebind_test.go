package mcp

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestServerReconnectAdoptsChangedURLFromConfig(t *testing.T) {
	isolateHome(t)
	fastServerTimings(t)
	startTestLoop(t)
	entry := func(path string) string {
		return fmt.Sprintf("  srv:\n    url: https://mcp.example.test/%s\n    auth: oauth\n    oauth:\n      client_id: a\n", path)
	}
	writeServersConfig(t, entry("sse"))
	old := map[string]any{"url": "https://mcp.example.test/sse", "auth": "oauth", "oauth": map[string]any{"client_id": "a"}}
	s := newServerTask("srv", &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	var seen [][3]string
	s.transportRunner = func(ctx context.Context, r *Rendered) (string, error) {
		s.mu.Lock()
		seen = append(seen, [3]string{pyStr(r.Config["url"]), pyStr(s.config["url"]), s.authType})
		n := len(seen)
		s.mu.Unlock()
		fakeConnected(s, true)
		if n == 1 {
			writeServersConfig(t, entry("mcp"))
			return "reconnect", nil
		}
		s.shutdownEv.Set()
		return "shutdown", nil
	}
	if err := s.start(context.Background(), &Rendered{Config: old}); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "run did not finish", func() bool { return taskDone(s) })
	want := [][3]string{{"https://mcp.example.test/sse", "https://mcp.example.test/sse", "oauth"},
		{"https://mcp.example.test/mcp", "https://mcp.example.test/mcp", "oauth"}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("seen %v", seen)
	}
}

func TestServerUnchangedOrMissingDefinitionKeepsRunningConfig(t *testing.T) {
	isolateHome(t)
	cfg := map[string]any{"url": "https://mcp.example.test/mcp", "headers": map[string]any{"X": "1"}, "connect_timeout": 5}
	s := newServerTask("srv", &tools.Env{Home: os.Getenv("ICEBATH_HOME")})
	r := &Rendered{Config: cfg}
	s.rendered, s.config = r, cfg
	writeServersConfig(t, "  srv:\n    url: https://mcp.example.test/mcp\n    headers:\n      X: \"1\"\n    connect_timeout: 9\n")
	if got := s.refreshRemoteConfig(context.Background(), r); got != r {
		t.Fatal("non endpoint keys triggered a rebind")
	}
	writeServersConfig(t, "  {}\n")
	if got := s.refreshRemoteConfig(context.Background(), r); got != r {
		t.Fatal("a missing definition replaced the running config")
	}
}
