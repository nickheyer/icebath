package kanban

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Session id only when this profile's state database holds it, nil when absent or unopenable
func persistedSessionID(k *kanbanCall, sessionID any) (*string, error) {
	if !anyutil.IsTrue(sessionID) {
		return nil, nil
	}
	db, err := state.Open(home.StatePath(k.home()), state.Options{ReadOnly: true})
	if err != nil {
		pkgLog().Debug("Could not open state.db to verify Kanban provenance", "exc_info", excText(err))
		return nil, nil
	}
	defer func() { _ = db.Close() }()
	id, err := sqliteBind(sessionID, 1)
	if err != nil || id == nil {
		return nil, err
	}
	row, err := db.GetSession(k.ctx, *id)
	if err != nil {
		return nil, err
	}
	if row == nil || row.Len() == 0 {
		return nil, nil
	}
	return id, nil
}

// Optional text of a create argument Python strips and treats as absent when empty, an attribute error for other types
func strippedArg(value any) (*string, error) {
	if !anyutil.IsTrue(value) {
		return nil, nil
	}
	text, isText := value.(string)
	if !isText {
		return nil, &RuntimeError{fmt.Sprintf("expected text, got %s", errchain.Kind(value))}
	}
	return nonEmpty(strings.TrimSpace(text)), nil
}

