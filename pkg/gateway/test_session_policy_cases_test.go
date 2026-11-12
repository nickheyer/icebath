package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Session id a session.create dispatch returned, else the refusal message it carried
func dispatchedCreate(t *testing.T, ctx context.Context, connection *AuthorityConnection, params map[string]any) string {
	t.Helper()
	request, _ := pyjson.Ordered(params).(*pyjson.Object)
	reply, err := connection.Dispatch(ctx, pyjson.ObjectOf("id", int64(1), "method", "session.create", "params", request))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := pyjson.Plain(reply).(map[string]any)
	if result, ok := plain["result"].(map[string]any); ok {
		if sid, _ := result["session_id"].(string); sid != "" {
			return sid
		}
	}
	failure, ok := plain["error"].(map[string]any)
	if !ok {
		t.Fatalf("reply %v carries neither a session nor an error", plain)
	}
	message, _ := failure["message"].(string)
	return message
}

// Recover runner that also keeps no cached agent for any route
type launchTestRunner struct {
	*recoverTestRunner
}

func (launchTestRunner) CachedAgentFor(string) any { return nil }

func TestLaunchOptionsReachTheOwnerFrozenAndHooksConsented(t *testing.T) {
	ctx, a, runner := newRecoverAuthority(t)
	a.Runner = launchTestRunner{runner}
	installPrefillLoaders(t)
	home := os.Getenv("ICEBATH_HOME")
	a.ProfileID = home
	skill := filepath.Join(home, "skills", "launch-fixture")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, skill, "SKILL.md", "---\nname: launch-fixture\ndescription: fixture\n---\n\nLAUNCH_SKILL_BODY\n")
	t.Setenv("ICEBATH_ACCEPT_HOOKS", "")
	os.Unsetenv("ICEBATH_ACCEPT_HOOKS")
	hook := filepath.Join(t.TempDir(), "hook.sh")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	envRefsWrite(t, home, "config.yaml", "model: fixture\nplatform_toolsets:\n  cli: []\nhooks:\n  pre_tool_call:\n  - command: "+hook+"\n")
	plugins.ResetPluginManagersForTests()
	plugins.ResetShellHooksForTests()
	t.Cleanup(func() {
		plugins.ResetPluginManagersForTests()
		plugins.ResetShellHooksForTests()
	})
	connection, err := NewAuthorityConnection(a, wireContractTransport{}, map[string]any{"user_id": "human"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(ctx) })
	cwd := t.TempDir()
	create := func(requestID string, params map[string]any) string {
		full := map[string]any{"request_id": requestID, "source": "tui", "cwd": cwd}
		for key, value := range params {
			full[key] = value
		}
		return dispatchedCreate(t, ctx, connection, full)
	}
	frozen := func(sid string) *LocalSessionPolicy {
		raw, _ := recoverReceipt(t, ctx, a.DB, sid).Get("policy")
		policy, err := restorePolicy(raw)
		if err != nil {
			t.Fatal(err)
		}
		return policy
	}
	launch := map[string]any{"skills": []any{"launch-fixture", "missing-typo"}, "checkpoints": true, "pass_session_id": true, "accept_hooks": true}
	sid := create("launch", launch)
	policy := frozen(sid)
	if policy.SkillsPrompt == nil || !strings.Contains(*policy.SkillsPrompt, "LAUNCH_SKILL_BODY") || !policy.PassSessionID() || !policy.CheckpointsEnabled() {
		t.Fatalf("launch policy %+v", policy)
	}
	allowlist, err := os.ReadFile(filepath.Join(home, plugins.ShellHookAllowlistFilename))
	if err != nil || !strings.Contains(string(allowlist), hook) {
		t.Fatalf("allowlist %q %v", allowlist, err)
	}
	if hooks := plugins.GetPluginManager(ctx).IterHookCallbacks("pre_tool_call"); len(hooks) != 1 {
		t.Fatalf("pre_tool_call hooks %d", len(hooks))
	}
	if again := create("launch", launch); again != sid {
		t.Fatalf("retry %q, want %q", again, sid)
	}
	if hooks := plugins.GetPluginManager(ctx).IterHookCallbacks("pre_tool_call"); len(hooks) != 1 {
		t.Fatalf("pre_tool_call hooks after retry %d", len(hooks))
	}
	plain := frozen(create("plain", map[string]any{}))
	if plain.SkillsPrompt != nil || plain.PassSessionID() || plain.CheckpointsEnabled() {
		t.Fatalf("plain policy %+v", plain)
	}
	for _, params := range []map[string]any{
		{"skills": []any{"missing-typo"}}, {"skills": "launch-fixture"}, {"checkpoints": "yes"},
		{"model": "m", "safe_mode": true, "skills": []any{"launch-fixture"}},
	} {
		text, _ := pyjson.Dumps(params)
		if got := create("refused-"+text, params); got != "unknown_skill" && got != "invalid_params" {
			t.Fatalf("params %v gave %q", params, got)
		}
	}
	if got := create("safe", map[string]any{"model": "m", "safe_mode": true, "accept_hooks": true}); !strings.HasPrefix(got, "local-") {
		t.Fatalf("safe launch %q", got)
	}
}
