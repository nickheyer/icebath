package gateway

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Whether the request names exactly the claim fields, the db path optional
func kanbanClaimKeys(params *pyjson.Object) bool {
	keys := slices.Clone(params.Keys)
	sort.Strings(keys)
	return slices.Equal(keys, []string{"board", "claim_lock", "run_id", "task_id"}) ||
		slices.Equal(keys, []string{"board", "claim_lock", "db", "run_id", "task_id"})
}

// Durable request identity of one dispatcher attempt
func kanbanRequestID(board, taskID, runID any) (string, error) {
	text, err := pyjson.Dumps([]any{board, taskID, runID}, pyjson.Separators(",", ":"))
	if err != nil {
		return "", err
	}
	return "kanban:" + text, nil
}

// Refuses an unreadable claim database or board as an invalid claim, refusals and lookups passed through
func invalidKanbanClaim(err error) error {
	var refusal *state.RuntimeStoreError
	var missing *pyKeyError
	if errors.As(err, &refusal) || errors.As(err, &missing) {
		return err
	}
	return runtimeRefusal("invalid_kanban_claim")
}

// Read only connection URI of a resolved sqlite path
func readOnlySQLiteURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro"
}

// Facts the claim check reads off the task, its run and the board
type kanbanClaimFacts struct {
	board, path, context string
	task                 *kanban.Task
	skills               []string
}

// Checks the dispatcher claim against the board read only and collects what the worker policy needs
func readKanbanClaim(ctx context.Context, a *SessionAuthority, params *pyjson.Object, runID int64) (*kanbanClaimFacts, error) {
	claim := &kanbanClaimFacts{}
	board, err := kanban.RequireSlug(params.GetOr("board", "").(string))
	if err != nil {
		return nil, err
	}
	claim.board = board
	if raw, ok := params.Get("db"); ok {
		absolute, err := filepath.Abs(raw.(string))
		if err != nil {
			return nil, err
		}
		if claim.path, err = pypath.Realpath(absolute, pypath.StrictAll); err != nil {
			return nil, err
		}
	} else {
		dbPath, err := kanban.KanbanDBPath(ctx, &board)
		if err != nil {
			return nil, err
		}
		if claim.path, err = pypath.Resolve(dbPath); err != nil {
			return nil, err
		}
	}
	conn, err := sql.Open("sqlite", readOnlySQLiteURI(claim.path))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	taskID := params.GetOr("task_id", "").(string)
	claimLock := params.GetOr("claim_lock", "").(string)
	task, err := kanban.GetTask(ctx, conn, taskID)
	if err != nil {
		return nil, err
	}
	var runStatus, runTask, runLock sql.NullString
	runFound := true
	if err := conn.QueryRowContext(ctx, "SELECT status, task_id, claim_lock FROM task_runs WHERE id=?", runID).Scan(&runStatus, &runTask, &runLock); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		runFound = false
	}
	if task == nil || !runFound || task.Status != "running" || runStatus.String != "running" ||
		task.CurrentRunID == nil || *task.CurrentRunID != runID || runTask.String != task.ID ||
		task.ClaimLock == nil || *task.ClaimLock != claimLock || !runLock.Valid || runLock.String != *task.ClaimLock {
		return nil, runtimeRefusal("stale_kanban_claim")
	}
	assignee := ""
	if task.Assignee != nil {
		assignee = *task.Assignee
	}
	env, err := profiles.ResolveProfileEnv(assignee)
	if err != nil {
		return nil, err
	}
	assigneeHome, err := pypath.Resolve(env)
	if err != nil {
		return nil, err
	}
	ownerHome, err := pypath.Resolve(a.ProfileID)
	if err != nil {
		return nil, err
	}
	if assigneeHome != ownerHome {
		return nil, runtimeRefusal("profile_mismatch")
	}
	var bound string
	err = conn.QueryRowContext(ctx, "SELECT payload FROM task_events WHERE task_id=? AND run_id=? AND kind='owner_admitted' ORDER BY id DESC LIMIT 1",
		task.ID, *task.CurrentRunID).Scan(&bound)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		marker, err := pyjson.LoadsDict(bound)
		if err != nil {
			return nil, err
		}
		requestID, ok := marker.Get("request_id")
		if !ok {
			return nil, &pyKeyError{"request_id"}
		}
		expected, err := kanbanRequestID(board, task.ID, *task.CurrentRunID)
		if err != nil {
			return nil, err
		}
		if !pyrepr.Equal(requestID, expected) {
			return nil, runtimeRefusal("stale_kanban_claim")
		}
	}
	if claim.context, err = kanban.BuildWorkerContext(ctx, conn, task.ID); err != nil {
		return nil, err
	}
	claim.skills = slices.Clone(task.Skills)
	status, err := kanban.RetryStatusForRun(ctx, conn, task.ID, task.CurrentRunID)
	if err != nil {
		return nil, err
	}
	if status == "review" && !slices.Contains(claim.skills, "sdlc-review") {
		claim.skills = append(claim.skills, "sdlc-review")
	}
	claim.task = task
	return claim, nil
}

