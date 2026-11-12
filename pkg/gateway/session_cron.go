package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/state"
)

// CronOwnerExecution is the cron admission a context runs under, the owner's exact store and the occurrence ids
type CronOwnerExecution struct {
	DB          *state.DB
	DBPath      string
	SessionID   string
	JobID       string
	ExecutionID string
}

type cronExecutionKey struct{}

// Cron owner execution bound to ctx, false outside a canonical cron run
func CurrentCronExecution(ctx context.Context) (CronOwnerExecution, bool) {
	exec, ok := ctx.Value(cronExecutionKey{}).(CronOwnerExecution)
	return exec, ok
}

// Context carrying the cron admission of an owner's store, session, job and execution
func BindCronExecution(ctx context.Context, exec CronOwnerExecution) context.Context {
	return context.WithValue(ctx, cronExecutionKey{}, exec)
}

// Session authority surface the cron owner registry keys by, the path of its state database
type cronOwnerAuthority interface {
	DBPath() string
}

type boundCronOwner struct {
	pid       int
	authority cronOwnerAuthority
	loop      context.Context
}

var (
	cronOwnersMu sync.Mutex
	cronOwners   = map[string]boundCronOwner{}
)

// Resolved path of a home, panicking where pathlib resolve raises
func resolvedCronHome(p string) string {
	resolved, err := pypath.Resolve(p)
	if err != nil {
		panic(err)
	}
	return resolved
}

// Registers an authority as the cron owner of its store home for this process while its loop lives
func bindCronOwner(loop context.Context, authority cronOwnerAuthority) {
	home := pypath.PathParent(resolvedCronHome(authority.DBPath()))
	cronOwnersMu.Lock()
	defer cronOwnersMu.Unlock()
	cronOwners[home] = boundCronOwner{pid: os.Getpid(), authority: authority, loop: loop}
}

// Drops the cron owner binding of its store home when this authority still holds it
func unbindCronOwner(authority cronOwnerAuthority) {
	home := pypath.PathParent(resolvedCronHome(authority.DBPath()))
	cronOwnersMu.Lock()
	defer cronOwnersMu.Unlock()
	if bound, ok := cronOwners[home]; ok && bound.authority == authority {
		delete(cronOwners, home)
	}
}

// Authority owning cron for a home in this process, false when unbound, foreign or its loop has closed
func CronOwnerForHome(home string) (authority any, ok bool) {
	resolved := resolvedCronHome(home)
	cronOwnersMu.Lock()
	bound, found := cronOwners[resolved]
	cronOwnersMu.Unlock()
	if found && bound.pid == os.Getpid() && bound.loop.Err() == nil {
		return bound.authority, true
	}
	return nil, false
}

// Admits a cron result into a local chat's FIFO on the owner, refused when the chat names no session
func admitLocalDelivery(ctx context.Context, a *SessionAuthority, chatID, text string, metadata map[string]any) (bool, error) {
	ctx = ownerScope(ctx, a, false)
	live, ok := a.live(chatID)
	if !ok {
		canMount, err := mountable(ctx, a, chatID)
		if err != nil {
			return false, err
		}
		if canMount {
			if _, err := restoreRoutedSession(ctx, a, chatID); err != nil {
				return false, err
			}
		} else if _, err := restoreLocalSession(ctx, a, chatID); err != nil {
			return false, err
		}
	} else if live.Source == nil || live.Source.Platform == PlatformLocal {
		if _, err := restoreLocalSession(ctx, a, chatID); err != nil {
			return false, err
		}
	}
	live, ok = a.live(chatID)
	if !ok {
		return false, &pyKeyError{key: chatID}
	}
	source := live.Source
	var adapter PlatformAdapter
	if source.Platform == PlatformLocal {
		adapters, err := localAdapterMap(ctx, a)
		if err != nil {
			return false, err
		}
		adapter, _ = adapters.Get(PlatformLocal)
	} else {
		owner, err := a.Runner.AdapterForSource(ctx, source)
		if err != nil {
			return false, err
		}
		adapter = owner
	}
	return admitRouteWake(ctx, a.Runner, adapter, source, text, "cron", metadata)
}

// Python TypeError or AttributeError of a stored terminal result that is not a mapping
type cronResultShapeError struct{ typeName, msg string }

