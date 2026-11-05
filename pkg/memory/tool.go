package memory

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Values key holding the per session Manager
const ManagerValuesKey = "memory"

// Values key marking a background review the user explicitly asked for, true means attended
const ReviewAttendedValuesKey = "review_attended"

// Values key carrying the store flags from the availability check to the schema narrowing
const surfaceFlagsValuesKey = "memory.surface_flags"

// Origin value the review and curator forks tag their writes with
const backgroundReviewOrigin = "background_review"

// Bound on an error body destined for model context, in code points
const maxToolErrorChars = 2048

const toolErrorTruncationMarker = "… [truncated]"

// Manager is the per session memory surface the memory tool writes through and mirrors to
type Manager interface {
	Store() *Store
	NotifyMemoryToolWrite(result string, args map[string]any, taskID, toolCallID string)
}

// storeManager serves a session whose only memory is the built in store
type storeManager struct {
	store *Store
}

// NewStoreManager wraps a built in store as a Manager without external providers
func NewStoreManager(store *Store) Manager {
	return &storeManager{store: store}
}

func (m *storeManager) Store() *Store { return m.store }

// External providers receive mirrored writes, and a store only session has none to receive them
func (m *storeManager) NotifyMemoryToolWrite(result string, args map[string]any, taskID, toolCallID string) {
}

// Session Manager from the environment, registering a store only manager when the host registered none
func ManagerFor(env *tools.Env) Manager {
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load(ManagerValuesKey); ok {
			if m, ok := v.(Manager); ok {
				return m
			}
		}
	}
	m := NewStoreManager(sessionStore(env))
	if env != nil && env.Values != nil {
		if actual, _ := env.Values.LoadOrStore(ManagerValuesKey, m); actual != nil {
			if am, ok := actual.(Manager); ok {
				return am
			}
		}
	}
	return m
}

// Built in store the agent creates at init from the session config, nil when both stores are disabled
func sessionStore(env *tools.Env) *Store {
	var tree config.Tree
	if env != nil {
		tree = env.Config
	}
	section := builtinMemoryConfig(tree)
	memoryEnabled, userEnabled := builtinMemoryStoreFlags(section)
	if !memoryEnabled && !userEnabled {
		return nil
	}
	store, ok := storeFromSection(env, section)
	if !ok {
		store = NewStore(home.MemoriesDir(envHome(env)), DefaultMemoryCharLimit, DefaultUserCharLimit, memoryEnabled, userEnabled)
	}
	store.Log = envLog(env)
	if err := store.LoadFromDisk(); err != nil {
		store.log().Debug("Memory store load failed at session start", "error", err)
	}
	return store
}

// Store with the configured limits and flags, false when a limit is not an integer
func storeFromSection(env *tools.Env, section map[string]any) (*Store, bool) {
	memoryEnabled, userEnabled := builtinMemoryStoreFlags(section)
	memoryLimit, ok1 := pyInt(sectionValue(section, "memory_char_limit", DefaultMemoryCharLimit))
	userLimit, ok2 := pyInt(sectionValue(section, "user_char_limit", DefaultUserCharLimit))
	if !ok1 || !ok2 {
		envLog(env).Warn("Memory char limits in config are not integers, using defaults",
			"memory_char_limit", pyStr(section["memory_char_limit"]), "user_char_limit", pyStr(section["user_char_limit"]))
		return nil, false
	}
	return NewStore(home.MemoriesDir(envHome(env)), memoryLimit, userLimit, memoryEnabled, userEnabled), true
}

func sectionValue(section map[string]any, key string, fallback any) any {
	if v, ok := section[key]; ok {
		return v
	}
	return fallback
}

// Fresh on disk store with configured limits and flags for contexts with no live agent, defaults when config fails
func LoadOnDiskStore(env *tools.Env) (*Store, error) {
	var store *Store
	if tree, err := config.Load(envHome(env), configLookup(env)); err == nil {
		store, _ = storeFromSection(env, builtinMemoryConfig(tree))
	}
	if store == nil {
		store = NewStore(home.MemoriesDir(envHome(env)), DefaultMemoryCharLimit, DefaultUserCharLimit, true, true)
	}
	store.Log = envLog(env)
	if err := store.LoadFromDisk(); err != nil {
		return nil, err
	}
	return store, nil
}

