package app

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/linktier"
)

// Kanban worker steps of a one shot query, bound by the extras tier that carries kanban and nil in a build without it
var (
	kanbanRegisterWorker func(ctx context.Context) bool
	kanbanRecoverTurns   func(ctx context.Context, rerun func(nudge string), last func() map[string]any, emit func(string)) error
	kanbanGoalLoop       func(c *CLI, firstResponse string, runTurn func(string) (any, error), log func(string)) error
	kanbanTaskImages     func(c *CLI, images *[]string) []string
)

// Registers this run as its card's worker, false when the card was reclaimed, a refusal and exit 1 without kanban
func registerKanbanWorker(c *CLI) bool {
	if kanbanRegisterWorker == nil {
		fmt.Fprintln(stderrWriter(), linktier.Missing("a kanban worker run", linktier.Extras))
		exitSingleQuery(1)
	}
	return kanbanRegisterWorker(c.ctx)
}

// Reruns a kanban worker's failed turns with a nudge, nothing for an ordinary run or a build without kanban
func recoverKanbanTurns(ctx context.Context, rerun func(nudge string), last func() map[string]any, emit func(string)) error {
	if kanbanRecoverTurns == nil {
		return nil
	}
	return kanbanRecoverTurns(ctx, rerun, last, emit)
}

// Drives a kanban worker's goal loop after its first reply, nothing for an ordinary run or a build without kanban
func runKanbanGoalLoopQ(c *CLI, firstResponse string, runTurn func(string) (any, error), log func(string)) error {
	if kanbanGoalLoop == nil {
		return nil
	}
	return kanbanGoalLoop(c, firstResponse, runTurn, log)
}

// Image paths and URLs of a kanban worker's task body added to the first turn, none without kanban
func collectKanbanTaskImages(c *CLI, images *[]string) []string {
	if kanbanTaskImages == nil {
		return []string{}
	}
	return kanbanTaskImages(c, images)
}