// Creates a task, a child of the caller's own task inheriting its project when no workspace is named
func handleCreate(k *kanbanCall) (string, error) {
	if err := rejectDelegatedChildMutation(k, "kanban_create"); err != nil {
		return "", err
	}
	title, err := requireText(k, "title", "")
	if err != nil {
		return "", err
	}
	assignee := k.get("assignee")
	if !anyutil.IsTrue(assignee) {
		return "", reject("assignee is required — name the profile that should execute this " +
			"task (the dispatcher will only spawn tasks with an assignee)")
	}
	workspaceKind, workspacePath := k.get("workspace_kind"), k.get("workspace_path")
	projectID, hasProject := k.args["project"]
	if !hasProject {
		projectID = k.get("project_id")
	}
	triage, err := parseBoolArg(k, "triage")
	if err != nil {
		return "", err
	}
	skillItems, hasSkills, err := coerceStrList(k.get("skills"), "skills", "skill names", false)
	if err != nil {
		return "", err
	}
	goalMode, err := parseBoolArg(k, "goal_mode")
	if err != nil {
		return "", err
	}
	modelOverride, providerOverride := k.get("model"), k.get("provider")
	if !anyutil.IsTrue(modelOverride) && anyutil.IsTrue(providerOverride) {
		return "", reject("'provider' requires 'model' to be set as well")
	}
	rawParents := k.get("parents")
	if !anyutil.IsTrue(rawParents) {
		rawParents = []any{}
	}
	parentItems, _, err := coerceStrList(rawParents, "parents", "task ids", false)
	if err != nil {
		return "", err
	}
	board, _ := boardArg(k.get("board"))
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		selfTID := ""
		if isDispatcherOwnedWorker(k.ctx, k.env) {
			selfTID = os.Getenv("ICEBATH_KANBAN_TASK")
		}
		var selfTask *Task
		if selfTID != "" {
			if selfTask, err = GetTask(k.ctx, conn, selfTID); err != nil {
				return "", err
			}
		}
		sessionID, err := persistedSessionID(k, k.get("session_id"))
		if err != nil {
			return "", err
		}
		if !truthyStr(sessionID) && selfTask != nil && truthyStr(selfTask.SessionID) {
			sessionID = selfTask.SessionID
		}
		if !truthyStr(sessionID) {
			if sessionID, err = persistedSessionID(k, tools.CurrentOriginSessionID(k.env)); err != nil {
				return "", err
			}
		}
		if !truthyStr(sessionID) {
			if sessionID, err = persistedSessionID(k, tools.SessionEnv(k.env, "ICEBATH_SESSION_ID")); err != nil {
				return "", err
			}
		}
		if !truthyStr(sessionID) {
			sessionID = nil
		}
		var projectSourceTaskID *string
		if projectID == nil && workspaceKind == nil && workspacePath == nil && selfTask != nil && truthyStr(selfTask.ProjectID) {
			projectID, projectSourceTaskID = *selfTask.ProjectID, &selfTask.ID
		}
		priority, err := optIntArg(k.get("priority"))
		if err != nil {
			return "", err
		}
		maxRuntime, err := optIntArg(k.get("max_runtime_seconds"))
		if err != nil {
			return "", err
		}
		goalMaxTurns, err := optIntArg(k.get("goal_max_turns"))
		if err != nil {
			return "", err
		}
		o := CreateTaskOptions{Title: strings.TrimSpace(anyutil.String(title)), Triage: triage, GoalMode: goalMode, Board: board,
			ProjectSourceTaskID: projectSourceTaskID, SessionID: sessionID}
		assigneeText := anyutil.String(assignee)
		o.Assignee = &assigneeText
		createdBy := persistedIdentity(k.ctx)
		o.CreatedBy = &createdBy
		if selfTID != "" {
			o.CreatorTaskID = &selfTID
		}
		if raw := k.get("completion_contract"); raw != nil {
			contract, isText := raw.(string)
			if !isText {
				contract = "\x00" + anyutil.String(raw)
			}
			o.CompletionContract = &contract
		}
		if _, err := validateContract(o.CompletionContract); err != nil {
			return "", err
		}
		if o.ModelOverride, err = strippedArg(modelOverride); err != nil {
			return "", err
		}
		if o.ProviderOverride, err = strippedArg(providerOverride); err != nil {
			return "", err
		}
		var selectBinding, parentBinding, insertBinding error
		bind := func(slot **string, value any, position int, atSelect bool) {
			text, err := sqliteBind(value, position)
			*slot = text
			if err == nil {
				return
			}
			if atSelect && selectBinding == nil {
				selectBinding = err
			} else if !atSelect && insertBinding == nil {
				insertBinding = err
			}
		}
		bindInt := func(value *big.Int, position int) *int64 {
			if value == nil {
				return nil
			}
			if !value.IsInt64() {
				if insertBinding == nil {
					_, insertBinding = sqliteBind(value, position)
				}
				return nil
			}
			n := value.Int64()
			return &n
		}
		bind(&o.Body, k.get("body"), 3, false)
		if n := bindInt(priority, 6); n != nil {
			o.Priority = *n
		}
		bind(&o.WorkspacePath, workspacePath, 10, false)
		if _, isText := workspacePath.(string); !isText && o.WorkspacePath != nil {
			o.workspacePathRaw = workspacePath
		}
		tenant := k.get("tenant")
		if !anyutil.IsTrue(tenant) {
			tenant = nil
			if inherited, set := os.LookupEnv("ICEBATH_TENANT"); set {
				tenant = inherited
			}
		}
		bind(&o.Tenant, tenant, 13, false)
		if _, isText := tenant.(string); !isText && o.Tenant != nil {
			o.tenantRaw = tenant
		}
		idempotencyKey := k.get("idempotency_key")
		bind(&o.IdempotencyKey, idempotencyKey, map[bool]int{true: 1, false: 14}[anyutil.IsTrue(idempotencyKey)], anyutil.IsTrue(idempotencyKey))
		o.idempotencyKeyFalsy = idempotencyKey != nil && !anyutil.IsTrue(idempotencyKey)
		o.MaxRuntimeSeconds = bindInt(maxRuntime, 15)
		o.GoalMaxTurns = bindInt(goalMaxTurns, 22)
		if workspaceKind != nil {
			kind := anyutil.String(workspaceKind)
			o.WorkspaceKind = &kind
		}
		if projectID != nil {
			project := anyutil.String(projectID)
			o.ProjectID = &project
		}
		if hasSkills {
			o.Skills = []string{}
			for _, item := range skillItems {
				if anyutil.IsTrue(item) {
					o.Skills = append(o.Skills, anyutil.String(item))
				}
			}
		}
		for _, item := range parentItems {
			if !anyutil.IsTrue(item) {
				continue
			}
			o.Parents = append(o.Parents, anyutil.String(item))
			if _, err := sqliteBind(item, len(o.Parents)); err != nil && parentBinding == nil {
				parentBinding = err
			}
		}
		o.InitialStatus = "running"
		if raw := k.get("initial_status"); anyutil.IsTrue(raw) {
			o.InitialStatus = anyutil.String(raw)
		}
		if selectBinding != nil || parentBinding != nil || insertBinding != nil {
			probe := o
			if selectBinding != nil || parentBinding != nil {
				probe.Parents = nil
			}
			probed, err := createRolledBack(k.ctx, conn, probe)
			if err != nil {
				return "", createArgumentError(err, workspaceKind, parentItems)
			}
			if selectBinding != nil {
				return "", selectBinding
			}
			existing, err := GetTask(k.ctx, conn, probed)
			if err != nil {
				return "", err
			}
			if existing == nil && parentBinding != nil {
				return "", parentBinding
			}
			if existing == nil {
				return "", insertBinding
			}
		}
		newTID, err := CreateTask(k.ctx, conn, o)
		if err != nil {
			return "", createArgumentError(err, workspaceKind, parentItems)
		}
		created, err := GetTask(k.ctx, conn, newTID)
		if err != nil {
			return "", err
		}
		events, err := ListEvents(k.ctx, conn, newTID)
		if err != nil {
			return "", err
		}
		fields := []any{"task_id", newTID}
		landed := fieldsOf(created, createdFields)
		for _, name := range landed.Keys() {
			fields = append(fields, name, landed.Value(name))
		}
		var wait *Event
		for _, e := range events {
			if e.Kind == "dependency_wait" {
				wait = e
			}
		}
		if wait == nil {
			fields = append(fields, "gated", false)
		} else {
			items, _ := dictObject(wait.Payload)
			var parent any
			present := false
			if items != nil {
				parent, present = items.Get("parent")
			}
			if !present {
				return "", &RuntimeError{"'parent'"}
			}
			fields = append(fields, "gated", true, "gated_by", parent)
		}
		return okPayload(append(fields, "subscribed", maybeAutoSubscribe(k, conn, newTID))...)
	})
}