// Normalized memory config section, empty when missing or malformed so flags default to enabled
func builtinMemoryConfig(tree config.Tree) map[string]any {
	if tree == nil {
		return map[string]any{}
	}
	if section, ok := tree.Get("memory").(map[string]any); ok {
		return section
	}
	return map[string]any{}
}

// Memory and user profile store flags, each enabled unless set to a falsy value
func builtinMemoryStoreFlags(section map[string]any) (bool, bool) {
	return isTruthyValue(section["memory_enabled"], true), isTruthyValue(section["user_profile_enabled"], true)
}

// Store flags from the live config, both enabled when the config cannot load
func liveStoreFlags(env *tools.Env) (bool, bool) {
	tree, err := config.Load(envHome(env), configLookup(env))
	if err != nil {
		envLog(env).Debug("Could not read memory config for availability", "error", err)
		return builtinMemoryStoreFlags(map[string]any{})
	}
	return builtinMemoryStoreFlags(builtinMemoryConfig(tree))
}

// Shared truthy coercion, None takes the default and strings match the truthy set
func isTruthyValue(v any, def bool) bool {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		switch strings.ToLower(pyStrip(s)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyTruthy(v)
}

// Python int of a config value, false when the value cannot convert
func pyInt(v any) (int, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case int:
		return x, true
	case int64:
		return int(x), true
	case uint64:
		return int(x), true
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return 0, false
		}
		return int(math.Trunc(x)), true
	case string:
		s := pyStrip(x)
		if !pyIntLiteral.MatchString(s) {
			return 0, false
		}
		n, err := strconv.Atoi(strings.ReplaceAll(s, "_", ""))
		return n, err == nil
	}
	return 0, false
}

// Decimal literal int accepts, sign and single underscores between digits allowed
var pyIntLiteral = regexp.MustCompile(`^[+-]?[0-9]+(?:_[0-9]+)*$`)

// Snapshots the store flags for the following schema narrowing and reports whether either store is on
func checkMemoryRequirements(env *tools.Env) bool {
	if env != nil && env.Values != nil {
		env.Values.Delete(surfaceFlagsValuesKey)
	}
	memoryEnabled, userEnabled := liveStoreFlags(env)
	if env != nil && env.Values != nil {
		env.Values.Store(surfaceFlagsValuesKey, [2]bool{memoryEnabled, userEnabled})
	}
	return memoryEnabled || userEnabled
}

const memoryDescription = "Save durable facts to persistent memory that survive across sessions. Memory is " +
	"injected into every future turn, so keep entries compact and high-signal.\n\n" +
	"HOW: make ALL your changes to one target in ONE call via an 'operations' array (each " +
	"item: {action, content?, old_text?}). The batch applies atomically and the char limit is " +
	"checked only on the FINAL result — so a single call can remove/patch stale entries " +
	"to free room AND add new ones, even when an add alone would overflow. The response " +
	"reports current/limit chars and confirms completion; one batch call finishes the " +
	"update, so don't repeat it.\n\n" +
	"WHEN: only for facts that apply to EVERY session regardless of task: who the user " +
	"is, stable environment facts, standing conventions with no task home. Anything " +
	"learned while doing a task (procedures, pitfalls, and the user's preferences and " +
	"corrections for that kind of work) belongs in the task's skill via skill_manage, " +
	"where it loads only when relevant; memory is injected into every turn and must " +
	"stay small.\n\n" +
	"IF FULL: an add is rejected with the current entries shown. Reissue as ONE batch that " +
	"removes or shortens enough stale entries and adds the new one together.\n\n" +
	targetsText + "\n\n" +
	"SKIP: trivial/obvious info, easily re-discovered facts, raw data dumps, task progress, " +
	"completed-work logs, temporary TODO state (use session_search for those). Reusable " +
	"procedures belong in a skill, not memory."

const targetsText = "TARGETS: 'user' = who the user is (name, role, preferences, style). 'memory' = your " +
	"notes (environment, conventions, tool quirks, lessons)."

