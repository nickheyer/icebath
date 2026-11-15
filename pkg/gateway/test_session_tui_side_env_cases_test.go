package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSideScopeUnderMultiplexGivesTheTerminalRunEnvTheServedProfilesSecrets(t *testing.T) {
	ctx := context.Background()
	served := profileEnvServedHome(t, profileEnvLaunchHome(t))
	owner, sid := profileEnvOwner(t, ctx, served, filepath.Join(t.TempDir(), "work"), true)
	a := owner.authority
	live, err := liveSession(a, sid)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil || policy == nil {
		t.Fatalf("policy %v %v", policy, err)
	}
	var runEnv map[string]string
	err = SideScope(ctx, a, policy, "bg_side", func(_ context.Context, env *tools.Env) error {
		var buildErr error
		runEnv, buildErr = tools.MakeRunEnv(context.Background(), env, nil)
		return buildErr
	})
	if err != nil {
		t.Fatalf("side task terminal env: %v", err)
	}
	if runEnv["SKILL_TOKEN"] != "skill-secret" || runEnv["SVC_TOKEN"] != "served-secret" {
		t.Fatalf("side task terminal env SKILL_TOKEN %q SVC_TOKEN %q", runEnv["SKILL_TOKEN"], runEnv["SVC_TOKEN"])
	}
	for _, name := range []string{"LAUNCH_ONLY_TOKEN", "OPENROUTER_API_KEY"} {
		if value, present := runEnv[name]; present {
			t.Fatalf("side task terminal env carries %s=%q", name, value)
		}
	}
}
