package kanban

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/goals"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Block kinds a goal mode task may still use, the genuine external blockers
var goalModeBlockAllowedKinds = map[string]bool{"dependency": true, "needs_input": true}

// Judge rejection guidance per tool and verdict, reason and tid filled in
var goalGateMessages = map[string]map[string]string{
	"kanban_complete": {
		"blocked": "Goal completion rejected: judge ruled the goal unachievable — {reason}. The task " +
			"will NOT complete silently. Either re-scope the task with kanban_edit, or record " +
			"the block with kanban_block and hand the decision to a human / reviewer.",
		"continue": "Goal completion rejected by judge: {reason}. To proceed, either: (1) provide " +
			"explicit acceptance evidence in your summary matching the task's criteria, or (2) " +
			"create continuation tasks with parents=[{tid}] and keep this task alive.",
	},
	"kanban_request_review": {
		"blocked": "Goal review handoff rejected: judge ruled the goal unachievable — {reason}. " +
			"Record the block with kanban_block instead of requesting review.",
		"continue": "Goal review handoff rejected by judge: {reason}. Provide acceptance evidence " +
			"matching the card before requesting review.",
	},
}

var (
	judgeGoalFn          = goals.JudgeGoal
	goalJudgeAvailableFn = goalJudgeAvailable
)

// Whether a goal judge model is reachable, the gate being enforced only then
func goalJudgeAvailable(ctx context.Context) (available bool) {
	defer func() {
		if recover() != nil {
			available = false
		}
	}()
	client, model, err := provider.GetTextAuxiliaryClient(ctx, "goal_judge", nil)
	if err != nil {
		return false
	}
	return client != nil && model != ""
}

// Rejects a goal mode handoff the judge does not rule done, failing open when the judge breaks or is unreachable
func goalGate(k *kanbanCall, toolName string, task *Task, tid, evidence string) (err error) {
	if task == nil || !task.GoalMode || !goalJudgeAvailableFn(k.ctx) {
		return nil
	}
	var result goals.JudgeResult
	judged := func() (failed any) {
		defer func() { failed = recover() }()
		ctx := k.ctx
		if provider.PortalAffinityScope(ctx) == "" {
			ctx = provider.WithPortalAffinityScope(ctx, "kanban:"+tid)
		}
		body := ""
		if task.Body != nil {
			body = *task.Body
		}
		result = judgeGoalFn(ctx, pystr.Strip(task.Title+"\n\n"+body), pystr.Strip(evidence), goals.JudgeOptions{})
		return nil
	}()
	if judged != nil {
		kanbanToolsLog.Warn(fmt.Sprintf("goal judge check failed, allowing lifecycle handoff: %v", judged), "exc_info", fmt.Sprint(judged))
		return nil
	}
	if result.TransportFailed {
		kanbanToolsLog.Warn(fmt.Sprintf("goal judge unreachable (%s), allowing lifecycle handoff", result.Reason))
		return nil
	}
	if result.Verdict == "done" {
		return nil
	}
	key := "continue"
	if result.Verdict == "blocked" {
		key = "blocked"
	}
	message := strings.NewReplacer("{reason}", result.Reason, "{tid}", tid).Replace(goalGateMessages[toolName][key])
	return reject(message)
}

const (
	autoHeartbeatMinIntervalSeconds = 60.0
	commentPollMinIntervalSeconds   = 6.0
)

var (
	bridgeMu                 sync.Mutex
	autoHeartbeatLastAttempt float64
	autoHeartbeatFenceWarned bool
	commentPollLastAttempt   float64
	commentWatermark         = map[string]int64{}
	bridgeMonotonic          = pythread.Monotonic
)