func (e *cronResultShapeError) Error() string      { return e.msg }
func (e *cronResultShapeError) PyTypeName() string { return e.typeName }

// Python KeyError of a missing mapping key
type pyKeyError struct{ key string }

func (e *pyKeyError) Error() string { return pyrepr.Repr(e.key) }

// Python repr of an exception, its class name around the repr of its message
func pyExceptionRepr(err error) string {
	var keyErr *pyKeyError
	if errors.As(err, &keyErr) {
		return "KeyError(" + pyrepr.Repr(keyErr.key) + ")"
	}
	if err.Error() == "" {
		return pyrepr.ExceptionName(err) + "()"
	}
	return pyrepr.ExceptionName(err) + "(" + pyrepr.Repr(err.Error()) + ")"
}

// Seconds a cross goroutine local chat delivery waits for the owner to confirm it
const localDeliveryTimeout = 60 * time.Second

// Admits a cron result into local chat chatID through this process's owner, nil once admitted else the error text
func DeliverToLocalChat(ctx context.Context, home, chatID, content string, diagnostic bool) (*string, error) {
	owner, ok := CronOwnerForHome(home)
	if !ok {
		msg := fmt.Sprintf("local chat %s: no gateway in this process owns its session", chatID)
		return &msg, nil
	}
	authority := owner.(*SessionAuthority)
	metadata := map[string]any{}
	if diagnostic {
		metadata["notification_category"] = "diagnostic"
	}
	type outcome struct {
		admitted bool
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		admitted, err := admitLocalDelivery(context.WithoutCancel(ctx), authority, chatID, content, metadata)
		done <- outcome{admitted, err}
	}()
	var result outcome
	select {
	case result = <-done:
	case <-time.After(localDeliveryTimeout):
		result = outcome{err: errLocalDeliveryTimeout}
	}
	if result.err != nil {
		var refusal *state.RuntimeStoreError
		var keyErr *pyKeyError
		if errors.As(result.err, &refusal) || errors.As(result.err, &keyErr) || errors.Is(result.err, errLocalDeliveryTimeout) {
			msg := fmt.Sprintf("local chat %s: delivery not confirmed: %s", chatID, pyExceptionRepr(result.err))
			return &msg, nil
		}
		return nil, result.err
	}
	if result.admitted {
		return nil, nil
	}
	msg := fmt.Sprintf("local chat %s: delivery refused: the chat has no committed route", chatID)
	return &msg, nil
}

// Python TimeoutError of a stalled owner, repr TimeoutError()
var errLocalDeliveryTimeout = &timeoutError{}

type timeoutError struct{}

func (*timeoutError) Error() string { return "" }

// Python class name of the stalled owner error
func (*timeoutError) PyTypeName() string { return "TimeoutError" }

// Cron identity of a fire, the profile scheduler principal with the caller's capabilities and transport
func cronActor(a *SessionAuthority, actor *Principal) (Principal, error) {
	if actor == nil {
		return Principal{Subject: state.CronPrincipalID, ProfileID: a.ProfileID,
			Capabilities: map[string]bool{"session:create": true, "session:submit": true, "session:read": true, "session:control": true},
			TransportID:  "cron-ticker"}, nil
	}
	if actor.ProfileID != a.ProfileID {
		return Principal{}, runtimeRefusal("profile_mismatch")
	}
	if !actor.Capabilities["session:create"] || !actor.Capabilities["session:submit"] || !actor.Capabilities["session:control"] {
		return Principal{}, runtimeRefusal("permission_denied")
	}
	return Principal{Subject: state.CronPrincipalID, ProfileID: a.ProfileID, Capabilities: actor.Capabilities, TransportID: actor.TransportID}, nil
}

// Exact set of params keys
func paramsKeysAre(params map[string]any, keys ...string) bool {
	if len(params) != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := params[k]; !ok {
			return false
		}
	}
	return true
}

// Fresh admission guard, the membership test a case answers the way a str subclass overrides __contains__
var cronExecutionIDHasColon = func(executionID string) bool { return strings.Contains(executionID, ":") }

// Replaces the fresh admission colon guard and returns the one it replaced
func SwapCronExecutionIDGuard(guard func(executionID string) bool) func(executionID string) bool {
	prev := cronExecutionIDHasColon
	cronExecutionIDHasColon = guard
	return prev
}

