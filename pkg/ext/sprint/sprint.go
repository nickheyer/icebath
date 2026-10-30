// Sprint extension, a project manager, a planner, a lead and engineers tackling one request as a team
package sprint

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/protocol"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/ui/keys"
)

type extension struct{}

func (extension) Manifest() ext.Manifest {
	return ext.Manifest{Name: Name, Version: "1", Description: "sprint team workflow, a project manager settles the request, a planner cuts it into tickets and a lead with engineers builds it"}
}

// Declares the kinds, tools, budget hook, workflow, board and settings of the sprint team
func (extension) Attach(h *ext.Host) error {
	if err := LoadError(); err != nil {
		return err
	}
	for _, k := range Current().Kinds() {
		h.Kind(k)
	}
	h.Tools(func(a *harness.App) ([]tool.Tool, error) { return sprintTools(a), nil })
	h.Hook(budgetHook)
	h.Workflow(workflow())
	h.Context(Context, "sprint board")
	h.Action(keys.Action{ID: ActionOpen, Context: Context, Label: "open", Doc: "open the session of the member, or the detail of the sprint or ticket", Default: []string{"enter", "o"}})
	h.Action(keys.Action{ID: ActionStop, Context: Context, Label: "stop", Doc: "stop the sprint", Default: []string{"x"}})
	h.Action(keys.Action{ID: ActionSettings, Context: Context, Label: "settings", Doc: "open the sprint settings", Default: []string{"s"}})
	h.Panel(ext.Panel{Key: BoardKey, Title: "Sprints", Detail: "every sprint with its team and tickets", New: newBoard})
	h.Panel(ext.Panel{Key: SettingsKey, Title: "Sprint settings", Detail: "models, team size and time limits of the sprint workflow", New: newSettingsCard, Settings: true})
	return nil
}

// Role a kind name stands for, false for a kind of another extension
func roleOfKind(kind string) (string, bool) {
	role, ok := strings.CutPrefix(kind, Name+"-")
	if !ok || !contains(Roles, role) {
		return "", false
	}
	return role, true
}

// Hook that ends a run once a sprint role has spent its token budget on the session, nil for other agents
func budgetHook(info harness.AgentInfo) agent.Hook {
	role, ok := roleOfKind(info.Kind.Name)
	if !ok {
		return nil
	}
	cfg := Current()
	budget := cfg.Role(role).Budget
	if budget == 0 {
		return nil
	}
	sess := info.Session
	return agent.Hooks{AfterTurnFn: func(ctx context.Context, resp protocol.Response) error {
		if spent := sess.Usage().Total(); spent > budget {
			return fmt.Errorf("sprint %s spent %d tokens, past its budget of %d", role, spent, budget)
		}
		return nil
	}}
}

func init() { ext.Register(extension{}) }
