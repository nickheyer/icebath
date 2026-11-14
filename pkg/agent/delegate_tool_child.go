package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"weak"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dedicated session db handle on the parent's db file, nil when the parent has none or it cannot open
func openChildSessionDB(ctx context.Context, parent *AIAgent) (SessionStore, error) {
	workerStore, isWorkerStore := parent.sessionDB.(*RuntimeSessionStore)
	if IsWorkerProcess() || isWorkerStore {
		if !isWorkerStore {
			return nil, tools.NewWorkerPersistenceError("worker_child_registration_required")
		}
		return ReserveChildStore(ctx, workerStore)
	}
	if parent.sessionDB == nil {
		return nil, nil
	}
	db, err := state.Acquire(ctx, sessionStorePath(parent.sessionDB))
	if err != nil {
		delegateLog().Debug("subagent: failed to open dedicated SessionDB; child persistence disabled", "error", err)
		return nil, nil
	}
	return db, nil
}

func childStrArg(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

func strListArg(v any) []string {
	switch l := v.(type) {
	case []string:
		return append([]string{}, l...)
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			out = append(out, pyrepr.Str(x))
		}
		return out
	}
	return nil
}

func intArg(v any, def int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return def
}

// Agent constructor of a child, swapped by tests that read the resolved construction options
var newChildAIAgentFn = NewAIAgent

// Builds without running a child agent for one task, its route overrides replacing parent inheritance
func buildChildAgent(ctx context.Context, spec childBuildSpec) (*AIAgent, error) {
	parent := spec.Parent
	goal := childStrArg(spec.Goal)
	childDepth := parent.delegateDepth + 1
	maxSpawn := tools.MaxSpawnDepth(ctx)
	effectiveRole := "leaf"
	if tools.OrchestratorEnabled(ctx) && childDepth < maxSpawn {
		effectiveRole = "orchestrator"
	}
	subagentID := fmt.Sprintf("sa-%d-%s", spec.TaskIndex, strings.ReplaceAll(uuid.NewString(), "-", "")[:8])
	parentSubagentID := parent.subagentID
	delegationCfg := tools.LoadDelegationConfig(ctx)
	childToolsets, childDisabled := resolveChildToolsets(ctx, parent, spec.Toolsets, effectiveRole)
	childPrompt := buildChildSystemPrompt(ctx, parent.env, childStrArg(spec.Context), resolveWorkspaceHint(parent), effectiveRole, maxSpawn, childDepth)
	parentAPIKey := parent.apiKey
	if parentAPIKey == "" && parent.clientKwargs != nil {
		parentAPIKey, _ = parent.clientKwargs["api_key"].(string)
	}
	sessionRef, sessionRefMu := map[string]any{}, &sync.Mutex{}
	model := childStrArg(spec.Model)
	relayModel := model
	if relayModel == "" {
		relayModel = parent.model
	}
	depth := max(0, childDepth-1)
	progressCB := buildChildProgressCallback(spec.TaskIndex, goal, parent, spec.TaskCount, subagentID, parentSubagentID,
		&depth, relayModel, childToolsets, sessionRef, sessionRefMu)
	buildCtx := delegatedProfileScope(ctx, spec.ProfileHome)
	var childEnv *tools.Env
	if spec.ProfileHome != "" {
		childEnv = tools.EnvForProfileHome(buildCtx, parent.env, spec.ProfileHome)
	}
	var apiMode *string
	if spec.OverrideAPIMode != nil {
		m := pyrepr.Str(spec.OverrideAPIMode)
		apiMode = &m
	}
	overrideProvider := childStrArg(spec.OverrideProvider)
	o, err := resolveChildRuntime(buildCtx, parent, delegationCfg, parentAPIKey, childRuntimeSpec{
		model: model, overrideProvider: overrideProvider, overrideBaseURL: childStrArg(spec.OverrideBaseURL),
		overrideAPIKey: childStrArg(spec.OverrideAPIKey), overrideAPIMode: apiMode, overrideACPCommand: childStrArg(spec.OverrideACPCommand),
		overrideACPArgs: strListArg(spec.OverrideACPArgs), routingCfg: spec.RoutingCfg, overrideReasoningEffort: spec.OverrideReasoningEffort,
	})
	if err != nil {
		return nil, err
	}
	if tier := childStrArg(spec.OverrideServiceTier); tier != "" {
		o.ServiceTier = tier
	}
	requestOverrides := map[string]any{}
	if ro, ok := spec.OverrideRequestOverrides.(map[string]any); ok && spec.OverrideRequestOverrides != nil {
		for k, v := range ro {
			requestOverrides[k] = v
		}
	} else if overrideProvider == "" {
		for k, v := range parent.requestOverrides {
			requestOverrides[k] = v
		}
	}
	parentSID := parent.sessionID
	childDB, err := openChildSessionDB(buildCtx, parent)
	if err != nil {
		return nil, err
	}
	o.MaxIterations = new(intArg(spec.MaxIterations, delegateDefaultMaxIterations))
	o.PrefillMessages = parent.prefillMessages
	o.EnabledToolsets, o.DisabledToolsets = childToolsets, childDisabled
	o.QuietMode, o.EphemeralSystemPrompt = true, childPrompt
	o.LogPrefix, o.Platform, o.SideAgent = fmt.Sprintf("[subagent-%d]", spec.TaskIndex), "subagent", true
	o.SkipContextFiles, o.SkipMemory = true, true
	if progressCB != nil {
		o.ThinkingCallback = func(text string) {
			if text != "" {
				safeProgress(progressCB, "_thinking", text, "", nil)
			}
		}
	}
	o.SessionDB, o.ParentSessionID, o.RequestOverrides = childDB, parentSID, requestOverrides
	if childEnv != nil {
		o.Env = childEnv
	}
	o.ToolProgressCallback = progressCB
	o.LoadSoulIdentity = spec.ProfileHome != ""
	child, err := func() (child *AIAgent, err error) {
		defer func() {
			if r := recover(); r != nil {
				if childDB != nil {
					delegateQuiet("", func() { releaseOrCloseStore(childDB) })
				}
				panic(r)
			}
		}()
		childCtx := tools.WithDelegatedChildContext(buildCtx, true)
		sessionEnv := o.Env
		if sessionEnv == nil {
			sessionEnv = tools.ToolEnvFrom(childCtx)
		}
		defer tools.ScopedCurrentSessionID(sessionEnv, nil)()
		o.Env = tools.ForkSessionContext(sessionEnv)
		child, err = newChildAIAgentFn(childCtx, o)
		if err != nil && childDB != nil {
			delegateQuiet("", func() { releaseOrCloseStore(childDB) })
		}
		return child, err
	}()
	if err != nil {
		return nil, err
	}
	poolParent := parent
	if spec.ProfileHome != "" {
		poolParent = nil
	}
	childPool := resolveChildCredentialPool(buildCtx, o.Provider, poolParent, o.BaseURL, o.RequestedProvider)
	child.printFn = parent.printFn
	applyChildCacheTTL(child)
	if childDB != nil {
		child.ownsSessionDB = true
	}
	if err := reserveChildTranscript(child); err != nil {
		return nil, err
	}
	sessionRefMu.Lock()
	sessionRef["session_id"] = child.sessionID
	sessionRefMu.Unlock()
	child.progressIdentityRef = sessionRef
	child.delegateDepth, child.delegateRole = childDepth, effectiveRole
	child.subagentID, child.parentSubagentID = subagentID, parentSubagentID
	if err := applyChildCompressionCap(child, delegationCfg); err != nil {
		return nil, err
	}
	child.delegateParentRef = weak.Make(parent)
	if parentSID != "" && child.sessionInitModelConfig != nil {
		child.sessionInitModelConfig["_delegate_from"] = parentSID
	}
	child.delegateProfileHome = spec.ProfileHome
	if childPool != nil {
		child.credentialPool = childPool
	}
	attachChild(parent, child)
	safeProgress(progressCB, "subagent.spawn_requested", "", goal, nil)
	delegateQuiet("subagent_start hook invocation failed", func() {
		plugins.InvokeHook(ctx, "subagent_start", map[string]any{
			"parent_session_id": strOrNone(parentSID), "parent_turn_id": parent.currentTurnID,
			"parent_subagent_id": strOrNone(parentSubagentID), "child_session_id": strOrNone(child.sessionID),
			"child_subagent_id": subagentID, "child_role": effectiveRole, "child_goal": goal,
		})
	})
	return child, nil
}

