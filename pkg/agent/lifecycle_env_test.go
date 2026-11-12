package agent

import (
	"context"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/tools"
)

// A turn binds the agent Env as the session Env only when its surface bound none, so tool and hook code read the turn key
func TestTurnContextCarriesTheSessionEnvOfTheTurn(t *testing.T) {
	agentEnv := &tools.Env{Values: &sync.Map{}}
	restore := tools.ScopedSessionKey(agentEnv, "agent:main:telegram:dm:42")
	defer restore()
	if got := tools.BoundSessionKey(tools.ToolEnvFrom(withLifecycleEnv(context.Background(), agentEnv))); got != "agent:main:telegram:dm:42" {
		t.Fatalf("turn without a surface env read %q", got)
	}
	surface := &tools.Env{Values: &sync.Map{}}
	undo := tools.ScopedSessionKey(surface, "tui:session")
	defer undo()
	if got := tools.ToolEnvFrom(withLifecycleEnv(tools.WithToolEnv(context.Background(), surface), agentEnv)); got != surface {
		t.Fatalf("surface env replaced by %v", got)
	}
}
