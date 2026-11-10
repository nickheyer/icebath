package kanban

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	KanbanListDefaultLimit = 50
	KanbanListMaxLimit     = 200
)

// Worker tools that end or hand over a run, refused to a worker that cannot name its run id
var runLifecycleTools = map[string]bool{
	"kanban_complete": true, "kanban_block": true, "kanban_schedule": true,
	"kanban_request_review": true, "kanban_request_changes": true,
}

// Argument keys a handler reads that its model facing schema leaves undeclared
var undeclaredArgs = map[string]map[string]bool{
	"kanban_create":     {"session_id": true, "project_id": true},
	"kanban_attach_url": {"title": true},
}

// One kanban tool invocation with its context, session environment and arguments
type kanbanCall struct {
	ctx  context.Context
	env  *tools.Env
	args map[string]any
}

// Home whose config and state the call reads, the env profile home before the context home
func (k *kanbanCall) home() string { return kanbanToolHome(k.ctx, k.env) }

// Argument value under a key with nil for an absent key, the dict get of the handlers
func (k *kanbanCall) get(name string) any { return k.args[name] }

// Profile home a tool environment is bound to, else the home of the context
func kanbanToolHome(ctx context.Context, env *tools.Env) string {
	if env != nil && pystr.Strip(env.Home) != "" {
		return env.Home
	}
	if ctx == nil {
		return home.Process()
	}
	return home.Dir(ctx)
}

// Secret lookup config reference expansion reads through
func kanbanConfigLookup(env *tools.Env) config.Lookup {
	if env != nil && env.Secrets != nil {
		return env.Secrets.Lookup()
	}
	return func(string) (string, bool) { return "", false }
}

// Whether the active profile opted into the kanban toolset, false on any unreadable config
func profileHasKanbanToolset(env *tools.Env) (has bool) {
	requested, inAssembly := tools.KanbanToolsetRequested(env)
	if requested {
		return true
	}
	defer func() {
		if recover() != nil {
			has = false
		}
	}()
	ctx := context.Background()
	cfg, err := config.Load(kanbanToolHome(ctx, env), kanbanConfigLookup(env))
	if err != nil {
		return false
	}
	if toolsets := cfg["toolsets"]; pyrepr.Truthy(toolsets) && tools.PyContainsKanban(toolsets) {
		return true
	}
	if inAssembly {
		return false
	}
	platforms := cfg["platform_toolsets"]
	if !pyrepr.Truthy(platforms) {
		return false
	}
	names, values, isDict := dictItems(platforms)
	if !isDict {
		return false
	}
	for i, platform := range names {
		if _, isList := values[i].([]any); !isList {
			continue
		}
		if tools.Default.PlatformTools(ctx, "", cfg, platform, tools.PlatformToolsOptions{})["kanban"] {
			return true
		}
	}
	return false
}

// Ordered dict of a dict shaped value, not ok for anything else
func dictObject(v any) (*pyjson.Object, bool) {
	if o, ok := v.(*pyjson.Object); ok && o != nil {
		return o, true
	}
	keys, values, ok := dictItems(v)
	if !ok {
		return nil, false
	}
	o := pyjson.NewObject()
	for i, key := range keys {
		o.Set(key, values[i])
	}
	return o, true
}

// Delegation context predicate named like its Python function, the default when the name is unknown
var delegationCtx = func(ctx context.Context, env *tools.Env, predicate string, def bool) bool {
	switch predicate {
	case "is_delegated_child_context":
		return tools.DelegatedChildActive(ctx, env)
	case "is_dispatcher_owned_worker_context":
		return tools.DispatcherOwnedWorkerActive(ctx, env)
	case "is_delegated_child_process_context":
		return tools.DelegatedChildProcessActive(ctx, env)
	}
	return def
}

// Whether the code runs for an in process delegate_task child
func isDelegatedChildContext(ctx context.Context, env *tools.Env) bool {
	return delegationCtx(ctx, env, "is_delegated_child_context", false)
}

// False for delegate children and in process cron runs, whenever the kanban identity is present but not ours
var isDispatcherOwnedWorker = func(ctx context.Context, env *tools.Env) bool {
	return delegationCtx(ctx, env, "is_dispatcher_owned_worker_context", true)
}