// Parameters advertised once inside operations, the handler still accepting the legacy single op shape
func memoryParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"enum":        []any{"memory", "user"},
				"description": "Which memory store: 'memory' for personal notes, 'user' for user profile.",
			},
			"operations": map[string]any{
				"type": "array",
				"description": "Changes applied atomically in one call against the final char budget; " +
					"a single change is a one-item list.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"action": map[string]any{"type": "string", "enum": []any{"add", "patch", "remove"}},
						"content": map[string]any{
							"type": "string",
							"description": "Required for 'add' and 'patch'. For 'add', the new entry. For 'patch', " +
								"the text that replaces old_text; the rest of the entry is kept.",
						},
						"old_text": map[string]any{
							"type": "string",
							"description": "REQUIRED for 'patch' and 'remove': text from one entry. 'patch' replaces " +
								"exactly this span (it must occur once in the entry); 'remove' deletes the " +
								"whole entry. Omit for 'add'.",
						},
					},
					"required":             []any{"action"},
					"additionalProperties": false,
				},
			},
		},
		"required": []any{"target", "operations"},
	}
}

// Full OpenAI function schema of the memory tool
func MemorySchema() map[string]any {
	return map[string]any{"name": "memory", "description": memoryDescription, "parameters": memoryParameters()}
}

// Target description and TARGETS replacement when exactly one built in store is enabled
var singleTargetText = map[string][2]string{
	"memory": {"The enabled built-in store: 'memory' for personal notes.",
		"TARGET: only 'memory' is enabled for personal notes (environment, conventions, " +
			"tool quirks, lessons)."},
	"user": {"The enabled built-in store: 'user' for user profile.",
		"TARGET: only 'user' is enabled for user profile facts (name, role, preferences, style)."},
}

// Narrows the advertised targets to the enabled stores using the availability snapshot
func memorySchemaOverrides(env *tools.Env) map[string]any {
	var flags [2]bool
	snapshot := false
	if env != nil && env.Values != nil {
		if v, ok := env.Values.LoadAndDelete(surfaceFlagsValuesKey); ok {
			flags, snapshot = v.([2]bool)
		}
	}
	if !snapshot {
		flags[0], flags[1] = liveStoreFlags(env)
	}
	targets := []any{}
	var names []string
	for i, t := range []string{"memory", "user"} {
		if flags[i] {
			targets = append(targets, t)
			names = append(names, t)
		}
	}
	parameters := memoryParameters()
	targetSchema := parameters["properties"].(map[string]any)["target"].(map[string]any)
	targetSchema["enum"] = targets
	description := memoryDescription
	if len(names) == 1 {
		narrowed := singleTargetText[names[0]]
		targetSchema["description"] = narrowed[0]
		description = strings.ReplaceAll(description, targetsText, narrowed[1])
	}
	return map[string]any{"description": description, "parameters": parameters}
}

// Error envelope with the bounded message and extra fields
func toolError(message string, extra map[string]any) string {
	data := map[string]any{"error": boundErrorText(message)}
	for k, v := range extra {
		data[k] = v
	}
	return tools.JSON(data)
}

// Error body bounded in code points so a raw exception cannot bloat history across retries
func boundErrorText(text string) string {
	if runeLen(text) <= maxToolErrorChars {
		return text
	}
	return runePrefix(text, maxToolErrorChars) + toolErrorTruncationMarker
}

// Validates one explicit target and nested op targets before writing, staging or replaying a batch
func memoryTargetError(store *Store, target any, operations any) *Result {
	if target == nil {
		r := failure("Missing required target. Retry with target='memory' or target='user' at the top level. "+
			"For a batch, remove target from every operation; use separate calls for different stores. "+
			"Nothing was written or staged.", "invalid_args", nil)
		return &r
	}
	name, ok := target.(string)
	if !ok || (name != "memory" && name != "user") {
		r := failure(boundErrorText(fmt.Sprintf("Invalid memory target '%s'. Use 'memory' or 'user'.", pyStr(target))), "invalid_args", nil)
		return &r
	}
	var nested []any
	switch ops := operations.(type) {
	case []any:
		nested = ops
	case []map[string]any:
		for _, op := range ops {
			nested = append(nested, op)
		}
	}
	for index, op := range nested {
		m, isMap := op.(map[string]any)
		if !isMap {
			continue
		}
		if t, present := m["target"]; present && t != nil && t != name {
			r := failure(fmt.Sprintf("Operation %d names a different target than the one at the top level. "+
				"One call writes one store: retry with separate calls for different stores. "+
				"Nothing was written or staged.", index+1), "invalid_args", nil)
			return &r
		}
	}
	if store.TargetEnabled(name) {
		return nil
	}
	label := "MEMORY.md"
	if name == "user" {
		label = "USER.md"
	}
	r := failure(fmt.Sprintf("Built-in %s writes are disabled in memory config.", label), "disabled", map[string]any{"target": name})
	return &r
}