var errCreateProbe = errors.New("kanban create probe")

// Runs a creation and rolls it back, returning the id reached and any failure raised before the insert
func createRolledBack(ctx context.Context, conn state.SQLiteConn, o CreateTaskOptions) (id string, err error) {
	err = WriteTxn(ctx, conn, true, func() error {
		var cerr error
		if id, cerr = CreateTask(ctx, conn, o); cerr != nil {
			return cerr
		}
		return errCreateProbe
	})
	if errors.Is(err, errCreateProbe) {
		return id, nil
	}
	return "", err
}

// The failure Python raises for a workspace kind or parent id that was not a string, the error itself otherwise
func createArgumentError(err error, workspaceKind any, parentItems []any) error {
	var ve *ValueError
	if !errors.As(err, &ve) {
		return err
	}
	if _, isText := workspaceKind.(string); workspaceKind != nil && !isText {
		passed := ", got " + anyutil.String(workspaceKind)
		if strings.HasPrefix(ve.Msg, "workspace_kind must be one of ") && strings.HasSuffix(ve.Msg, passed) {
			if !pyHashable(workspaceKind) {
				return unhashableSetError(workspaceKind)
			}
			return &ValueError{strings.TrimSuffix(ve.Msg, passed) + ", got " + errchain.Show(workspaceKind)}
		}
	}
	const unknownParents = "unknown parent task(s): "
	if missing, isMissing := strings.CutPrefix(ve.Msg, unknownParents); isMissing {
		position := 0
		named := map[string]bool{}
		for _, id := range strings.Split(missing, ", ") {
			named[id] = true
		}
		for _, item := range parentItems {
			if !anyutil.IsTrue(item) || !named[anyutil.String(item)] {
				continue
			}
			if _, isText := item.(string); !isText {
				return &RuntimeError{fmt.Sprintf("task id at position %d is %s where text was expected", position, errchain.Kind(item))}
			}
			position++
		}
	}
	return err
}

// Profile and session store functions the create and review paths call, swapped by tests like the Python module attributes
var (
	profileExistsFn = profiles.ProfileExists
	profileDirFn    = profiles.GetProfileDir
	acquireStateFn  = state.Acquire
	addNotifySubFn  = AddNotifySub
)

