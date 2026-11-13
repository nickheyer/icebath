package observability

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Records one loop mark inside the profile home the producer captured, an empty home keeping the bound one
func emitLoop(ctx context.Context, mark, homeDir string, build func() map[string]string) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("Shared-metrics %s not recorded", mark), "error", r)
		}
	}()
	emit(boundHome(ctx, homeDir), mark, build)
}

// Dict form of a tool result, decoding JSON text and reading anything else as an empty dict
func parsedResult(result any) map[string]any {
	if d, ok := asDict(result); ok {
		return d
	}
	var text string
	switch r := result.(type) {
	case string:
		text = r
	case []byte:
		s, err := pyjson.DecodeBytes(r)
		if err != nil {
			return map[string]any{}
		}
		text = s
	default:
		return map[string]any{}
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return map[string]any{}
	}
	if d, ok := asDict(data); ok {
		return d
	}
	return map[string]any{}
}

// Profile home the current context resolves, captured by producers that hand work to other goroutines
func currentHome(ctx context.Context) string {
	return home.Dir(ctx)
}

// Closed memory operation each provider verb maps to
var memoryOpWords = func() map[string]string {
	words := map[string]string{}
	for op, verbs := range map[string][]string{
		"add":     {"add", "conclude", "create", "ingest", "remember", "retain", "save", "store", "upload"},
		"replace": {"edit", "replace", "update"},
		"remove":  {"delete", "forget", "remove"},
		"search":  {"probe", "query", "reason", "reasoning", "recall", "related", "search"},
		"read":    {"context", "get", "list", "profile", "read"},
	} {
		for _, v := range verbs {
			words[v] = op
		}
	}
	return words
}()

// Rows one memory call exports at most however many operations its batch carries
const maxBatchOps = 25

// Closed operation for one action word, other when unknown
func MemoryOp(value any) string {
	if op, ok := memoryOpWords[norm(value)]; ok {
		return op
	}
	return "other"
}

// Operation a provider tool performs from its action argument, else the last verb in its name
func MemoryProviderOp(toolName, args any) string {
	if d, ok := asDict(args); ok {
		if action, ok := d["action"].(string); ok {
			if op, ok := memoryOpWords[norm(action)]; ok {
				return op
			}
		}
	}
	words := strings.Split(strings.ReplaceAll(norm(toolName), "-", "_"), "_")
	for i := len(words) - 1; i >= 0; i-- {
		if op, ok := memoryOpWords[words[i]]; ok {
			return op
		}
	}
	return "other"
}

// Contract provider name, builtin when empty and plugin when off the catalog
func MemoryProviderName(value any) string {
	name := norm(value)
	if name == "" {
		name = "builtin"
	}
	if MemoryProviders.Has(name) {
		return name
	}
	return "plugin"
}

// Write origin the env carries, background_review inside the review and curator forks
func isBackgroundReviewEnv(env *tools.Env) bool {
	if env == nil || env.Values == nil {
		return false
	}
	origin, _ := env.Values.Load("write_origin")
	return origin == "background_review"
}

// Origin dimension of a memory operation from the session write origin
func memoryOrigin(env *tools.Env) string {
	if isBackgroundReviewEnv(env) {
		return "background_review"
	}
	return "foreground"
}

// Bounded dimensions of one memory operation, success forcing failure class none
func MemoryOpFields(op string, provider, outcome, origin, failureClass any) map[string]string {
	outcomeValue, originValue, classValue := norm(outcome), norm(origin), norm(failureClass)
	if !MemoryOpOutcomes.Has(outcomeValue) {
		outcomeValue = "failed"
	}
	fields := map[string]string{"failure_class": "none", "op": "other", "origin": "foreground",
		"outcome": outcomeValue, "provider": MemoryProviderName(provider)}
	if outcomeValue != "success" {
		fields["failure_class"] = "unknown"
		if MemoryOpFailureClasses.Minus("none").Has(classValue) {
			fields["failure_class"] = classValue
		}
	}
	if MemoryOps.Has(op) {
		fields["op"] = op
	}
	if MemoryOpOrigins.Has(originValue) {
		fields["origin"] = originValue
	}
	return fields
}

