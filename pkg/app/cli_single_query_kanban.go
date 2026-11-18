//go:build !icebath_core && !icebath_noextras

package app

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/goals"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Binds the kanban worker steps of a one shot query
func init() {
	kanbanRegisterWorker, kanbanGoalLoop, kanbanTaskImages = kanban.RegisterCurrentWorkerFromEnv, kanbanGoalLoopRun, kanbanTaskImageRefs
	kanbanRecoverTurns = func(ctx context.Context, rerun func(nudge string), last func() map[string]any, emit func(string)) error {
		_, err := kanban.RecoverFailedKanbanTurns(ctx, rerun, last, emit)
		return err
	}
}

// Drives a kanban goal mode worker through the goal loop after its first turn
func kanbanGoalLoopRun(c *CLI, firstResponse string, runTurn func(string) (any, error), log func(string)) error {
	taskID := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_TASK"))
	if taskID == "" {
		return nil
	}
	rawRunID := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_RUN_ID"))
	var workerRunID *int64
	if rawRunID != "" {
		if n, ok := pyIntOf(rawRunID); ok {
			id := int64(n)
			workerRunID = &id
		} else {
			pkgLog().Warn(fmt.Sprintf("invalid ICEBATH_KANBAN_RUN_ID=%s", errchain.Show(rawRunID)))
		}
	}
	var task *kanban.Task
	err := kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		t, gerr := kanban.GetTask(c.ctx, conn, taskID)
		task = t
		return gerr
	})
	if err != nil {
		return err
	}
	if task == nil {
		return nil
	}
	var parts []string
	if task.Title != "" {
		parts = append(parts, task.Title)
	}
	if task.Body != nil && *task.Body != "" {
		parts = append(parts, *task.Body)
	}
	goalText := strings.TrimSpace(strings.Join(parts, "\n\n"))
	if goalText == "" {
		return nil
	}
	quietTurn := func(prompt string) (any, error) {
		result, rerr := c.agentRef().RunConversation(c.turnCtx, prompt, agent.RunConversationOptions{ConversationHistory: c.conversationHistory})
		if rerr != nil {
			return nil, rerr
		}
		syncCLISessionIDFromAgent(c)
		resp := cliStr(result["final_response"], "")
		if resp != "" {
			fmt.Fprintln(c.console.Out, resp)
		}
		return map[string]any{"response": resp, "failed": cliTruthy(result["failed"]), "failure_reason": result["failure_reason"]}, nil
	}
	taskStatus := func() (*string, error) {
		var status *string
		err := kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			s, serr := kanban.GoalRunStatus(c.ctx, conn, taskID, workerRunID)
			status = s
			return serr
		})
		return status, err
	}
	block := func(reason string) error {
		return kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
			return kanban.BlockTask(c.ctx, conn, taskID, reason, workerRunID)
		})
	}
	if runTurn == nil {
		runTurn = quietTurn
	}
	if log == nil {
		log = func(m string) { pkgLog().Info(m) }
	}
	maxTurns := goals.DefaultGoalMaxTurns
	if goalMax := kanban.TaskGoalMaxTurns(task); goalMax != nil && *goalMax > 0 {
		maxTurns = int(*goalMax)
	}
	_, err = goals.RunKanbanGoalLoop(c.ctx, goals.GoalLoopOptions{TaskID: taskID, GoalText: goalText, RunTurn: runTurn,
		TaskStatus: taskStatus, Block: block, MaxTurns: maxTurns, FirstResponse: firstResponse, Log: log})
	return err
}

// Adds the image paths and URLs of a Kanban worker's task body to the first turn's attachments
func kanbanTaskImageRefs(c *CLI, images *[]string) []string {
	imageURLs := []string{}
	taskID := strings.TrimSpace(os.Getenv("ICEBATH_KANBAN_TASK"))
	if taskID == "" {
		return imageURLs
	}
	var task *kanban.Task
	err := kanban.ConnectClosing(c.ctx, "", nil, func(conn state.SQLiteConn) error {
		t, gerr := kanban.GetTask(c.ctx, conn, taskID)
		task = t
		return gerr
	})
	if err != nil {
		pkgLog().Debug(fmt.Sprintf("kanban image-ref extraction failed: %v", err))
		return imageURLs
	}
	if task == nil || task.Body == nil || *task.Body == "" {
		return imageURLs
	}
	paths, urls := tools.ExtractImageRefs(*task.Body)
	seen := map[string]bool{}
	for _, p := range *images {
		seen[p] = true
	}
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			*images = append(*images, p)
		}
	}
	return append(imageURLs, urls...)
}
