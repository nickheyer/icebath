package plugins

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

type policyKey struct{}

// test_plugin_gateway_session_parity.py test_plugin_context_reads_the_calling_sessions_cwd_and_surface plus the terminal cwd tail
func TestPluginContextReadsTheCallingSessionsCwdAndSurface(t *testing.T) {
	previous := installedServices()
	InstallServices(&Services{ActiveSessionPolicy: func(ctx context.Context) (string, string, bool) {
		ws, ok := ctx.Value(policyKey{}).(string)
		return "tui", ws, ok
	}})
	t.Cleanup(func() { InstallServices(previous) })
	c := newContext(&Manifest{Name: "probe", Key: "probe", Source: "user"}, NewManager(t.TempDir()))
	workspace := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	local := &tools.Env{Values: &sync.Map{}}
	tools.SetLogicalSessionCwd(local, workspace)
	ctx := context.WithValue(tools.WithToolEnv(context.Background(), local), policyKey{}, workspace)
	cwd, err := c.SessionCwd(ctx)
	surface, serr := c.SessionSurface(ctx)
	if err != nil || serr != nil || cwd != workspace || surface != "tui" {
		t.Fatalf("local turn = %q %q %v %v", cwd, surface, err, serr)
	}
	tools.SetLogicalSessionCwd(local, "")
	if cwd, err := c.SessionCwd(ctx); err != nil || cwd != workspace {
		t.Fatalf("policy cwd = %q %v", cwd, err)
	}
	messaging := &tools.Env{Values: &sync.Map{}}
	tools.BindSessionContext(messaging, map[string]string{"ICEBATH_SESSION_PLATFORM": "telegram", "ICEBATH_SESSION_KEY": "agent:main:telegram:dm:1"})
	mctx := tools.WithToolEnv(context.Background(), messaging)
	if surface, err := c.SessionSurface(mctx); err != nil || surface != "telegram" || SessionKey(mctx) != "agent:main:telegram:dm:1" {
		t.Fatalf("messaging turn = %q %q %v", surface, SessionKey(mctx), err)
	}
	terminal := t.TempDir()
	t.Setenv("TERMINAL_CWD", "  "+terminal+"  ")
	bare := tools.WithToolEnv(context.Background(), &tools.Env{Values: &sync.Map{}, Home: t.TempDir()})
	if cwd, err := c.SessionCwd(bare); err != nil || cwd != terminal || SessionKey(bare) != "" || SessionSurface(bare) != "" {
		t.Fatalf("terminal tail = %q %v", cwd, err)
	}
	c.manager.cliRef = struct{ CLIHost }{}
	launch, _ := os.Getwd()
	cwd, err = c.SessionCwd(ctx)
	surface, serr = c.SessionSurface(ctx)
	if err != nil || serr != nil || cwd != launch || surface != "cli" {
		t.Fatalf("classic cli = %q %q %v %v", cwd, surface, err, serr)
	}
}
