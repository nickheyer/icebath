//go:build !windows

package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestPreCommandShellHookUnderMultiplexRunsWithTheServedProfilesSecretsWithoutFailingClosed(t *testing.T) {
	ctx := context.Background()
	served := profileEnvServedHome(t, profileEnvLaunchHome(t))
	owner, sid := profileEnvOwner(t, ctx, served, filepath.Join(t.TempDir(), "work"), true)
	record := filepath.Join(t.TempDir(), "hook.out")
	hook := filepath.Join(t.TempDir(), "hook.sh")
	probe := "#!/bin/sh\n{ " + strings.ReplaceAll(shellEnvProbe, "; pwd", "") + "; } > '" + record + "'\n"
	if err := os.WriteFile(hook, []byte(probe), 0o755); err != nil {
		t.Fatal(err)
	}
	plugins.ResetPluginManagersForTests()
	plugins.ResetShellHooksForTests()
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		plugins.ResetShellHooksForTests()
	})
	hooks := map[string]any{"hooks": map[string]any{"pre_command": []any{map[string]any{"command": hook}}}}
	if registered, err := plugins.RegisterShellHooksFromConfig(ProfileRuntimeScope(ctx, served, true), hooks, true, false); err != nil || len(registered) != 1 {
		t.Fatalf("registered %v %v", registered, err)
	}
	done, err := queuePreCommand(ctx, owner.authority, SessionRef{ProfileID: owner.authority.ProfileID, SessionID: sid}, "status", "status", "")
	if err != nil || done == nil {
		t.Fatalf("pre_command not queued %v %v", done, err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("pre_command delivery did not settle within 30s")
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the pre_command hook never ran: %v", err)
	}
	want := []string{"svc=served", "skill=served", "launch=launch-secret", "key=", "home=" + served}
	if got := strings.Split(strings.TrimSpace(string(raw)), "\n"); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("hook saw %q, want %q", got, want)
	}
}