// Staged creation receipt of a fresh cron session, or the ref of an exact retry restored from the store
func cronCreate(ctx context.Context, a *SessionAuthority, actor Principal, params map[string]any) (*pyjson.Object, *SessionRef, string, error) {
	if !paramsKeysAre(params, "job_id", "request_id", "extra_prompt") {
		return nil, nil, "", runtimeRefusal("invalid_params")
	}
	jid, jidOK := params["job_id"].(string)
	requestID, ridOK := params["request_id"].(string)
	extra, extraOK := params["extra_prompt"].(string)
	if !jidOK || jid == "" || !ridOK || requestID == "" || len([]rune(requestID)) > 256 || (params["extra_prompt"] != nil && !extraOK) {
		return nil, nil, "", runtimeRefusal("invalid_params")
	}
	executionID := requestID
	requestID = state.CronRequestID(jid, requestID)
	sid, err := state.LocalIdentity(a.ProfileID, actor.Subject, requestID)
	if err != nil {
		return nil, nil, "", err
	}
	row, err := a.DB.GetSession(ctx, sid)
	if err != nil {
		return nil, nil, "", err
	}
	if row != nil {
		ref, err := restoreLocalSession(ctx, a, sid)
		if err != nil {
			return nil, nil, "", err
		}
		return nil, &ref, requestID, nil
	}
	if cronExecutionIDHasColon(executionID) {
		return nil, nil, "", runtimeRefusal("invalid_params")
	}
	hooks := cronOwnerHooks()
	job, err := hooks.GetJob(ctx, jid)
	if err != nil {
		return nil, nil, "", err
	}
	if job == nil {
		return nil, nil, "", runtimeRefusal("not_found")
	}
	if noAgent, _ := job.Get("no_agent"); pyTruthy(noAgent) {
		return nil, nil, "", runtimeRefusal("not_found")
	}
	cfg := loadGatewayConfig(ctx, "")
	private := &RecoveredSecrets{}
	launch := map[string]any{"source": "cli", "request_id": requestID, "toolsets": []any{}}
	if workdir, _ := job.Get("workdir"); pyTruthy(workdir) {
		launch["cwd"] = workdir
	}
	policy, err := buildPolicy(ctx, launch, cfg, buildPolicyOptions{PrivateSecrets: private})
	if err != nil {
		return nil, nil, "", err
	}
	name := jid
	if n, _ := job.Get("name"); pyTruthy(n) {
		name = pyrepr.Str(n)
	}
	model, err := hooks.JobModel(ctx, job, jid, name)
	if err != nil {
		return nil, nil, "", err
	}
	var extraValue any
	if params["extra_prompt"] != nil {
		extraValue = extra
	}
	requestJSON, err := pyjson.Dumps(pyjson.ObjectOf("cron_job", job, "extra_prompt", extraValue, "request_id", requestID))
	if err != nil {
		return nil, nil, "", err
	}
	replaced := *policy
	replaced.Source, replaced.Platform, replaced.Model, replaced.RequestJSON = "cron", "cron", model, requestJSON
	bound, err := bindLaunchKey(ctx, a, sid, &replaced, nil, private)
	if err != nil {
		return nil, nil, "", err
	}
	source, err := localSource(ctx, a, sid, actor.Subject)
	if err != nil {
		return nil, nil, "", err
	}
	route, err := a.Runner.SessionStore().generateSessionKey(ctx, source, nil)
	if err != nil {
		return nil, nil, "", err
	}
	now := sessionNow()
	entry := NewSessionEntry(route, sid, now, now)
	entry.Origin = source
	entry.Platform = PlatformLocal
	return pyjson.ObjectOf("profile_id", a.ProfileID, "principal_id", actor.Subject, "request_id", requestID,
		"session_id", sid, "route", route, "entry", entry.ToDict(), "policy", localPolicyAsDict(bound)), nil, requestID, nil
}