// Trusted worker policy of a current dispatcher claim, the card's model, provider and reasoning overrides applied
func buildKanbanPolicy(ctx context.Context, c *AuthorityConnection, params *pyjson.Object, cfg map[string]any) (*LocalSessionPolicy, *RecoveredSecrets, error) {
	a, actor := c.authority, c.actor
	if !c.nativeOwner || !actor.Capabilities["session:create"] {
		return nil, nil, runtimeRefusal("permission_denied")
	}
	if actor.ProfileID != a.ProfileID {
		return nil, nil, runtimeRefusal("profile_mismatch")
	}
	if !kanbanClaimKeys(params) {
		return nil, nil, runtimeRefusal("invalid_params")
	}
	for _, key := range []string{"board", "task_id", "claim_lock"} {
		if text, isStr := params.GetOr(key, nil).(string); !isStr || text == "" {
			return nil, nil, runtimeRefusal("invalid_params")
		}
	}
	var runID int64
	switch value := params.GetOr("run_id", nil).(type) {
	case int64:
		runID = value
	case *big.Int:
		return nil, nil, fmt.Errorf("OverflowError: Python int too large to convert to SQLite INTEGER")
	default:
		return nil, nil, runtimeRefusal("invalid_params")
	}
	claim, err := readKanbanClaim(ctx, a, params, runID)
	if err != nil {
		return nil, nil, invalidKanbanClaim(err)
	}
	task := claim.task
	var cwd any
	if task.WorkspacePath != nil {
		cwd = *task.WorkspacePath
	}
	launch := map[string]any{"source": "cli", "cwd": cwd}
	for name, value := range map[string]*string{"model": task.ModelOverride, "provider": task.ProviderOverride, "reasoning": task.ReasoningEffort} {
		if value != nil && *value != "" {
			launch[name] = *value
		}
	}
	private := &RecoveredSecrets{}
	policy, err := buildPolicy(ctx, launch, cfg, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		return nil, nil, err
	}
	if policy.Model == nil {
		frozen, err := policy.config(ctx, nil, true)
		if err != nil {
			return nil, nil, err
		}
		model := resolveGatewayModel(ctx, frozen)
		replaced := *policy
		replaced.Model = &model
		policy = &replaced
	}
	root, err := kanban.WorkspacesRoot(ctx, &claim.board)
	if err != nil {
		return nil, nil, err
	}
	if root, err = pypath.Resolve(root); err != nil {
		return nil, nil, err
	}
	var goalParts []string
	for _, part := range []*string{&task.Title, task.Body} {
		if part != nil && *part != "" {
			goalParts = append(goalParts, *part)
		}
	}
	skills := make([]any, len(claim.skills))
	for i, skill := range claim.skills {
		skills[i] = skill
	}
	worker := params.Copy()
	worker.Set("workspace", cwd)
	worker.Set("branch", optionalText(task.BranchName))
	worker.Set("tenant", optionalText(task.Tenant))
	worker.Set("profile", optionalText(task.Assignee))
	worker.Set("workspaces_root", root)
	worker.Set("skills", skills)
	worker.Set("goal_mode", task.GoalMode)
	worker.Set("goal_max_turns", optionalInt(task.GoalMaxTurns))
	worker.Set("goal_text", strings.Join(goalParts, "\n\n"))
	worker.Set("context", claim.context)
	worker.Set("max_runtime_seconds", optionalInt(task.MaxRuntimeSeconds))
	worker.Set("accept_hooks", true)
	worker.Set("db", claim.path)
	request, err := pyjson.LoadsDict(policy.RequestJSON)
	if err != nil {
		return nil, nil, err
	}
	request.Set("source", "kanban")
	request.Set("board", claim.board)
	request.Set("task_id", task.ID)
	request.Set("run_id", *task.CurrentRunID)
	requestJSON, err := pyjson.Dumps(request, pyjson.SortKeys(true))
	if err != nil {
		return nil, nil, err
	}
	kanbanJSON, err := pyjson.Dumps(worker)
	if err != nil {
		return nil, nil, err
	}
	toolsets := slices.Clone(policy.Toolsets)
	if !slices.Contains(toolsets, "kanban") {
		toolsets = append(toolsets, "kanban")
	}
	sort.Strings(toolsets)
	toolsets = slices.Compact(toolsets)
	replaced := *policy
	replaced.Source, replaced.Platform, replaced.Toolsets = "kanban", "cli", toolsets
	replaced.RequestJSON, replaced.KanbanJSON = requestJSON, &kanbanJSON
	return &replaced, private, nil
}

