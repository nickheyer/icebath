package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

type storeCaseClient struct {
	mu      sync.Mutex
	events  *gateway.EventQueue
	creates []map[string]any
}

func (c *storeCaseClient) RPCTimeout(_ context.Context, method string, params map[string]any, _ time.Duration) (any, error) {
	if method != "session.create" {
		return map[string]any{"status": "ok", "detached": true}, nil
	}
	c.mu.Lock()
	c.creates = append(c.creates, params)
	c.mu.Unlock()
	return map[string]any{"session_id": "local-new", "stored_session_id": "local-new", "messages": []any{}, "revision": 3,
		"execution_generation": 0, "subscription_id": "sub-local-new", "info": map[string]any{"model": "fake-model"}}, nil
}

func (c *storeCaseClient) EventQueue() *gateway.EventQueue { return c.events }
func (c *storeCaseClient) SendFrame(string) error          { return nil }
func (c *storeCaseClient) Close() error                    { return nil }

func TestStartAndQuitClosesTheGatewaySessionWithNoAgent(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	host := &CLI{ctx: context.Background(), sessionID: "local-new", gatewaySessionOpen: true}
	if got := host.closingSessionID(); got != "local-new" {
		t.Fatalf("closing id %q", got)
	}
	host.gatewaySessionOpen = false
	if got := host.closingSessionID(); got != "" {
		t.Fatalf("closing id %q with no gateway session", got)
	}
	host.agent = offlineAgent(t, &CLI{ctx: context.Background(), sessionID: "local-agent"})
	if got := host.closingSessionID(); got != "local-agent" {
		t.Fatalf("closing id %q with an agent", got)
	}
}

func TestNewSessionIdOffTheGatewayIsMainsMint(t *testing.T) {
	host := &CLI{}
	minted, err := host.newSessionID(time.Date(2026, 10, 12, 1, 2, 3, 0, time.Local), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(minted, "20261012_010203_") {
		t.Fatalf("minted %q", minted)
	}
}

func TestAProjectResumedAfterWIsSentNotTheWWorktree(t *testing.T) {
	for _, tc := range []struct{ backend, mount string }{{"local", ""}, {"docker", "true"}} {
		t.Run(tc.backend, func(t *testing.T) {
			ctx := context.Background()
			t.Setenv("ICEBATH_HOME", t.TempDir())
			tmp := t.TempDir()
			tree, projectB := filepath.Join(tmp, "a", ".worktrees", "wt"), filepath.Join(tmp, "b")
			for _, dir := range []string{tree, projectB} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(filepath.Join(tmp, "a"))
			activeWorktreeMu.Lock()
			previous := activeWorktree
			activeWorktree = map[string]string{"path": tree}
			activeWorktreeMu.Unlock()
			t.Cleanup(func() {
				activeWorktreeMu.Lock()
				activeWorktree = previous
				activeWorktreeMu.Unlock()
			})
			t.Setenv("TERMINAL_CWD", tree)
			t.Setenv("TERMINAL_ENV", tc.backend)
			if tc.mount != "" {
				t.Setenv("TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE", tc.mount)
			}
			for _, name := range []string{"ICEBATH_SESSION_SOURCE", "ICEBATH_SESSION_SOURCE_EXPLICIT", "ICEBATH_SINGLE_QUERY_SESSION"} {
				t.Setenv(name, "")
				os.Unsetenv(name)
			}
			client := &storeCaseClient{events: gateway.NewEventQueue(0)}
			conn := gatewaycli.NewGatewayConnection(func(context.Context) (gatewaycli.ConnectionClient, error) { return client, nil })
			if err := conn.Start(ctx, 5*time.Second); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close(ctx) })
			(&CLI{ctx: ctx}).restoreSessionCwd(map[string]any{"cwd": projectB}, true)
			db := gatewaycli.NewGatewaySessionStore(conn)
			if _, err := db.Create(ctx, map[string]any{"toolsets": []any{"file"}}, "", false); err != nil {
				t.Fatal(err)
			}
			want, err := pypath.Resolve(projectB)
			if err != nil {
				t.Fatal(err)
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if got := client.creates[len(client.creates)-1]["cwd"]; got != want {
				t.Fatalf("cwd %v, want %s", got, want)
			}
		})
	}
}