// Dataclass dict of a frozen local session policy in field order
func localPolicyAsDict(p *LocalSessionPolicy) *pyjson.Object {
	toolsets := make([]any, len(p.Toolsets))
	for i, t := range p.Toolsets {
		toolsets[i] = t
	}
	return pyjson.ObjectOf("source", p.Source, "platform", p.Platform, "cwd", p.Cwd, "model", optStrAny(p.Model),
		"toolsets", toolsets, "config_json", p.ConfigJSON, "request_json", p.RequestJSON, "terminal_json", p.TerminalJSON,
		"credential_ref", optStrAny(p.CredentialRef), "config_secret_ref", optStrAny(p.ConfigSecretRef),
		"editor_mcp_json", optStrAny(p.EditorMCPJSON), "kanban_json", optStrAny(p.KanbanJSON), "safe_mode", p.SafeMode,
		"ignore_user_config", p.IgnoreUserConfig, "skills_prompt", optStrAny(p.SkillsPrompt), "coding_posture", p.CodingPosture,
		"client_env_json", optStrAny(p.ClientEnvJSON), "client_env_remote", p.ClientEnvRemote,
		"client_env_rules_json", optStrAny(p.ClientEnvRulesJSON), "edit_approval_policy", p.EditApprovalPolicy)
}

// Settlement descriptor of a message typed into a scheduled job's session
var cronSessionReadOnlySurface = []any{"layer", "gateway", "code", "cron_session_read_only", "retryable", false}

// Run side verdicts run_job stamps on its job copy for the firing scheduler's bookkeeping
var runJobFlags = []string{"_model_unreachable", "_quota_hold_seconds"}

// Cron scheduler functions the owner reaches, installed by pkg/cron which imports this package
type CronOwnerHooks struct {
	GetJob           func(ctx context.Context, jobID string) (*pyjson.Object, error)
	JobModel         func(ctx context.Context, job *pyjson.Object, jobID, jobName string) (*string, error)
	GetExecution     func(ctx context.Context, executionID string) (map[string]any, error)
	EnterExecution   func(ctx context.Context, job *pyjson.Object, executionID string, record map[string]any) context.Context
	RunJob           func(ctx context.Context, job *pyjson.Object, executionID string, extraPrompt *string, cancel *pythread.Event) (CronRunResult, error)
	OwnerProgress    func(executionID string) any
	PopOwnerProgress func(executionID string)
}

// Success flag, run document, final response and error text of one cron run
type CronRunResult struct {
	Success       bool
	Output        string
	FinalResponse string
	Error         *string
}

var (
	cronOwnerHooksMu  sync.Mutex
	installedCronHook *CronOwnerHooks
)

// Installs the cron scheduler functions the owner reaches
func SetCronOwnerHooks(h *CronOwnerHooks) {
	cronOwnerHooksMu.Lock()
	defer cronOwnerHooksMu.Unlock()
	installedCronHook = h
}

// Installed cron scheduler functions, panicking when pkg/cron never installed them
func cronOwnerHooks() *CronOwnerHooks {
	cronOwnerHooksMu.Lock()
	defer cronOwnerHooksMu.Unlock()
	if installedCronHook == nil {
		panic("gateway cron owner hooks unassigned: pkg/cron installs them at init")
	}
	return installedCronHook
}

// Runs one cron owner operation on the authority as the profile scheduler, the in process door of cron firing
func CronOwnerOperation(ctx context.Context, authority any, op string, params map[string]any) (map[string]any, error) {
	return cronOperation(ctx, authority.(*SessionAuthority), op, params, nil)
}