// Emits one memory row per operation up to the batch cap
func recordMemoryOps(ctx context.Context, env *tools.Env, ops []string, provider any, outcome, failureClass string) {
	origin := "foreground"
	func() {
		defer func() { _ = recover() }()
		origin = memoryOrigin(env)
	}()
	if len(ops) > maxBatchOps {
		ops = ops[:maxBatchOps]
	}
	for _, op := range ops {
		emitLoop(ctx, MemoryOpMark, "", func() map[string]string {
			return MemoryOpFields(op, provider, outcome, origin, failureClass)
		})
	}
}

// Operations of one builtin memory call, each batch entry action or the single action
func BuiltinMemoryOps(action, operations any) []string {
	if list, ok := operations.([]any); ok && len(list) > 0 {
		ops := make([]string, 0, len(list))
		for _, op := range list {
			var value any
			if d, ok := asDict(op); ok {
				value = d["action"]
			}
			ops = append(ops, MemoryOp(value))
		}
		return ops
	}
	return []string{MemoryOp(action)}
}

// One row per operation of a builtin memory tool call, failure class none on success
func RecordBuiltinMemoryCall(ctx context.Context, env *tools.Env, action, operations any, outcome, failureClass string) {
	recordMemoryOps(ctx, env, BuiltinMemoryOps(action, operations), "builtin", outcome, failureClass)
}

// Reports whether a provider tool result text opens with an error or a false success
func providerResultFailed(result any) bool {
	s, ok := result.(string)
	if !ok {
		return false
	}
	head := pystr.Lstrip(s)
	if r := pystr.Runes(head); len(r) > 200 {
		head = pystr.FromRunes(r[:200])
	}
	return strings.HasPrefix(head, `{"error"`) || strings.Contains(head, `"success": false`)
}

// One row per memory provider tool call, raised or an error result counting it failed
func RecordProviderMemoryCall(ctx context.Context, env *tools.Env, provider, toolName, args, result any, raised bool) {
	failed := ""
	if raised {
		failed = "exception"
	} else if providerResultFailed(result) {
		failed = "provider_error"
	}
	outcome, class := "success", "none"
	if failed != "" {
		outcome, class = "failed", failed
	}
	recordMemoryOps(ctx, env, []string{MemoryProviderOp(toolName, args)}, provider, outcome, class)
}

var curatorCountKeys = []string{"archived", "created", "merged", "patched"}

// Bounded dimensions of one curator pass, counts resolved only while collection is on
func CuratorRunFields(trigger, outcome any, counts func() map[string]int) map[string]string {
	resolved := map[string]int{}
	if counts != nil {
		if got := counts(); got != nil {
			resolved = got
		}
	}
	fields := map[string]string{}
	for _, key := range curatorCountKeys {
		fields[key+"_bucket"] = CountBucket(resolved[key])
	}
	fields["outcome"] = "failed"
	if o := norm(outcome); CuratorOutcomes.Has(o) {
		fields["outcome"] = o
	}
	fields["trigger"] = "manual"
	if norm(trigger) == "scheduled" {
		fields["trigger"] = "scheduled"
	}
	return fields
}

// One row per curator pass recorded in the profile home the curator captured
func RecordCuratorRun(ctx context.Context, trigger, outcome string, counts func() map[string]int, homeDir string) {
	emitLoop(ctx, CuratorRunMark, homeDir, func() map[string]string { return CuratorRunFields(trigger, outcome, counts) })
}

var (
	delegationSuccessStatuses   = Set("completed")
	delegationCancelledStatuses = Set("interrupted", "cancelled")
)

// Open delegations kept at most, the oldest dropped when units never report back
const maxOpenDelegations = 256

// DelegationRun is one open delegate_task call folding its units
type DelegationRun struct {
	Expected   int
	Depth      int
	HomeDir    string
	Statuses   []any
	Background bool
}