// Extends the claim and heartbeat of the current worker at most once a minute, true when both writes land
func HeartbeatCurrentWorkerFromEnv(ctx context.Context) (beat bool) {
	tid := os.Getenv("ICEBATH_KANBAN_TASK")
	now := bridgeMonotonic()
	bridgeMu.Lock()
	tooSoon := now-autoHeartbeatLastAttempt < autoHeartbeatMinIntervalSeconds
	bridgeMu.Unlock()
	if tid == "" || tooSoon {
		return false
	}
	if isDelegatedChildContext(ctx, tools.ToolEnvFrom(ctx)) {
		return false
	}
	bridgeMu.Lock()
	autoHeartbeatLastAttempt = now
	bridgeMu.Unlock()
	bridgeFailed := func(cause any) bool {
		kanbanToolsLog.Debug("auto-heartbeat: bridge failed", "exc_info", fmt.Sprint(cause))
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			beat = bridgeFailed(r)
		}
	}()
	conn, err := Connect(ctx, "", nil)
	if err != nil {
		return bridgeFailed(err)
	}
	defer func() { _ = conn.Close() }()
	var claimer *string
	if lock, set := os.LookupEnv("ICEBATH_KANBAN_CLAIM_LOCK"); set {
		claimer = &lock
	}
	ops := []struct {
		name string
		run  func() (bool, error)
	}{
		{"heartbeat_claim", func() (bool, error) { return HeartbeatClaim(ctx, conn, tid, nil, claimer) }},
		{"heartbeat_worker", func() (bool, error) { return HeartbeatWorker(ctx, conn, tid, nil, expectedRunID(tid)) }},
	}
	succeeded := true
	for _, op := range ops {
		ok, err := runBridgeOp(op.run)
		if err == nil {
			succeeded = ok && succeeded
			continue
		}
		succeeded = false
		var fenced *PermissionError
		if !errors.As(err, &fenced) {
			kanbanToolsLog.Debug(fmt.Sprintf("auto-heartbeat: %s failed", op.name), "exc_info", excText(err))
			continue
		}
		bridgeMu.Lock()
		warned := autoHeartbeatFenceWarned
		autoHeartbeatFenceWarned = true
		bridgeMu.Unlock()
		if !warned {
			kanbanToolsLog.Warn(fmt.Sprintf("kanban auto-heartbeat for task %s refused (%s): this process carries "+
				"ICEBATH_DELEGATED_CHILD_CONTEXT together with ICEBATH_KANBAN_TASK, so the board "+
				"treats it as a delegate_task descendant and its claim will not be extended by "+
				"activity. Only the dispatcher's own spawn grants worker scope; do not copy a "+
				"worker's environment into a hand-launched process.", tid, excText(err)))
		}
	}
	return succeeded
}

// Runs one board write of the heartbeat bridge, a panic becoming its error
func runBridgeOp(run func() (bool, error)) (ok bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if e, isErr := r.(error); isErr {
				ok, err = false, e
				return
			}
			ok, err = false, &RuntimeError{fmt.Sprint(r)}
		}
	}()
	return run()
}

// kanbanSteerer is the live agent surface operator notes are steered into
type kanbanSteerer interface{ Steer(text string) bool }

// Steers operator comments added since the last poll into the running agent, true when a note was injected
func InjectNewCommentsFromEnv(ctx context.Context, agent any) (injected bool) {
	env := tools.ToolEnvFrom(ctx)
	tid := ""
	if isDispatcherOwnedWorker(ctx, env) {
		tid = os.Getenv("ICEBATH_KANBAN_TASK")
	}
	steerer, canSteer := agent.(kanbanSteerer)
	now := bridgeMonotonic()
	bridgeMu.Lock()
	tooSoon := now-commentPollLastAttempt < commentPollMinIntervalSeconds
	if tid == "" || agent == nil || !canSteer || tooSoon {
		bridgeMu.Unlock()
		return false
	}
	commentPollLastAttempt = now
	seen, seeded := commentWatermark[tid]
	bridgeMu.Unlock()
	rows, err := commentsAfter(ctx, tid, seen)
	if err != nil {
		kanbanToolsLog.Debug("comment-inject: bridge failed", "exc_info", excText(err))
		return false
	}
	var newest int64
	for _, c := range rows {
		newest = max(newest, c.ID)
	}
	bridgeMu.Lock()
	if !seeded {
		commentWatermark[tid] = newest
	} else if len(rows) > 0 {
		commentWatermark[tid] = newest
	}
	bridgeMu.Unlock()
	if !seeded || len(rows) == 0 {
		return false
	}
	own := persistedIdentity(ctx)
	var lines []string
	for _, c := range rows {
		if pystr.Strip(c.Author) == own || pystr.Strip(c.Body) == "" {
			continue
		}
		author := c.Author
		if author == "" {
			author = "operator"
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", author, pystr.Strip(c.Body)))
	}
	if len(lines) == 0 {
		return false
	}
	plural := ""
	if len(lines) > 1 {
		plural = "s"
	}
	note := "New note" + plural + " on your kanban task from the operator (delivered mid-run). " +
		"Take it into account for the work you're doing right now:\n" + strings.Join(lines, "\n")
	defer func() {
		if r := recover(); r != nil {
			kanbanToolsLog.Debug("comment-inject: steer failed", "exc_info", fmt.Sprint(r))
			injected = false
		}
	}()
	return steerer.Steer(note)
}

// Comments of a task newer than a comment id, read on a board connection that is closed quietly
func commentsAfter(ctx context.Context, tid string, afterID int64) (rows []*Comment, err error) {
	defer func() {
		if r := recover(); r != nil {
			rows, err = nil, &RuntimeError{fmt.Sprint(r)}
		}
	}()
	var conn state.SQLiteConn
	if conn, err = Connect(ctx, "", nil); err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	return ListCommentsAfter(ctx, conn, tid, afterID)
}
