package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

func TestSessionTerminalScopeGivesTheTurnTheClientCwd(t *testing.T) {
	h, project := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	session := &tools.Env{Home: h, Values: &sync.Map{}}
	tr := &TurnRunner{ctx: &TurnContext{sessionEnv: session}, turnCtx: context.Background()}
	launch, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := tools.ResolveAgentCwd(tr.toolsEnv()); err != nil || got != launch {
		t.Fatalf("unscoped turn cwd %q %v, want the launch dir %q", got, err, launch)
	}
	scope := &tools.TerminalScope{Policy: tools.TerminalPolicy{"TERMINAL_ENV": "local", "TERMINAL_CWD": project}}
	restore := tr.bindPolicyScope(scope)
	if got, err := tools.ResolveAgentCwd(tr.toolsEnv()); err != nil || got != project {
		t.Fatalf("scoped turn cwd %q %v, want the policy cwd %q", got, err, project)
	}
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sid := "s1"
	if snap := tools.CaptureLocalEditSnapshot(tr.toolsEnv(), "write_file", map[string]any{"path": "notes.txt"}, &sid); snap == nil {
		t.Fatal("the edit snapshot of a relative path does not resolve under the client cwd")
	}
	if session.TerminalScope != nil {
		t.Fatal("the shared session env itself took the policy scope")
	}
	agentEnv := turnAgentEnv(tools.SetTerminalScope(context.Background(), scope), tr.ctx)
	if got, err := tools.ResolveAgentCwd(agentEnv); err != nil || got != project || agentEnv.Values != session.Values {
		t.Fatalf("turn agent cwd %q %v, want the policy cwd %q over the shared session values", got, err, project)
	}
	restore()
	if tr.toolsEnv() != session {
		t.Fatal("restore leaves the policy scope bound past the turn")
	}
}