// Never for a delegate child, the flag for a dispatcher spawned worker, else the profile toolset
func kanbanVisible(env *tools.Env, toEnvWorker bool) bool {
	if isDelegatedChildContext(nil, env) {
		return false
	}
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" && isDispatcherOwnedWorker(nil, env) {
		return toEnvWorker
	}
	return profileHasKanbanToolset(env)
}

// Lifecycle tools, shown to dispatcher workers and to profiles with the kanban toolset
var checkKanbanMode = tools.NoCacheCheck(func(env *tools.Env) bool { return kanbanVisible(env, true) })

// Board routing tools, hidden from task workers
var checkKanbanOrchestratorMode = tools.NoCacheCheck(func(env *tools.Env) bool { return kanbanVisible(env, false) })

// kanbanReject carries a finished tool error payload out of a validation helper
type kanbanReject struct{ payload string }

func (r *kanbanReject) Error() string { return r.payload }

// Rejection whose payload is the tool error for the message
func reject(message string) error { return &kanbanReject{kanbanToolError(message)} }

// Rejects with the message as a tool error unless the condition holds
func check(cond bool, message string) error {
	if !cond {
		return reject(message)
	}
	return nil
}

// JSON error object the model reads for a failed tool call
func kanbanToolError(message string) string {
	return tools.ToolResult(pyjson.ObjectOf("error", message))
}

// Profile name persisted into board records, worker when nothing names a profile
func persistedIdentity(ctx context.Context) string {
	if name := profiles.CurrentProfileName(ctx); name != "" {
		return name
	}
	return "worker"
}

// Whether a failure is a Python ValueError, reported without a traceback
func isValueError(err error) bool {
	var ve *ValueError
	if errors.As(err, &ve) {
		return true
	}
	var pe *pyint.Error
	return errors.As(err, &pe) && pe.Kind == "ValueError"
}

// Wraps a handler so unknown arguments, rejections and every failure come back as a structured tool error
func kanbanHandler(toolName string, fn func(k *kanbanCall) (string, error)) tools.Handler {
	return func(ctx context.Context, call *tools.Call) (out string) {
		fail := func(err error) string {
			var rejected *kanbanReject
			if errors.As(err, &rejected) {
				return rejected.payload
			}
			if !isValueError(err) {
				kanbanToolsLog.Error(toolName+" failed", "exc_info", excText(err))
			}
			return kanbanToolError(fmt.Sprintf("%s: %s", toolName, excText(err)))
		}
		defer func() {
			if r := recover(); r != nil {
				err, isErr := r.(error)
				if !isErr {
					err = &RuntimeError{fmt.Sprint(r)}
				}
				out = fail(err)
			}
		}()
		k := &kanbanCall{ctx: ctx, env: call.Env, args: exactArgs(call)}
		properties, err := registeredProperties(toolName)
		if err != nil {
			return fail(err)
		}
		var unknown []string
		for name := range k.args {
			if _, declared := properties[name]; !declared && !undeclaredArgs[toolName][name] {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			valid := make([]string, 0, len(properties))
			for name := range properties {
				valid = append(valid, name)
			}
			sort.Strings(valid)
			return fail(reject(fmt.Sprintf("%s: unknown parameter(s): %s. Valid parameters: %s. Nothing changed.",
				toolName, strings.Join(unknown, ", "), strings.Join(valid, ", "))))
		}
		out, err = fn(k)
		if err != nil {
			return fail(err)
		}
		return out
	}
}

// Declared parameter properties of a registered tool, the error Python raises on a missing registration or shape
func registeredProperties(toolName string) (map[string]any, error) {
	def := tools.Default.Get(toolName)
	if def == nil || def.Schema == nil {
		return nil, &RuntimeError{"'NoneType' object is not subscriptable"}
	}
	parameters, ok := def.Schema["parameters"]
	if !ok {
		return nil, &RuntimeError{"'parameters'"}
	}
	paramKeys, paramValues, isDict := dictItems(parameters)
	if !isDict {
		return nil, &RuntimeError{fmt.Sprintf("'%s' object is not subscriptable", pyrepr.TypeName(parameters))}
	}
	for i, key := range paramKeys {
		if key != "properties" {
			continue
		}
		names, values, isDict := dictItems(paramValues[i])
		if !isDict {
			return nil, &RuntimeError{fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(paramValues[i]))}
		}
		out := make(map[string]any, len(names))
		for j, name := range names {
			out[name] = values[j]
		}
		return out, nil
	}
	return nil, &RuntimeError{"'properties'"}
}