// Submits, recovers, polls or cancels a persisted job's fire through the ordinary admission ledger
func cronOperation(ctx context.Context, a *SessionAuthority, name string, params map[string]any, actorIn *Principal) (map[string]any, error) {
	actor, err := cronActor(a, actorIn)
	if err != nil {
		return nil, err
	}
	if err := requireServing(a); err != nil {
		return nil, err
	}
	switch name {
	case "recover":
		return cronRecover(ctx, a, params, actor)
	case "submit":
		return cronSubmit(ctx, a, params, actor)
	}
	if !paramsKeysAre(params, "session_id", "admission_id") || (name != "status" && name != "cancel") {
		return nil, runtimeRefusal("invalid_params")
	}
	sessionID, _ := params["session_id"].(string)
	admissionID, _ := params["admission_id"].(string)
	ref := SessionRef{ProfileID: a.ProfileID, SessionID: sessionID}
	if err := a.Authorize(ctx, actor, ref, "session:submit"); err != nil {
		return nil, err
	}
	row, err := a.DB.GetSessionAdmission(ctx, admissionID)
	if err != nil {
		return nil, err
	}
	if row == nil || rowString(row, "target_session_id") != ref.SessionID {
		return nil, runtimeRefusal("permission_denied")
	}
	if principal, _ := row.Get("principal_id"); !SameOwner(principal, actor.Subject) {
		return nil, runtimeRefusal("permission_denied")
	}
	if name == "cancel" {
		if rowString(row, "status") == AdmissionQueued {
			claimed, err := a.CancelOrClaimed(ctx, actor, ref, rowString(row, "admission_id"))
			if err != nil {
				return nil, err
			}
			if claimed == nil {
				claimed = pyjson.ObjectOf("status", "terminal")
			}
			row = claimed
		}
		if rowString(row, "status") == AdmissionStarted {
			cronCancellationEvent(a, rowString(row, "admission_id")).Set()
		}
		return map[string]any{"ok": true}, nil
	}
	var result any
	flags := any(pyjson.NewObject())
	if rowString(row, "status") == AdmissionTerminal {
		saved, err := admissionResult(ctx, a.DB, rowString(row, "admission_id"))
		if err != nil {
			return nil, err
		}
		if pyTruthy(saved) {
			savedObj, ok := saved.(*pyjson.Object)
			if !ok {
				return nil, &cronResultShapeError{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(saved))}
			}
			inner, found := savedObj.Get("result")
			if !found {
				return nil, &pyKeyError{key: "result"}
			}
			innerObj, ok := inner.(*pyjson.Object)
			if !ok {
				return nil, &cronResultShapeError{"AttributeError", fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(inner))}
			}
			result, _ = innerObj.Get("cron_result")
			if f, _ := innerObj.Get("cron_job_flags"); pyTruthy(f) {
				flags = f
			}
		}
		if result == nil {
			outcome, _ := row.Get("outcome")
			if !pyTruthy(outcome) {
				outcome = "unknown_execution"
			}
			result = []any{false, "", "", outcome}
		}
	}
	requestID := rowString(row, "request_id")
	key := requestID[strings.LastIndex(requestID, ":")+1:]
	return map[string]any{"status": rowString(row, "status"), "result": result, "job_flags": flags,
		"progress_at": cronOwnerHooks().OwnerProgress(key)}, nil
}

// Receipt a firer whose submit answer was lost polls, missing when the fire never committed
func cronRecover(ctx context.Context, a *SessionAuthority, params map[string]any, actor Principal) (map[string]any, error) {
	jobID, jobOK := params["job_id"].(string)
	requestIDIn, ridOK := params["request_id"].(string)
	_, extraOK := params["extra_prompt"].(string)
	if !paramsKeysAre(params, "job_id", "request_id", "extra_prompt") || !jobOK || jobID == "" || !ridOK || requestIDIn == "" ||
		(params["extra_prompt"] != nil && !extraOK) {
		return nil, runtimeRefusal("invalid_params")
	}
	requestID := state.CronRequestID(jobID, requestIDIn)
	sid, err := state.LocalIdentity(a.ProfileID, actor.Subject, requestID)
	if err != nil {
		return nil, err
	}
	rows, err := a.DB.ListSessionAdmissions(ctx, sid, false)
	if err != nil {
		return nil, err
	}
	var row *pyjson.Object
	for _, r := range rows {
		if principal, _ := r.Get("principal_id"); rowString(r, "request_id") == requestID && SameOwner(principal, actor.Subject) {
			row = r
			break
		}
	}
	if row == nil {
		return map[string]any{"status": "missing", "result": nil}, nil
	}
	admissionID := rowString(row, "admission_id")
	stateOut, err := cronOperation(ctx, a, "status", map[string]any{"session_id": sid, "admission_id": admissionID}, &actor)
	if err != nil {
		return nil, err
	}
	if _, err := restoreLocalSession(ctx, a, sid); err != nil {
		return nil, err
	}
	adapters, err := localAdapterMap(ctx, a)
	if err != nil {
		return nil, err
	}
	held, _ := adapters.Get(PlatformLocal)
	policy, ok := held.(*LocalSessionAdapter).Policies[sid]
	if !ok {
		return nil, &pyKeyError{key: sid}
	}
	frozenValue, err := pyjson.Loads(policy.RequestJSON)
	if err != nil {
		return nil, err
	}
	frozen, ok := frozenValue.(*pyjson.Object)
	if !ok {
		return nil, &cronResultShapeError{"TypeError", fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(frozenValue))}
	}
	frozenExtra, found := frozen.Get("extra_prompt")
	if !found {
		return nil, &pyKeyError{key: "extra_prompt"}
	}
	if !pyjson.Equal(frozenExtra, params["extra_prompt"]) {
		return nil, runtimeRefusal("admission_conflict")
	}
	cronJob, found := frozen.Get("cron_job")
	if !found {
		return nil, &pyKeyError{key: "cron_job"}
	}
	stateOut["job"] = cronJob
	stateOut["session_id"] = sid
	stateOut["admission_id"] = admissionID
	return stateOut, nil
}