var (
	delegationsMu    sync.Mutex
	delegations      = map[any]*DelegationRun{}
	delegationsOrder []any
)

// Depth label of a delegation, 1 to 3 or gte_4
func DelegationDepth(depth any) string {
	value, ok := plainInt(depth)
	if !ok {
		value = 1
	}
	if value >= 4 {
		return "gte_4"
	}
	return strconv.Itoa(max(1, value))
}

// Outcome of a delegation from its unit statuses
func DelegationOutcome(statuses []any) string {
	succeeded, cancelled := 0, false
	for _, s := range statuses {
		v := norm(s)
		if delegationSuccessStatuses.Has(v) {
			succeeded++
		}
		if delegationCancelledStatuses.Has(v) {
			cancelled = true
		}
	}
	switch {
	case len(statuses) > 0 && succeeded == len(statuses):
		return "success"
	case succeeded > 0:
		return "partial"
	case cancelled:
		return "cancelled"
	}
	return "failed"
}

// Bounded dimensions of one finished delegate_task call
func DelegationRunFields(statuses []any, depth, background, subagents any) map[string]string {
	mode := "foreground"
	if background == true {
		mode = "background"
	}
	count, ok := plainInt(subagents)
	if b, isBool := subagents.(bool); isBool && b {
		count, ok = 1, true
	}
	if !ok {
		count = 0
	}
	return map[string]string{"depth": DelegationDepth(depth), "mode": mode, "outcome": DelegationOutcome(statuses),
		"subagent_count_bucket": CountBucket(count)}
}

// Opens one delegate_task call keyed by the value every unit of the call shares
func BeginDelegationRun(ctx context.Context, callKey any, subagents, depth int) {
	if subagents <= 0 {
		return
	}
	run := &DelegationRun{Expected: subagents, Depth: depth, HomeDir: currentHome(ctx)}
	delegationsMu.Lock()
	defer delegationsMu.Unlock()
	if _, open := delegations[callKey]; open {
		delegations[callKey] = run
		return
	}
	for len(delegations) >= maxOpenDelegations {
		oldest := delegationsOrder[0]
		delegationsOrder = delegationsOrder[1:]
		delete(delegations, oldest)
	}
	delegations[callKey] = run
	delegationsOrder = append(delegationsOrder, callKey)
}

// Folds one joined unit into its call and emits the call row once its last unit lands
func FinishDelegationUnit(ctx context.Context, callKey any, results []any, background bool) {
	statuses := make([]any, 0, len(results))
	for _, r := range results {
		var status any
		if d, ok := asDict(r); ok {
			status = d["status"]
		}
		statuses = append(statuses, status)
	}
	delegationsMu.Lock()
	run, ok := delegations[callKey]
	if !ok {
		delegationsMu.Unlock()
		return
	}
	run.Statuses = append(run.Statuses, statuses...)
	run.Background = run.Background || background
	if len(run.Statuses) < run.Expected {
		delegationsMu.Unlock()
		return
	}
	delete(delegations, callKey)
	for i, k := range delegationsOrder {
		if k == callKey {
			delegationsOrder = append(delegationsOrder[:i:i], delegationsOrder[i+1:]...)
			break
		}
	}
	delegationsMu.Unlock()
	emitLoop(ctx, DelegationRunMark, run.HomeDir, func() map[string]string {
		return DelegationRunFields(run.Statuses, run.Depth, run.Background, run.Expected)
	})
}

var statusErrorClasses = map[string]string{"timeout": "timeout", "timed_out": "timeout",
	"interrupted": "interrupted", "cancelled": "interrupted", "blocked": "blocked"}

// Terminal failure class, exit code 124 being the foreground deadline and an error field a tool error
func terminalFailure(data map[string]any) (string, bool) {
	if pyrepr.Equal(data["exit_code"], 124) {
		return "timeout", true
	}
	if pyrepr.Truthy(data["error"]) {
		return "tool_error", true
	}
	return "", false
}