// Arguments with the number types and key order the model wrote, read from the raw JSON where it still matches
func exactArgs(call *tools.Call) map[string]any {
	out := make(map[string]any, len(call.Args))
	for name, value := range call.Args {
		out[name] = value
	}
	if call.RawArgs == "" {
		return out
	}
	decoded, err := pyjson.Loads(call.RawArgs)
	if err != nil {
		return out
	}
	raw, isDict := decoded.(*pyjson.Object)
	if !isDict {
		return out
	}
	for name, value := range out {
		if exact, present := raw.Get(name); present && sameJSON(exact, value) {
			out[name] = exact
		}
	}
	return out
}

// Whether an exactly decoded JSON value and a loosely decoded one hold the same document
func sameJSON(exact, value any) bool {
	switch e := exact.(type) {
	case nil:
		return value == nil
	case bool:
		v, isBool := value.(bool)
		return isBool && v == e
	case string:
		v, isText := value.(string)
		return isText && v == e
	case []any:
		v, isList := value.([]any)
		if !isList || len(v) != len(e) {
			return false
		}
		for i := range e {
			if !sameJSON(e[i], v[i]) {
				return false
			}
		}
		return true
	case *pyjson.Object:
		keys, values, isDict := dictItems(value)
		if !isDict || len(keys) != e.Len() {
			return false
		}
		for i, key := range keys {
			member, present := e.Get(key)
			if !present || !sameJSON(member, values[i]) {
				return false
			}
		}
		return true
	}
	if _, isBool := value.(bool); isBool {
		return false
	}
	return pyjson.Equal(exact, value)
}

// Refuses board mutations to a delegate_task child, which shares its parent's kanban environment
func rejectDelegatedChildMutation(k *kanbanCall, toolName string) error {
	if delegationCtx(k.ctx, k.env, "is_delegated_child_process_context", false) {
		return reject(toolName + " refused: delegate_task child agents are not Kanban run owners. " +
			"Return findings to the parent agent; the dispatcher worker or an explicitly " +
			"configured Kanban orchestrator must perform board mutations.")
	}
	return nil
}

// Task id argument, else the task the dispatcher spawned this worker for, not ok when neither applies
func defaultTaskID(k *kanbanCall, arg any) (string, bool) {
	if arg != nil {
		if val := pystr.Strip(pyrepr.Str(arg)); val != "" {
			return val, true
		}
	}
	if isDelegatedChildContext(k.ctx, k.env) {
		return "", false
	}
	if !isDispatcherOwnedWorker(k.ctx, k.env) {
		return "", false
	}
	if val := pystr.Strip(os.Getenv("ICEBATH_KANBAN_TASK")); val != "" {
		return val, true
	}
	return "", false
}

// Target task of the call, rejected with a message the caller can act on when none resolves
func requireTaskID(k *kanbanCall) (string, error) {
	if tid, ok := defaultTaskID(k, k.get("task_id")); ok {
		return tid, nil
	}
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
		return "", reject("task_id is required: this session does not own the inherited " +
			"ICEBATH_KANBAN_TASK, so it is not a valid default. Pass an explicit " +
			"task_id.")
	}
	return "", reject("task_id is required: this session has no dispatcher-assigned task to " +
		"default to. Pass an explicit task_id; discover task ids with kanban_list.")
}

// Run id this worker passes as the expected run, nil when it is not scoped to the task
func expectedRunID(taskID string) *int64 { return workerRunIDArg(taskID) }

// Metadata with the trusted worker session id added for this worker's own task
func stampWorkerSessionMetadata(taskID string, metadata *pyjson.Object) *pyjson.Object {
	sessionID, _ := ownTaskEnv(taskID, "ICEBATH_SESSION_ID")
	if sessionID == "" {
		return metadata
	}
	out := pyjson.NewObject()
	if metadata != nil {
		for _, key := range metadata.Keys {
			out.Set(key, metadata.Vals[key])
		}
	}
	out.Set("worker_session_id", sessionID)
	return out
}

// Refuses a dispatcher spawned worker mutating any task but its own
func enforceWorkerTaskOwnership(tid string) error {
	if envTID := os.Getenv("ICEBATH_KANBAN_TASK"); envTID != "" && tid != envTID {
		return reject(fmt.Sprintf("worker is scoped to task %s; refusing to mutate %s. Use kanban_comment "+
			"to hand off information to other tasks, or kanban_create to spawn follow-up work.", envTID, tid))
	}
	return nil
}

