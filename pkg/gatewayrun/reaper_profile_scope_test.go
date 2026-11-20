package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestInterruptReaperKeepsServedProfileHome(t *testing.T) {
	tmp := t.TempDir()
	launchHome, servedHome := filepath.Join(tmp, "launch"), filepath.Join(tmp, "served")
	for _, d := range []string{launchHome, servedHome} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", launchHome)
	seen := make(chan string, 1)
	original := killStartedSince
	killStartedSince = func(ctx context.Context, env *tools.Env, taskID string, baseline map[string]bool, source string) int {
		if source != "gateway_turn_interrupt" {
			t.Errorf("source %s", source)
		}
		seen <- home.Dir(ctx)
		return 0
	}
	t.Cleanup(func() { killStartedSince = original })
	r := allocGatewayRunner()
	running := &agent.AIAgent{}
	r.sessionState("served-session").turn.agent = running
	r.stampTurnProcess(running, "served-session", map[string]bool{"proc_existing": true})
	ctx := home.WithOverride(context.Background(), servedHome)
	r.interruptRunningTurn(ctx, "served-session", "test interrupt", "test", "")
	select {
	case got := <-seen:
		if got != servedHome {
			t.Fatalf("reaper ran under %s", got)
		}
	case <-time.After(time.Second):
		t.Fatal("reaper thread did not run")
	}
}