// Creates the cron session when fresh and admits the fire's input in FIFO order off the owner loop
func cronSubmit(ctx context.Context, a *SessionAuthority, params map[string]any, actor Principal) (map[string]any, error) {
	if err := a.requireAdmissionOpen(false); err != nil {
		return nil, err
	}
	staged, ref, requestID, err := cronCreate(ownerScope(ctx, a, false), a, actor, params)
	if err != nil {
		return nil, err
	}
	if staged != nil {
		if _, err := trackedWrite(ctx, a, func(writeCtx context.Context) (any, error) {
			return nil, commitLocalSession(writeCtx, a.DB, a.Epoch, staged)
		}, nil, false, nil); err != nil {
			return nil, err
		}
		sid, _ := staged.Get("session_id")
		restored, err := restoreLocalSession(ctx, a, sid.(string))
		if err != nil {
			return nil, err
		}
		ref = &restored
	}
	text := ""
	if extra, ok := params["extra_prompt"].(string); ok && extra != "" {
		text = extra
	}
	result, err := trackedWrite(ctx, a, func(writeCtx context.Context) (any, error) {
		return a.DB.AdmitSessionInput(writeCtx, state.SessionInput{Epoch: a.Epoch, PrincipalID: actor.Subject, SessionID: ref.SessionID,
			RequestID: requestID, Payload: pyjson.ObjectOf("text", text), AuthorizeWrite: a.admissionGate(nil, false)})
	}, func(thenCtx context.Context, row any) (any, error) {
		return a.admitted(thenCtx, *ref, nil, row.(*pyjson.Object))
	}, true, func(any) { a.scheduleAdmitted(ctx, *ref) })
	if err != nil {
		return nil, err
	}
	return map[string]any{"session_id": ref.SessionID, "admission_id": result.(AdmissionReceipt).AdmissionID}, nil
}

// Cancellation latch of an admission, created on first use so a cancel before execute registers is kept
func cronCancellationEvent(a *SessionAuthority, admissionID string) *pythread.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cronCancellations == nil {
		a.cronCancellations = map[string]*pythread.Event{}
	}
	event, ok := a.cronCancellations[admissionID]
	if !ok {
		event = &pythread.Event{}
		a.cronCancellations[admissionID] = event
	}
	return event
}

// Number of pending cron cancellation latches of the authority
func (a *SessionAuthority) CronCancellations() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cronCancellations)
}

// Authenticated connection a cron RPC arrives on
type CronRPCConnection interface {
	CronAuthority() *SessionAuthority
	CronActor() *Principal
}

// Cron RPC of a gateway connection, run as the connection's actor on its authority
func CronRPC(ctx context.Context, connection CronRPCConnection, name string, params map[string]any) (map[string]any, error) {
	return cronOperation(ctx, connection.CronAuthority(), name, params, connection.CronActor())
}

// Scheduler execution id an admission carries, its request id after the cron job prefix
func schedulerExecutionID(jobID, requestID string) string {
	return strings.TrimPrefix(requestID, state.CronRequestID(jobID, ""))
}