// Worker mutation preamble returning the task id after child, scope and run identity checks
func workerGuard(k *kanbanCall, toolName string) (string, error) {
	if err := rejectDelegatedChildMutation(k, toolName); err != nil {
		return "", err
	}
	tid, err := requireTaskID(k)
	if err != nil {
		return "", err
	}
	if err := enforceWorkerTaskOwnership(tid); err != nil {
		return "", err
	}
	if runLifecycleTools[toolName] && os.Getenv("ICEBATH_KANBAN_TASK") != "" && expectedRunID(tid) == nil {
		return "", reject(toolName + " refused: this worker cannot resolve its " +
			"ICEBATH_KANBAN_RUN_ID, so it cannot prove ownership of the card's " +
			"current run. A stale or unbound worker must not terminate a run a " +
			"live successor owns. Re-run through the dispatcher so the run id is " +
			"pinned, or use an orchestrator/CLI path that passes an explicit " +
			"expected_run_id.")
	}
	return tid, nil
}

// Refuses an orchestrator only tool to a dispatcher spawned worker that reached it anyway
func requireOrchestratorTool(toolName string) error {
	if os.Getenv("ICEBATH_KANBAN_TASK") != "" {
		return reject(toolName + " is orchestrator-only; dispatcher-spawned workers must use " +
			"kanban_complete, kanban_request_review, kanban_request_changes, kanban_block, " +
			"kanban_heartbeat, or kanban_comment for their assigned task.")
	}
	return nil
}

// Board slug argument as text, nil keeping the env and symlink resolution chain, an invalid slug named as passed
func boardArg(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s := pyrepr.Str(v)
	if _, isText := v.(string); !isText {
		if _, err := normalizeBoardSlug(&s); err != nil {
			return nil, &ValueError{fmt.Sprintf("invalid board slug %s: must be 1-64 chars, lowercase "+
				"alphanumerics / hyphens / underscores, not starting with '-' or '_'", pyrepr.Repr(v))}
		}
	}
	return &s, nil
}

// Runs fn on the board connection and closes it, a close failure replacing any outcome unless quiet
func withBoard(ctx context.Context, rawBoard any, quietClose bool, fn func(conn state.SQLiteConn) (string, error)) (out string, err error) {
	board, err := boardArg(rawBoard)
	if err != nil {
		return "", err
	}
	conn, err := Connect(ctx, "", board)
	if err != nil {
		return "", err
	}
	defer func() {
		raised := recover()
		if cerr := conn.Close(); cerr != nil && !quietClose {
			out, err = "", cerr
			return
		}
		if raised != nil {
			panic(raised)
		}
	}()
	return fn(conn)
}

// Task row of an id, rejected when the board has none
func existingTask(ctx context.Context, conn state.SQLiteConn, tid string) (*Task, error) {
	task, err := GetTask(ctx, conn, tid)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, reject(fmt.Sprintf("task %s not found", tid))
	}
	return task, nil
}

// Success payload led by ok true with the given fields in order
func okPayload(fields ...any) (string, error) {
	o := pyjson.ObjectOf("ok", true)
	for i := 0; i+1 < len(fields); i += 2 {
		o.Set(fields[i].(string), fields[i+1])
	}
	return pyjson.Dumps(o)
}

// Success payload reporting the run and the status the task actually landed in
func okLanded(ctx context.Context, conn state.SQLiteConn, tid, defaultStatus string, extra ...any) (string, error) {
	run, err := LatestRun(ctx, conn, tid)
	if err != nil {
		return "", err
	}
	landed, err := GetTask(ctx, conn, tid)
	if err != nil {
		return "", err
	}
	var runID any
	if run != nil {
		runID = run.ID
	}
	status := defaultStatus
	if landed != nil {
		status = landed.Status
	}
	return okPayload(append([]any{"task_id", tid, "run_id", runID, "status", status}, extra...)...)
}

// Text of a value with every secret pattern masked
func redactText(env *tools.Env, value any) string {
	return tools.RedactSensitiveTextWith(env, pyrepr.Str(value), tools.RedactOptions{Force: true})
}

// Redacted text of a truthy value, the value itself when falsy
func redactOpt(env *tools.Env, value any) any {
	if !pyrepr.Truthy(value) {
		return value
	}
	return redactText(env, value)
}