// Memory tool entry point returning the JSON result, single op or an atomic operations batch
func MemoryTool(ctx context.Context, env *tools.Env, store *Store, action, target, content, oldText, newText, operations any) string {
	if store == nil {
		return toolError("Memory is not available. It may be disabled in config or this environment.", map[string]any{"success": false})
	}
	outcome, result, class, err := runMemoryTool(ctx, env, store, action, target, content, oldText, newText, operations)
	if err != nil {
		envLog(env).Error("memory tool raised", "error", err)
		return "Error executing tool 'memory': " + osErrorText(err)
	}
	failureClass := "none"
	if outcome != "success" {
		failureClass = class
	}
	recordBuiltinMemoryCall(env, action, operations, outcome, failureClass)
	return result
}

// Outcome as success, failed or rejected, the result JSON and the failure class
func runMemoryTool(ctx context.Context, env *tools.Env, store *Store, action, target, content, oldText, newText, operations any) (string, string, string, error) {
	if !pyTruthy(content) && pyTruthy(newText) {
		content = newText
	}
	if targetErr := memoryTargetError(store, target, operations); targetErr != nil {
		return "rejected", tools.JSON(targetErr.Data), targetErr.FailureClass, nil
	}
	name := target.(string)
	if pyTruthy(operations) {
		list, ok := operations.([]any)
		if !ok {
			return invalid("operations must be a list of {action, content?, old_text?} objects.")
		}
		for _, op := range list {
			if m, ok := op.(map[string]any); ok && m["action"] == "replace" {
				return invalid(replaceRemoved)
			}
		}
		ops, err := opMaps(list)
		if err != nil {
			return "", "", "", err
		}
		if denied, class, err := backgroundDeleteGate(env, store, nil, list, name, nil, nil); err != nil || denied != "" {
			return "rejected", denied, class, err
		}
		if gated, class, err := applyWriteGate(ctx, env, store, "batch", name, nil, nil, list); err != nil || gated != "" {
			return "rejected", gated, class, err
		}
		res, err := store.ApplyBatch(name, ops)
		if err != nil {
			return "", "", "", err
		}
		return applied(res)
	}
	if !pyTruthy(action) {
		return invalid("Missing required parameter: 'operations', a list of {action: add/patch/remove, " +
			"content?, old_text?} changes; a single change is a one-item list.")
	}
	if action == "replace" {
		return invalid(replaceRemoved)
	}
	if err := hashable(action); err != nil {
		return "", "", "", err
	}
	verb, ok := action.(string)
	if !ok || (verb != "add" && verb != "patch" && verb != "remove") {
		return invalid(fmt.Sprintf("Unknown action '%s'. Use: add, patch, remove", pyStr(action)))
	}
	if msg, class := validateSingleOp(store, verb, name, content, oldText); msg != "" {
		return "rejected", msg, class, nil
	}
	if denied, class, err := backgroundDeleteGate(env, store, verb, nil, name, content, oldText); err != nil || denied != "" {
		return "rejected", denied, class, err
	}
	if gated, class, err := applyWriteGate(ctx, env, store, verb, name, content, oldText, nil); err != nil || gated != "" {
		return "rejected", gated, class, err
	}
	res, err := storeCall(store, verb, name, content, oldText, nil)
	if err != nil {
		return "", "", "", err
	}
	return applied(res)
}

// Message refusing the removed whole entry replace action
const replaceRemoved = "'replace' is not available; use 'patch'. old_text is the exact span to change and " +
	"content is its new text; the rest of the entry is kept. To rewrite a whole entry, " +
	"set old_text to the whole entry. Nothing was written."

func applied(res Result) (string, string, string, error) {
	outcome := "failed"
	if res.Success() {
		outcome = "success"
	}
	return outcome, tools.JSON(res.Data), res.FailureClass, nil
}

func invalid(message string) (string, string, string, error) {
	return "rejected", toolError(message, map[string]any{"success": false}), "invalid_args", nil
}