// State database of a named profile, empty for the default store or when the profile cannot be resolved
func profileStatePath(profile string) (path string) {
	defer func() {
		if recover() != nil {
			path = ""
		}
	}()
	if profile == "" || profile == "default" || !profileExistsFn(profile) {
		return ""
	}
	dir, err := profileDirFn(profile)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "state.db")
}

// Session key of a TUI subscription mapped to the live tip of its compaction lineage, unchanged on any failure
func liveTUISessionKey(ctx context.Context, sessionKey, profile string) (key string) {
	defer func() {
		if recover() != nil {
			key = sessionKey
		}
	}()
	dbPath := profileStatePath(profile)
	db, err := acquireStateFn(ctx, dbPath)
	if err != nil {
		return sessionKey
	}
	defer state.ReleaseOrClose(db)
	resolved, err := db.ResolveResumeSessionID(sessionKey)
	if err != nil || resolved == "" {
		return sessionKey
	}
	return resolved
}

// Subscription target of the calling session, nil for a CLI, cron or test call with no persistent channel
func resolveNotifyTarget(k *kanbanCall) *NotifySubOptions {
	env := func(name string) string { return tools.SessionEnv(k.env, name) }
	platform, chatID := env("ICEBATH_SESSION_PLATFORM"), env("ICEBATH_SESSION_CHAT_ID")
	if platform == "" || chatID == "" {
		sessionKey := env("ICEBATH_SESSION_KEY")
		if sessionKey == "" {
			sessionKey = os.Getenv("ICEBATH_SESSION_KEY")
		}
		if sessionKey == "" {
			return nil
		}
		platform, chatID = "tui", sessionKey
	}
	chatType, threadID := nonEmpty(env("ICEBATH_SESSION_CHAT_TYPE")), nonEmpty(env("ICEBATH_SESSION_THREAD_ID"))
	messageID := env("ICEBATH_SESSION_MESSAGE_ID")
	notifierProfile := env("ICEBATH_SESSION_PROFILE")
	if notifierProfile == "" {
		notifierProfile = profiles.CurrentProfileName(k.ctx)
		if notifierProfile == "" {
			notifierProfile = "default"
		}
	}
	if platform == "tui" {
		chatID = liveTUISessionKey(k.ctx, chatID, notifierProfile)
	}
	metadata := jsonutil.NewObject()
	for _, item := range [][2]any{{"thread_id", optStrValue(threadID)}, {"chat_type", optStrValue(chatType)},
		{"scope_id", env("ICEBATH_SESSION_SCOPE_ID")}, {"parent_chat_id", env("ICEBATH_SESSION_PARENT_CHAT_ID")}} {
		if anyutil.IsTrue(item[1]) {
			metadata.Set(item[0].(string), item[1])
		}
	}
	chat := ""
	if chatType != nil {
		chat = strings.ToLower(*chatType)
	}
	if strings.ToLower(platform) == "telegram" && threadID != nil && (chat == "dm" || chat == "direct" || chat == "private") {
		metadata.Set("telegram_dm_topic_reply_fallback", true)
		if *threadID != "1" {
			metadata.Set("direct_messages_topic_id", *threadID)
		}
		if messageID != "" {
			metadata.Set("telegram_reply_to_message_id", messageID)
		}
	}
	target := &NotifySubOptions{Platform: platform, ChatID: chatID, ChatType: chatType, ThreadID: threadID,
		UserID: nonEmpty(env("ICEBATH_SESSION_USER_ID")), UserIDAlt: nonEmpty(env("ICEBATH_SESSION_USER_ID_ALT")),
		NotifierProfile: &notifierProfile}
	if platform != "tui" {
		mode := "notify+wake"
		target.DeliveryMode = &mode
	}
	if metadata.Len() > 0 {
		target.DeliveryMetadata = metadata
	}
	return target
}