// Metadata redacted through a JSON round trip, nil when the redacted text no longer parses as a dict
func redactMetadata(env *tools.Env, metadata *pyjson.Object) (*pyjson.Object, error) {
	text, err := pyjson.Dumps(metadata)
	if err != nil {
		return nil, err
	}
	decoded, err := pyjson.Loads(tools.RedactSensitiveTextWith(env, text, tools.RedactOptions{Force: true}))
	if err != nil {
		var bad *pyjson.DecodeError
		if errors.As(err, &bad) {
			return nil, nil
		}
		return nil, err
	}
	out, _ := decoded.(*pyjson.Object)
	return out, nil
}

// A string or list argument as a list, stripped and emptied of blanks when asked, nil for an absent one
func coerceStrList(value any, name, what string, strip bool) ([]any, bool, error) {
	if value == nil {
		return nil, false, nil
	}
	var items []any
	switch v := value.(type) {
	case string:
		items = []any{v}
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	case pyrepr.Tuple:
		items = []any(v)
	default:
		return nil, false, reject(fmt.Sprintf("%s must be a list of %s, got %s", name, what, pyrepr.TypeName(value)))
	}
	if !strip {
		return items, true, nil
	}
	out := []any{}
	for _, x := range items {
		if s := pystr.Strip(pyrepr.Str(x)); s != "" {
			out = append(out, s)
		}
	}
	return out, true, nil
}

// Text of every list item, the sqlite text a non string item binds as
func strItems(items []any) []string {
	out := make([]string, 0, len(items))
	for _, x := range items {
		out = append(out, pyrepr.Str(x))
	}
	return out
}

// Rejects metadata that is neither absent nor a dict
func requireDictMetadata(metadata any) error {
	if metadata == nil {
		return nil
	}
	if _, _, isDict := dictItems(metadata); isDict {
		return nil
	}
	return reject(fmt.Sprintf("metadata must be an object/dict, got %s", pyrepr.TypeName(metadata)))
}

// Folds declared artifact paths into the metadata artifacts list, merged with a list already there
func mergeArtifacts(metadata any, artifacts []any) (*pyjson.Object, error) {
	if err := requireDictMetadata(metadata); err != nil {
		return nil, err
	}
	out := pyjson.NewObject()
	if metadata != nil {
		out, _ = dictObject(metadata)
	}
	existing, _ := out.Get("artifacts")
	var prior []any
	switch e := existing.(type) {
	case []any:
		prior = e
	case pyrepr.Tuple:
		prior = []any(e)
	default:
		out.Set("artifacts", artifacts)
		return out, nil
	}
	merged, seen := []any{}, map[string]bool{}
	for _, item := range append(append([]any{}, prior...), artifacts...) {
		if s := pystr.Strip(pyrepr.Str(item)); s != "" && !seen[s] {
			seen[s] = true
			merged = append(merged, s)
		}
	}
	out.Set("artifacts", merged)
	return out, nil
}

// Argument under a name, rejected when missing or blank
func requireText(k *kanbanCall, name, message string) (any, error) {
	value := k.get(name)
	if message == "" {
		message = name + " is required"
	}
	if !pyrepr.Truthy(value) || pystr.Strip(pyrepr.Str(value)) == "" {
		return nil, reject(message)
	}
	return value, nil
}

var boolWords = map[string]bool{"true": true, "1": true, "yes": true, "false": false, "0": false, "no": false}

// Boolean argument given as a bool or one of the true and false words, false when absent
func parseBoolArg(k *kanbanCall, name string) (bool, error) {
	value := k.get(name)
	if value == nil {
		return false, nil
	}
	if b, isBool := value.(bool); isBool {
		return b, nil
	}
	parsed, known := boolWords[pystr.Lower(pystr.Strip(pyrepr.Str(value)))]
	if !known {
		return false, reject(name + " must be a boolean or 'true'/'false'")
	}
	return parsed, nil
}

// Python int of an optional argument, nil when it is absent
func optIntArg(value any) (*big.Int, error) {
	if value == nil {
		return nil, nil
	}
	return pyint.Of(value)
}