// Rejects an invalid single op before the gate, a missing old_text returning the inventory to retry against
func validateSingleOp(store *Store, action, target string, content, oldText any) (string, string) {
	if action == "add" && !pyTruthy(content) {
		return toolError("Content is required for 'add' action.", map[string]any{"success": false}), "missing_content"
	}
	if (action == "remove" || action == "patch") && !pyTruthy(oldText) {
		return tools.JSON(map[string]any{
			"success": false,
			"error": fmt.Sprintf("'%s' needs old_text -- a short unique substring of the entry "+
				"to %s. None was provided. Reissue the %s with old_text "+
				"set to part of one of the current_entries below.", action, action, action),
			"current_entries": store.Entries(target), "usage": store.Usage(target)}), "missing_old_text"
	}
	if action == "patch" && !pyTruthy(content) {
		return toolError("content (the new text for the old_text span) is required for 'patch' action. "+
			PatchDropHint, map[string]any{"success": false}), "missing_content"
	}
	return "", ""
}

// Runs one store action, the staged replay passing the pinned entry it was reviewed against
func storeCall(store *Store, action, target string, content, oldText, matchedEntry any) (Result, error) {
	switch action {
	case "add":
		text, err := strMethodArg(content)
		if err != nil {
			return Result{}, err
		}
		return store.Add(target, text)
	case "replace":
		text, err := strMethodArg(content)
		if err != nil {
			return Result{}, err
		}
		old, err := strMethodArg(oldText)
		if err != nil {
			return Result{}, err
		}
		return store.Replace(target, old, text, matchedEntry)
	case "remove":
		old, err := strMethodArg(oldText)
		if err != nil {
			return Result{}, err
		}
		return store.Remove(target, old, matchedEntry)
	}
	old, err := strMethodArg(oldText)
	if err != nil {
		return Result{}, err
	}
	text := ""
	if content != nil {
		text = pyStr(content)
	}
	return store.Patch(target, old, text, matchedEntry)
}

// Gate summary and detail text of one single op
func gateText(action, label string, content, oldText any) (string, string) {
	switch action {
	case "add":
		return "add to " + label, pyStrOr(content, "")
	case "replace":
		return "replace in " + label, fmt.Sprintf("entry matching: %s\nwhole entry becomes: %s", pyStr(oldText), pyStr(content))
	case "remove":
		return "remove from " + label, pyStrOr(oldText, "")
	}
	return "patch in " + label, fmt.Sprintf("entry matching: %s\nspan '%s' becomes: %s (rest of the entry is kept)", pyStr(oldText), pyStr(oldText), pyStr(content))
}

// One display line of a batch op for gate prompts and staged summaries
func batchOpLine(op any) string {
	m, _ := op.(map[string]any)
	act, present := m["action"]
	if !present {
		act = "?"
	}
	content := m["content"]
	if !pyTruthy(content) {
		content = m["new_text"]
	}
	old, present := m["old_text"]
	if !present {
		old = ""
	}
	switch act {
	case "remove":
		return "- remove: " + pyStr(old)
	case "patch":
		return fmt.Sprintf("- patch span '%s' -> '%s' (rest of the entry is kept)", pyStr(old), pyStrOr(content, ""))
	}
	return fmt.Sprintf("- %s: %s", pyStr(act), pyStrOr(content, ""))
}

// Batch ops as maps with falsy items read as empty, a crash for any other non object item
func opMaps(v any) ([]map[string]any, error) {
	if !pyTruthy(v) {
		return []map[string]any{}, nil
	}
	switch x := v.(type) {
	case []map[string]any:
		return x, nil
	case []any:
		out := make([]map[string]any, len(x))
		for i, op := range x {
			if !pyTruthy(op) {
				out[i] = map[string]any{}
				continue
			}
			m, ok := op.(map[string]any)
			if !ok {
				return nil, &pyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(op))}
			}
			out[i] = m
		}
		return out, nil
	case string, map[string]any:
		return nil, &pyError{class: "AttributeError", msg: "'str' object has no attribute 'get'"}
	}
	return nil, &pyError{class: "TypeError", msg: fmt.Sprintf("'%s' object is not iterable", pyTypeName(v))}
}