// Subscribes the calling session to the terminal events of a new task, true when a subscription is in place
func maybeAutoSubscribe(k *kanbanCall, conn state.SQLiteConn, taskID string) (subscribed bool) {
	if !settings.KanbanAutoSubscribeOnCreate.Get(home.WithOverride(k.ctx, k.home())) {
		return false
	}
	var target *NotifySubOptions
	failed := func(cause error) bool {
		platform, keySet := "", false
		if target != nil {
			platform, keySet = target.Platform, target.ChatID != ""
		}
		pkgLog().Warn(fmt.Sprintf("_maybe_auto_subscribe failed: %s (platform=%s key_set=%s)",
			excText(cause), errchain.Show(platform), errchain.Show(keySet)))
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			err, isErr := r.(error)
			if !isErr {
				err = &RuntimeError{fmt.Sprint(r)}
			}
			subscribed = failed(err)
		}
	}()
	if target = resolveNotifyTarget(k); target == nil {
		return false
	}
	subs, err := ListNotifySubs(k.ctx, conn, &taskID, NotifyOwnerScope{})
	if err != nil {
		return failed(err)
	}
	wantThread := ""
	if target.ThreadID != nil {
		wantThread = *target.ThreadID
	}
	for _, sub := range subs {
		platform, _ := sub.Get("platform")
		chatID, _ := sub.Get("chat_id")
		thread, _ := sub.Get("thread_id")
		if !anyutil.IsTrue(thread) {
			thread = ""
		}
		if anyutil.Equal(platform, target.Platform) && anyutil.Equal(chatID, target.ChatID) && anyutil.Equal(thread, wantThread) {
			return true
		}
	}
	target.TaskID = taskID
	if err := addNotifySubFn(k.ctx, conn, *target); err != nil {
		return failed(err)
	}
	return true
}

// Moves a blocked task to ready, or back to waiting while a parent is still open
func handleUnblock(k *kanbanCall) (string, error) {
	if err := rejectDelegatedChildMutation(k, "kanban_unblock"); err != nil {
		return "", err
	}
	if err := requireOrchestratorTool("kanban_unblock"); err != nil {
		return "", err
	}
	raw := k.get("task_id")
	if !anyutil.IsTrue(raw) {
		return "", reject("task_id is required")
	}
	tid := anyutil.String(raw)
	if err := enforceWorkerTaskOwnership(tid); err != nil {
		return "", err
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		ok, err := UnblockTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", reject(fmt.Sprintf("could not unblock %s (not blocked or unknown)", tid))
		}
		task, err := GetTask(k.ctx, conn, tid)
		if err != nil {
			return "", err
		}
		return okPayload("task_id", tid, "status", rowField(task, "status"))
	})
}

// Adds a parent to child dependency edge between two existing tasks
func handleLink(k *kanbanCall) (string, error) {
	if err := rejectDelegatedChildMutation(k, "kanban_link"); err != nil {
		return "", err
	}
	parentID, childID := k.get("parent_id"), k.get("child_id")
	if !anyutil.IsTrue(parentID) || !anyutil.IsTrue(childID) {
		return "", reject("both parent_id and child_id are required")
	}
	return withBoard(k.ctx, k.get("board"), false, func(conn state.SQLiteConn) (string, error) {
		expected := expectedRunID(anyutil.String(childID))
		if anyutil.Equal(parentID, childID) {
			return "", &ValueError{"a task cannot depend on itself"}
		}
		var missing []any
		for i, id := range []any{parentID, childID} {
			text, err := sqliteBind(id, i+1)
			if err != nil {
				return "", err
			}
			if _, isText := id.(string); !isText {
				missing = append(missing, id)
				continue
			}
			task, err := GetTask(k.ctx, conn, *text)
			if err != nil {
				return "", err
			}
			if task == nil {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			names := make([]string, 0, len(missing))
			for i, id := range missing {
				name, isText := id.(string)
				if !isText {
					return "", &RuntimeError{fmt.Sprintf("task id at position %d is %s where text was expected", i, errchain.Kind(id))}
				}
				names = append(names, name)
			}
			return "", &ValueError{"unknown task(s): " + strings.Join(names, ", ")}
		}
		gated, err := LinkTasks(k.ctx, conn, parentID.(string), childID.(string), expected)
		if err != nil {
			return "", err
		}
		fields := []any{"parent_id", parentID, "child_id", childID, "gated", gated}
		if gated {
			fields = append(fields, "gated_by", parentID)
		}
		return okPayload(fields...)
	})
}
