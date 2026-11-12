package gateway

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// test_session_edit_policy.py test_the_owners_edit_guard_enforces_the_sessions_policy
func TestTheOwnersEditGuardEnforcesTheSessionsPolicy(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	cases := []struct {
		id, name, policy string
		asks             bool
	}{
		{"edit-ask", "edit.txt", "ask", true},
		{"edit-workspace", "edit.txt", "workspace_session", false},
		{"env-workspace", ".env", "workspace_session", true},
		{"edit-session", "edit.txt", "session", false},
		{"git-session", ".git/config", "session", true},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			route := "edit-policy-" + tc.policy + "-" + tc.name
			var mu sync.Mutex
			var asked []map[string]any
			tools.RegisterGatewayNotify(route, func(data map[string]any) error {
				mu.Lock()
				asked = append(asked, data)
				mu.Unlock()
				go func() {
					deadline := time.Now().Add(10 * time.Second)
					for time.Now().Before(deadline) && tools.ResolveGatewayApproval(route, "deny", false, "", "") == 0 {
						time.Sleep(5 * time.Millisecond)
					}
				}()
				return nil
			})
			defer tools.UnregisterGatewayNotify(route)
			frozen, err := buildPolicy(ctx, map[string]any{"source": "acp", "cwd": dir, "model": "fixture"}, map[string]any{}, buildPolicyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			frozen.EditApprovalPolicy = tc.policy
			path := filepath.Join(dir, tc.name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			env := &tools.Env{Values: &sync.Map{}}
			tools.SetCurrentSessionKey(env, route)
			var result string
			done := make(chan error, 1)
			go func() {
				done <- policyScope(ctx, frozen, nil, func(scoped context.Context) error {
					var err error
					result, err = tools.MaybeRequireEditApproval(scoped, env, "write_file", map[string]any{"path": path, "content": "new"})
					return err
				})
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the edit guard did not answer")
			}
			mu.Lock()
			got := len(asked) > 0
			mu.Unlock()
			if got != tc.asks {
				t.Fatalf("%s %s asked %v", tc.policy, tc.name, asked)
			}
			if (result == "") != !tc.asks {
				t.Fatalf("result %q", result)
			}
		})
	}
}