// JSON value of an optional text field
func optionalText(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// JSON value of an optional integer field
func optionalInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// Kanban run verb, the dispatcher's attempt admitted once as a finite card session turn
func runKanbanTask(ctx context.Context, c *AuthorityConnection, params *pyjson.Object) (any, error) {
	a, actor := c.authority, c.actor
	if !c.nativeOwner || !actor.Capabilities["session:create"] {
		return nil, runtimeRefusal("permission_denied")
	}
	ownerDB, err := pypath.Resolve(a.DB.Path())
	if err != nil {
		return nil, err
	}
	requestID, err := kanbanRequestID(params.GetOr("board", nil), params.GetOr("task_id", nil), params.GetOr("run_id", nil))
	if err != nil {
		return nil, err
	}
	sid, err := state.LocalIdentity(a.ProfileID, actor.Subject, requestID)
	if err != nil {
		return nil, err
	}
	var saved *pyjson.Object
	if row, err := a.DB.GetSession(ctx, sid); err != nil {
		return nil, err
	} else if row != nil {
		if saved, err = a.DB.LocalReceipt(ctx, sid); err != nil {
			return nil, err
		}
	}
	var policy *LocalSessionPolicy
	var ref SessionRef
	if saved != nil && saved.Len() > 0 {
		if policy, err = restorePolicy(saved.GetOr("policy", nil)); err != nil {
			return nil, err
		}
		stored := "{}"
		if policy.KanbanJSON != nil && *policy.KanbanJSON != "" {
			stored = *policy.KanbanJSON
		}
		savedContext, err := pyjson.LoadsDict(stored)
		if err != nil {
			return nil, err
		}
		if !c.nativeOwner || !kanbanClaimKeys(params) {
			return nil, runtimeRefusal("permission_denied")
		}
		for _, key := range params.Keys {
			if !pyrepr.Equal(params.GetOr(key, nil), savedContext.GetOr(key, nil)) {
				return nil, runtimeRefusal("permission_denied")
			}
		}
		if ref, err = restoreLocalSession(ctx, a, sid); err != nil {
			return nil, err
		}
	} else {
		built, private, err := buildKanbanPolicy(ctx, c, params, loadGatewayConfig(ctx, ""))
		if err != nil {
			return nil, err
		}
		policy = built
		if ref, err = createLocalSession(ctx, a, actor, map[string]any{"request_id": requestID},
			localCreateOptions{TrustedPolicy: policy, TrustedSecrets: private}); err != nil {
			return nil, err
		}
	}
	claimContext, err := pyjson.LoadsDict(*policy.KanbanJSON)
	if err != nil {
		return nil, err
	}
	taskID, runID, claimLock := params.GetOr("task_id", nil), params.GetOr("run_id", nil), params.GetOr("claim_lock", nil)
	admitClaim := func(ctx context.Context) (any, error) {
		return nil, kanban.ConnectClosing(ctx, pyrepr.Str(claimContext.GetOr("db", nil)), nil, func(conn state.SQLiteConn) error {
			return kanban.WriteTxn(ctx, conn, false, func() error {
				var marker string
				err := conn.QueryRowContext(ctx, "SELECT payload FROM task_events WHERE task_id=? AND run_id=? AND kind='owner_admitted' ORDER BY id DESC LIMIT 1",
					taskID, runID).Scan(&marker)
				if err == nil {
					payload, err := pyjson.LoadsDict(marker)
					if err != nil {
						return err
					}
					sessionID, ok := payload.Get("session_id")
					if !ok {
						return &pyKeyError{"session_id"}
					}
					if !pyrepr.Equal(sessionID, ref.SessionID) {
						return runtimeRefusal("stale_kanban_claim")
					}
					return nil
				}
				if !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				task, err := kanban.GetTask(ctx, conn, pyrepr.Str(taskID))
				if err != nil {
					return err
				}
				if task == nil || task.Status != "running" || task.CurrentRunID == nil || !pyrepr.Equal(*task.CurrentRunID, runID) ||
					task.ClaimLock == nil || !pyrepr.Equal(*task.ClaimLock, claimLock) {
					return runtimeRefusal("stale_kanban_claim")
				}
				pid := os.Getpid()
				birth, err := proc.WorkerBirth(pid)
				if err != nil {
					return err
				}
				return kanban.AppendEvent(ctx, conn, task.ID, "owner_admitted", pyjson.ObjectOf("db", ownerDB,
					"session_id", ref.SessionID, "request_id", requestID, "pid", int64(pid), "birth", birth), task.CurrentRunID)
			})
		})
	}
	if _, err := trackedWrite(ctx, a, admitClaim, nil, false, nil); err != nil {
		return nil, err
	}
	if _, err := c.resume(ctx, ref, pyjson.NewObject()); err != nil {
		return nil, err
	}
	var committed *pyjson.Object
	if saved != nil && saved.Len() > 0 {
		if committed, err = a.DB.FindSessionAdmission(ctx, actor.Subject, ref.SessionID, requestID); err != nil {
			return nil, err
		}
	}
	committedHas := func(key string) bool {
		payload, _ := committed.GetOr("payload", nil).(*pyjson.Object)
		if payload == nil {
			return false
		}
		_, ok := payload.Get(key)
		return ok
	}
	payload := map[string]any{"text": fmt.Sprintf("work kanban task %s", pyrepr.Str(taskID))}
	if committed == nil || committedHas("finite") {
		payload["finite"] = true
	}
	var card any
	if committed == nil || committedHas("kanban_card_v1") {
		card = pyjson.ObjectOf("board", claimContext.GetOr("board", nil), "task_id", taskID, "run_id", runID)
	}
	receipt, err := a.submit(ctx, actor, Submission{RequestID: requestID, Ref: ref, Payload: payload, Intent: "queue"}, SubmitOptions{KanbanCard: card})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("session_id", ref.SessionID, "receipt", admissionReceiptDict(receipt)), nil
}

// Kanban run verb
func (c *AuthorityConnection) kanbanRun(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
	return runKanbanTask(ctx, c, params)
}