// Runs a prebuilt child on the calling goroutine and returns its result entry
func runSingleChild(ctx context.Context, taskIndex int, goal string, child, parent *AIAgent, ownerSessionID string, ownerTransport, ownerSessionRecord any) (entry *pyjson.Object) {
	progressCB := child.toolProgressCallback
	pool, leasedID := leaseChildCredential(ctx, child)
	heartbeat := newChildHeartbeat(child, parent, taskIndex)
	subagentID := registerChild(ctx, child, parent, goal, ownerSessionID, ownerTransport, ownerSessionRecord)
	run := &childRun{ctx: ctx, child: child, parent: parent, taskIndex: taskIndex, goal: goal, subagentID: subagentID,
		progressCB: progressCB, heartbeat: heartbeat, childStart: time.Now()}
	closeDeferred := false
	defer func() { run.cleanup(pool, leasedID, closeDeferred) }()
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		if isUserInterruptPanic(r) {
			panic(r)
		}
		late := run.closeSteering()
		msg := fmt.Sprint(r)
		if err, ok := r.(error); ok {
			msg = err.Error()
		}
		delegateLog().Error(fmt.Sprintf("[subagent-%d] failed", taskIndex), "error", r)
		entry = run.finishFailed(fabricatedEntry(taskIndex, "error", msg, child, run.elapsed()), late, msg, msg, "failed")
	}()
	heartbeat.start(ctx)
	safeProgress(progressCB, "subagent.start", "", goal, nil)
	run.seedWorkspace()
	result, failure, deferred := run.awaitChild()
	closeDeferred = deferred
	if failure != nil {
		return failure
	}
	schema := validateChildOutputSchema(ctx, child, result, taskIndex, run.childTaskID, run.relayText)
	mergeLateSteer(result, subagentID, child)
	if progressCB != nil {
		delegateQuiet("Progress callback flush failed: %v", func() { flushProgressCallback(progressCB) })
	}
	duration := run.elapsed()
	entry = buildResultEntry(child, result, taskIndex, duration, schema)
	run.appendSiblingWriteReminder(entry)
	run.accountBackgroundProcesses(entry)
	run.emitComplete(result, entry, duration)
	return run.attachWorktree(entry)
}
