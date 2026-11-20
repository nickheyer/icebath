//go:build !windows

package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// Command printing whether each probed secret reached the child, then its home and cwd
const shellEnvProbe = `[ "$SVC_TOKEN" = served-secret ] && echo svc=served || echo svc=other; ` +
	`[ "$SKILL_TOKEN" = skill-secret ] && echo skill=served || echo skill=other; ` +
	`echo "launch=${LAUNCH_ONLY_TOKEN:-MISSING}"; echo "key=${OPENROUTER_API_KEY:-}"; echo "home=$ICEBATH_HOME"; pwd`

// Stdout lines of a probe run through shell.exec, failing the case on a nonzero exit
func shellProbe(t *testing.T, ctx context.Context, owner *AuthorityConnection, sid string) []string {
	t.Helper()
	ran := secretScopeCall(t, ctx, owner, "shell.exec", map[string]any{"session_id": sid, "command": shellEnvProbe})
	if fmt.Sprint(ran["code"]) != "0" {
		t.Fatalf("shell.exec ran %v", ran)
	}
	return strings.Split(strings.TrimSpace(ran["stdout"].(string)), "\n")
}

func TestShellExecUnderMultiplexRunsWithTheServedProfilesSecretsNeverTheLaunchResidue(t *testing.T) {
	ctx := context.Background()
	served := profileEnvServedHome(t, profileEnvLaunchHome(t))
	work := filepath.Join(t.TempDir(), "work")
	owner, sid := profileEnvOwner(t, ctx, served, work, true)
	want := []string{"svc=served", "skill=served", "launch=MISSING", "key=", "home=" + served, work}
	if got := shellProbe(t, ctx, owner, sid); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("stdout %q, want %q", got, want)
	}
}

func TestShellExecOfTheLaunchProfileKeepsItsOwnEnvCredential(t *testing.T) {
	ctx := context.Background()
	launch := profileEnvLaunchHome(t)
	envRefsWrite(t, launch, "config.yaml", "model: {default: m1, provider: custom, base_url: \"http://127.0.0.1:9/v1\", api_key: route-token}\n"+
		"platform_toolsets: {cli: [terminal]}\nterminal: {backend: local}\n")
	work := filepath.Join(t.TempDir(), "work")
	owner, sid := profileEnvOwner(t, ctx, launch, work, false)
	want := []string{"svc=other", "skill=other", "launch=launch-secret", "key=", "home=" + launch, work}
	if got := shellProbe(t, ctx, owner, sid); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("stdout %q, want %q", got, want)
	}
}