// Code execution failure class, a user traceback with output still counting as a backend success
func codeFailure(data map[string]any) (string, bool) {
	status := norm(data["status"])
	if class, ok := statusErrorClasses[status]; ok {
		return class, true
	}
	if status == "error" && pyrepr.Truthy(data["error"]) && !pyrepr.Truthy(data["output"]) {
		return "tool_error", true
	}
	return "", false
}

// Browser failure class from a false success or an error field
func browserFailure(data map[string]any) (string, bool) {
	if data["success"] == false || pyrepr.Truthy(data["error"]) {
		return "tool_error", true
	}
	return "", false
}

var failureClassifiers = map[string]func(map[string]any) (string, bool){
	"browser": browserFailure, "code": codeFailure, "terminal": terminalFailure}

// Bounded dimensions of one backend call, nil for an unknown kind, a nil error class classifying the result
func ExecutionBackendFields(kind, backend, result, errorClass any) map[string]string {
	kindValue := norm(kind)
	backends, ok := ExecutionBackendsByKind[kindValue]
	if !ok {
		return nil
	}
	if resolve, ok := backend.(func() string); ok {
		backend = resolve()
	}
	backendValue := norm(backend)
	var failure string
	failed := errorClass != nil
	if failed {
		failure, _ = errorClass.(string)
		if s, ok := errorClass.(fmt.Stringer); ok && failure == "" {
			failure = s.String()
		}
	} else {
		failure, failed = failureClassifiers[kindValue](parsedResult(result))
	}
	fields := map[string]string{"backend": "other", "error_class": "none", "kind": kindValue, "outcome": "success"}
	if backends.Has(backendValue) {
		fields["backend"] = backendValue
	}
	if failed {
		fields["outcome"] = "failed"
		fields["error_class"] = "unknown"
		if ToolErrorClasses.Has(failure) {
			fields["error_class"] = failure
		}
	}
	return fields
}

type unmeteredKey struct{}

// Context whose terminal, browser and code calls are icebath work, not counted as backend use
func UnmeteredBackendCalls(ctx context.Context) context.Context {
	return context.WithValue(ctx, unmeteredKey{}, true)
}

// Counts one tool call that reached its backend and returns the result unchanged
func RecordExecutionBackend(ctx context.Context, env *tools.Env, kind string, backend, result, errorClass any) any {
	if unmetered, _ := ctx.Value(unmeteredKey{}).(bool); unmetered || isBackgroundReviewEnv(env) {
		return result
	}
	emitLoop(ctx, ExecutionBackendMark, "", func() map[string]string {
		return ExecutionBackendFields(kind, backend, result, errorClass)
	})
	return result
}

// Counts a terminal call that got past planning, a nil plan meaning planning failed
func RecordTerminalBackend(ctx context.Context, env *tools.Env, plan, result, errorClass any) any {
	if plan == nil {
		return result
	}
	var envType any
	switch p := plan.(type) {
	case interface{ EnvType() string }:
		envType = p.EnvType()
	case map[string]any:
		envType = p["env_type"]
	}
	return RecordExecutionBackend(ctx, env, "terminal", envType, result, errorClass)
}

// Runs one browser call and counts it against the backend that served it, resolved after the call
func RecordBrowserCall(ctx context.Context, env *tools.Env, call func() (any, error), resolveBackend func() (string, error)) (any, error) {
	result, err := call()
	if err != nil {
		safeRecordBrowser(ctx, env, resolveBackend, nil, "exception")
		return result, err
	}
	safeRecordBrowser(ctx, env, resolveBackend, result, nil)
	return result, nil
}

// Records a browser call with a backend resolver that reads other when it fails
func safeRecordBrowser(ctx context.Context, env *tools.Env, backend func() (string, error), result, errorClass any) {
	name := func() (out string) {
		defer func() {
			if r := recover(); r != nil {
				out = "other"
			}
		}()
		got, err := backend()
		if err != nil {
			return "other"
		}
		return got
	}
	RecordExecutionBackend(ctx, env, "browser", name, result, errorClass)
}
