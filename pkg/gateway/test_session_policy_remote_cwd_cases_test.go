package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

const remoteCwdLaunch = "\x00launch"

func TestTerminalCwdFollowsTheBackend(t *testing.T) {
	cases := []struct {
		terminal map[string]any
		params   map[string]any
		cwd      string
		hostCwd  any
	}{
		{map[string]any{"backend": "ssh", "cwd": "/srv/app"}, map[string]any{}, "/srv/app", nil},
		{map[string]any{"backend": "ssh"}, map[string]any{}, "~", nil},
		{map[string]any{"backend": "docker", "cwd": "/workspace/app"}, map[string]any{}, "/workspace/app", nil},
		{map[string]any{"backend": "docker", "docker_mount_cwd_to_workspace": true}, map[string]any{}, "/workspace", remoteCwdLaunch},
		{map[string]any{"backend": "local"}, map[string]any{}, remoteCwdLaunch, nil},
		{map[string]any{"backend": "ssh", "cwd": "/srv/app"}, map[string]any{"cwd_explicit": true}, remoteCwdLaunch, nil},
	}
	for _, tc := range cases {
		t.Run("test_terminal_cwd_follows_the_backend", func(t *testing.T) {
			tmp := t.TempDir()
			dir := filepath.Join(tmp, "home")
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(map[string]any{"terminal": tc.terminal})
			if err := os.WriteFile(filepath.Join(dir, "config.yaml"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ICEBATH_HOME", dir)
			launch := filepath.Join(tmp, "project")
			if err := os.Mkdir(launch, 0o755); err != nil {
				t.Fatal(err)
			}
			params := map[string]any{"source": "tui", "cwd": launch, "model": "m"}
			for k, v := range tc.params {
				params[k] = v
			}
			ctx := context.Background()
			policy, err := buildPolicy(ctx, params, map[string]any{}, buildPolicyOptions{})
			if err != nil {
				t.Fatal(err)
			}
			restored, err := restorePolicy(policy.asDict())
			if err != nil {
				t.Fatal(err)
			}
			var config map[string]any
			err = policyScope(ctx, restored, nil, func(scoped context.Context) error {
				var cfgErr error
				config, cfgErr = tools.GetEnvConfig(&tools.Env{Home: home.Dir(scoped), TerminalScope: tools.GetTerminalScope(scoped)})
				return cfgErr
			})
			if err != nil {
				t.Fatal(err)
			}
			wantCwd, wantHost := tc.cwd, tc.hostCwd
			if wantCwd == remoteCwdLaunch {
				wantCwd = launch
			}
			if wantHost == remoteCwdLaunch {
				wantHost = launch
			}
			if config["cwd"] != wantCwd || config["host_cwd"] != wantHost {
				t.Fatalf("terminal %v params %v: (%v, %v), want (%v, %v)", tc.terminal, tc.params, config["cwd"], config["host_cwd"], wantCwd, wantHost)
			}
		})
	}
}