// Runs the job inside this fire's cron identity re-bound from its ledger row
func runIdentified(ctx context.Context, job *pyjson.Object, executionID string, extraPrompt *string, cancel *pythread.Event) (CronRunResult, error) {
	hooks := cronOwnerHooks()
	record, err := hooks.GetExecution(ctx, executionID)
	if err != nil {
		return CronRunResult{}, err
	}
	if record == nil {
		record = map[string]any{}
	}
	return hooks.RunJob(hooks.EnterExecution(ctx, job, executionID, record), job, executionID, extraPrompt, cancel)
}

// Runs a cron session's own frozen fire, any other input settling with the read only descriptor
func CronExecute(ctx context.Context, a *SessionAuthority, ref SessionRef, row *pyjson.Object, policy *LocalSessionPolicy) (string, error) {
	dataValue, err := pyjson.Loads(policy.RequestJSON)
	if err != nil {
		return "", err
	}
	data := dataValue.(*pyjson.Object)
	extraValue, _ := data.Get("extra_prompt")
	extraText := ""
	if pyTruthy(extraValue) {
		extraText, _ = extraValue.(string)
	}
	payload, _ := row.Get("payload")
	dataRequestID, _ := data.Get("request_id")
	live, ok := a.live(ref.SessionID)
	if !ok {
		return "", &pyKeyError{key: ref.SessionID}
	}
	principal, _ := row.Get("principal_id")
	admissionID := rowString(row, "admission_id")
	if !pyjson.Equal(payload, pyjson.ObjectOf("text", extraText)) || rowString(row, "request_id") != dataRequestID ||
		!pyjson.Equal(principal, optStrAny(live.Source.UserID)) {
		text := i18n.T(ctx, "gateway.errors.cron_session_read_only", "", nil)
		a.mu.Lock()
		a.PendingResults[admissionID] = pyjson.ObjectOf("result", pyjson.ObjectOf(
			"final_response", "", "messages", []any{}, "failed", true, "completed", false,
			"error", text, "error_surface", pyjson.ObjectOf(cronSessionReadOnlySurface...)), "usage", pyjson.NewObject())
		a.mu.Unlock()
		return text, nil
	}
	cancel := cronCancellationEvent(a, admissionID)
	cronJobValue, _ := data.Get("cron_job")
	cronJob := cronJobValue.(*pyjson.Object)
	jobIDValue, _ := cronJob.Get("id")
	jobID, _ := jobIDValue.(string)
	executionID := schedulerExecutionID(jobID, rowString(row, "request_id"))
	job := cronJob.Copy()
	job.Set("execution_id", executionID)
	ctx = BindCronExecution(ctx, CronOwnerExecution{DB: a.DB, DBPath: a.DBPath(), SessionID: ref.SessionID, JobID: jobID, ExecutionID: executionID})
	defer func() {
		cronOwnerHooks().PopOwnerProgress(executionID)
		a.mu.Lock()
		delete(a.cronCancellations, admissionID)
		a.mu.Unlock()
	}()
	dbPath, err := pypath.Resolve(a.DBPath())
	if err != nil {
		return "", err
	}
	var extraPrompt *string
	if s, ok := extraValue.(string); ok {
		extraPrompt = &s
	}
	result, err := runIdentified(profileRuntimeScope(ctx, pypath.PathParent(dbPath), true), job, executionID, extraPrompt, cancel)
	if err != nil {
		return "", err
	}
	flags := pyjson.NewObject()
	for _, key := range runJobFlags {
		if v, ok := job.Get(key); ok {
			flags.Set(key, v)
		}
	}
	var errorValue any
	if result.Error != nil {
		errorValue = *result.Error
	}
	a.mu.Lock()
	a.PendingResults[admissionID] = pyjson.ObjectOf("result", pyjson.ObjectOf(
		"final_response", result.FinalResponse,
		"cron_result", []any{result.Success, result.Output, result.FinalResponse, errorValue},
		"cron_job_flags", flags, "failed", !result.Success, "completed", result.Success), "usage", pyjson.NewObject())
	a.mu.Unlock()
	if !result.Success {
		message := "cron execution failed"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return "", &cronRuntimeError{message: message}
	}
	return result.FinalResponse, nil
}

// Python RuntimeError a failed cron execution raises
type cronRuntimeError struct{ message string }

func (e *cronRuntimeError) Error() string { return e.message }

// Python class name of a failed cron execution
func (e *cronRuntimeError) PyTypeName() string { return "RuntimeError" }