// Shared metric operation buckets for built in and provider memory verbs
var memoryOpWords = func() map[string]string {
	out := map[string]string{}
	for bucket, words := range map[string][]string{
		"add":     {"add", "conclude", "create", "ingest", "remember", "retain", "save", "store", "upload"},
		"replace": {"edit", "replace", "update"},
		"remove":  {"delete", "forget", "remove"},
		"search":  {"probe", "query", "reason", "reasoning", "recall", "related", "search"},
		"read":    {"context", "get", "list", "profile", "read"},
	} {
		for _, w := range words {
			out[w] = bucket
		}
	}
	return out
}()

// Closed failure classes the memory op metric accepts
var memoryOpFailureClasses = map[string]bool{
	"ambiguous": true, "disabled": true, "drift": true, "exception": true, "gate_refused": true, "invalid_args": true,
	"missing_content": true, "missing_old_text": true, "no_match": true, "other": true, "over_budget": true,
	"provider_error": true, "read_failed": true, "retry_cap": true, "scan_blocked": true, "staged": true,
	"stale_entry": true, "unknown": true, "would_empty": true,
}

// Name of the shared metric mark one memory operation records
const memoryOpMetricName = "icebath.memory.op"

// One batch never exports more operation rows than this
const maxBatchOpRows = 25

func normMetricValue(v any) string {
	return strings.ToLower(pyStrip(pyStrOr(v, "")))
}

// Metric operation bucket for a verb, other when it names none
func memoryOp(v any) string {
	if bucket, ok := memoryOpWords[normMetricValue(v)]; ok {
		return bucket
	}
	return "other"
}

// One metric row per operation of a built in memory call, a batch counting each op
func recordBuiltinMemoryCall(env *tools.Env, action, operations any, outcome, failureClass string) {
	if env == nil || env.Config == nil || !env.Config.Bool("telemetry.shared_metrics.enabled", false) {
		return
	}
	var ops []string
	if list, ok := operations.([]any); ok && len(list) > 0 {
		for _, op := range list {
			m, _ := op.(map[string]any)
			ops = append(ops, memoryOp(m["action"]))
		}
	} else {
		ops = []string{memoryOp(action)}
	}
	origin := "foreground"
	if tools.CurrentOrigin(env) == backgroundReviewOrigin {
		origin = backgroundReviewOrigin
	}
	outcomeValue := normMetricValue(outcome)
	if outcomeValue != "failed" && outcomeValue != "rejected" && outcomeValue != "success" {
		outcomeValue = "failed"
	}
	classValue := "none"
	if outcomeValue != "success" {
		classValue = normMetricValue(failureClass)
		if !memoryOpFailureClasses[classValue] {
			classValue = "unknown"
		}
	}
	for i, op := range ops {
		if i == maxBatchOpRows {
			break
		}
		env.Emit(tools.Event{Kind: "metric", Tool: "memory", Data: map[string]any{
			"name": memoryOpMetricName, "failure_class": classValue, "op": op, "origin": origin,
			"outcome": outcomeValue, "provider": "builtin",
		}})
	}
}

// Registry handler running the tool against the session store and mirroring committed writes
func memoryHandler(ctx context.Context, call *tools.Call) string {
	args := call.Args
	if args == nil {
		args = map[string]any{}
	}
	action, present := args["action"]
	if !present {
		action = ""
	}
	manager := ManagerFor(call.Env)
	result := MemoryTool(ctx, call.Env, manager.Store(), action, args["target"], args["content"], args["old_text"], args["new_text"], args["operations"])
	manager.NotifyMemoryToolWrite(result, args, call.TaskID, call.ToolCallID)
	return result
}

// Session home, the process home when the environment names none
func envHome(env *tools.Env) string {
	if env != nil && pyStrip(env.Home) != "" {
		return env.Home
	}
	return home.Process()
}

// Secret lookup for config reference expansion
func configLookup(env *tools.Env) config.Lookup {
	if env != nil && env.Secrets != nil {
		return env.Secrets.Lookup()
	}
	return func(string) (string, bool) { return "", false }
}

// Session logger falling back to the process default
func envLog(env *tools.Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log
	}
	return slog.Default()
}

// Memory tool_store registers nothing at import
func registerMemoryStore() {}

// Registers the memory tool into the default registry
func registerMemoryTool() {
	tools.Register(tools.Def{
		Name:          "memory",
		Toolset:       "memory",
		Schema:        MemorySchema(),
		Handler:       memoryHandler,
		Check:         tools.NoCacheCheck(checkMemoryRequirements),
		Emoji:         "🧠",
		DynamicSchema: memorySchemaOverrides,
	})
}