var (
	taskFields = strings.Fields("id title body assignee status tenant priority workspace_kind workspace_path created_by " +
		"created_at started_at completed_at result current_run_id model_override " +
		"provider_override completion_contract last_failure_error")
	taskSummaryFields = strings.Fields("id title assignee status priority tenant workspace_kind workspace_path project_id created_by " +
		"created_at started_at completed_at current_run_id model_override provider_override")
	runFields        = strings.Fields("id profile status outcome summary error metadata started_at ended_at")
	commentFields    = []string{"author", "body", "created_at"}
	eventFields      = []string{"kind", "payload", "created_at", "run_id"}
	attachmentFields = strings.Fields("id filename content_type size uploaded_by stored_path created_at")
	createdFields    = []string{"status", "workspace_kind", "workspace_path", "project_id"}
)

// Named attributes of a board row as an ordered dict, every value None for a missing row
func fieldsOf(obj any, names []string) *pyjson.Object {
	out := pyjson.NewObject()
	for _, name := range names {
		out.Set(name, rowField(obj, name))
	}
	return out
}

// One attribute of a task, run, comment, event or attachment row by its Python name
func rowField(obj any, name string) any {
	switch r := obj.(type) {
	case *Task:
		if r == nil {
			return nil
		}
		switch name {
		case "id":
			return r.ID
		case "title":
			return r.Title
		case "body":
			return optStrValue(r.Body)
		case "assignee":
			return optStrValue(r.Assignee)
		case "status":
			return r.Status
		case "tenant":
			return optStrValue(r.Tenant)
		case "priority":
			return r.Priority
		case "workspace_kind":
			return r.WorkspaceKind
		case "workspace_path":
			return optStrValue(r.WorkspacePath)
		case "project_id":
			return optStrValue(r.ProjectID)
		case "created_by":
			return optStrValue(r.CreatedBy)
		case "created_at":
			return optIntValue(r.CreatedAt)
		case "started_at":
			return optIntValue(r.StartedAt)
		case "completed_at":
			return optIntValue(r.CompletedAt)
		case "result":
			return optStrValue(r.Result)
		case "current_run_id":
			return optIntValue(r.CurrentRunID)
		case "model_override":
			return optStrValue(r.ModelOverride)
		case "provider_override":
			return optStrValue(r.ProviderOverride)
		case "completion_contract":
			return optStrValue(r.CompletionContract)
		case "last_failure_error":
			return optStrValue(r.LastFailureError)
		}
	case *Run:
		if r == nil {
			return nil
		}
		switch name {
		case "id":
			return r.ID
		case "profile":
			return optStrValue(r.Profile)
		case "status":
			return r.Status
		case "outcome":
			return optStrValue(r.Outcome)
		case "summary":
			return optStrValue(r.Summary)
		case "error":
			return optStrValue(r.Error)
		case "metadata":
			return r.Metadata
		case "started_at":
			return r.StartedAt
		case "ended_at":
			return optIntValue(r.EndedAt)
		}
	case *Comment:
		if r == nil {
			return nil
		}
		switch name {
		case "author":
			return r.Author
		case "body":
			return r.Body
		case "created_at":
			return r.CreatedAt
		}
	case *Event:
		if r == nil {
			return nil
		}
		switch name {
		case "kind":
			return r.Kind
		case "payload":
			return r.Payload
		case "created_at":
			return r.CreatedAt
		case "run_id":
			return optIntValue(r.RunID)
		}
	case *Attachment:
		if r == nil {
			return nil
		}
		switch name {
		case "id":
			return r.ID
		case "filename":
			return r.Filename
		case "content_type":
			return optStrValue(r.ContentType)
		case "size":
			return r.Size
		case "uploaded_by":
			return optStrValue(r.UploadedBy)
		case "stored_path":
			return r.StoredPath
		case "created_at":
			return r.CreatedAt
		}
	}
	panic(&RuntimeError{fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(obj), name)})
}

// Summary row of a task with its parent and child ids and their counts
func taskSummaryDict(ctx context.Context, conn state.SQLiteConn, task *Task) (*pyjson.Object, error) {
	parents, err := ParentIDs(ctx, conn, task.ID)
	if err != nil {
		return nil, err
	}
	children, err := ChildIDs(ctx, conn, task.ID)
	if err != nil {
		return nil, err
	}
	out := fieldsOf(task, taskSummaryFields)
	out.Set("parents", idList(parents))
	out.Set("children", idList(children))
	out.Set("parent_count", int64(len(parents)))
	out.Set("child_count", int64(len(children)))
	return out, nil
}

// Task ids as a JSON list, empty rather than null when there are none
func idList(ids []string) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}
