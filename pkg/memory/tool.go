package memory

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/settings"

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
func ManagerFor(ctx context.Context, env *tools.Env) Manager {
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load(ManagerValuesKey); ok {
			if m, ok := v.(Manager); ok {
				return m
			}
		}
	}
	m := NewStoreManager(sessionStore(tools.WithEnvHome(ctx, env), env))
	if env != nil && env.Values != nil {
		if actual, _ := env.Values.LoadOrStore(ManagerValuesKey, m); actual != nil {
			if am, ok := actual.(Manager); ok {
				return am
			}
		}
	}
	return m
}

// Built in store the agent creates at init from the home's settings, nil when both stores are disabled
func sessionStore(ctx context.Context, env *tools.Env) *Store {
	sctx := home.WithOverride(ctx, envHome(env))
	if memoryEnabled, userEnabled := BuiltinMemoryStoreFlags(sctx); !memoryEnabled && !userEnabled {
		return nil
	}
	store := configuredStore(sctx, env)
	if err := store.LoadFromDisk(tools.WithEnvHome(ctx, env)); err != nil {
		store.log().Debug("Memory store load failed at session start", "error", err)
	}
	return store
}

// Store with the limits and flags of the home in sctx
func configuredStore(sctx context.Context, env *tools.Env) *Store {
	memoryEnabled, userEnabled := BuiltinMemoryStoreFlags(sctx)
	store := NewStore(home.MemoriesDir(envHome(env)), settings.MemoryMemoryCharLimit.Get(sctx), settings.MemoryUserCharLimit.Get(sctx), memoryEnabled, userEnabled)
	store.Log = envLog(env)
	return store
}

// Fresh on disk store with the home's limits and flags for contexts with no live agent
func LoadOnDiskStore(ctx context.Context, env *tools.Env) (*Store, error) {
	store := configuredStore(home.WithOverride(ctx, envHome(env)), env)
	if err := store.LoadFromDisk(tools.WithEnvHome(ctx, env)); err != nil {
		return nil, err
	}
	return store, nil
}

// Memory and user profile store flags of the home in ctx
func BuiltinMemoryStoreFlags(ctx context.Context) (bool, bool) {
	return settings.Memory.Get(ctx), settings.LearnAboutMe.Get(ctx)
}

// Store flags of the environment's home as they are now
func liveStoreFlags(env *tools.Env) (bool, bool) {
	return BuiltinMemoryStoreFlags(home.WithOverride(context.Background(), envHome(env)))
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
func memoryParameters() *pyjson.Object {
	return pyjson.ObjectOf(
		"type", "object",
		"properties", pyjson.ObjectOf(
			"target", pyjson.ObjectOf(
				"type", "string",
				"enum", []any{"memory", "user"},
				"description", "Which memory store: 'memory' for personal notes, 'user' for user profile.",
			),
			"operations", pyjson.ObjectOf(
				"type", "array",
				"description", "Changes applied atomically in one call against the final char budget; "+
					"a single change is a one-item list.",
				"items", pyjson.ObjectOf(
					"type", "object",
					"properties", pyjson.ObjectOf(
						"action", pyjson.ObjectOf("type", "string", "enum", []any{"add", "patch", "remove"}),
						"content", pyjson.ObjectOf(
							"type", "string",
							"description", "Required for 'add' and 'patch'. For 'add', the new entry. For 'patch', "+
								"the text that replaces old_text; the rest of the entry is kept.",
						),
						"old_text", pyjson.ObjectOf(
							"type", "string",
							"description", "REQUIRED for 'patch' and 'remove': text from one entry. 'patch' replaces "+
								"exactly this span (it must occur once in the entry); 'remove' deletes the "+
								"whole entry. Omit for 'add'.",
						),
					),
					"required", []any{"action"},
					"additionalProperties", false,
				),
			),
		),
		"required", []any{"target", "operations"},
	)
}

// Full OpenAI function schema of the memory tool
func MemorySchema() *pyjson.Object {
	return pyjson.ObjectOf("name", "memory", "description", memoryDescription, "parameters", memoryParameters())
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
func memorySchemaOverrides(env *tools.Env) *pyjson.Object {
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
	targetSchema := parameters.Vals["properties"].(*pyjson.Object).Vals["target"].(*pyjson.Object)
	targetSchema.Set("enum", targets)
	description := memoryDescription
	if len(names) == 1 {
		narrowed := singleTargetText[names[0]]
		targetSchema.Set("description", narrowed[0])
		description = strings.ReplaceAll(description, targetsText, narrowed[1])
	}
	return pyjson.ObjectOf("description", description, "parameters", parameters)
}

// Error envelope with the bounded message and extra fields
func toolError(message string, extra map[string]any) string {
	data := tools.Dict("error", boundErrorText(message))
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		data.Set(k, extra[k])
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
	r := failure(fmt.Sprintf("Built-in %s writes are disabled in memory config.", label), "disabled", pyjson.ObjectOf("target", name))
	return &r
}

// Memory tool entry point returning the JSON result, single op or an atomic operations batch, a crash returned as the error it raised
func MemoryTool(ctx context.Context, env *tools.Env, store *Store, action, target, content, oldText, newText, operations any) (string, error) {
	if store == nil {
		return toolError("Memory is not available. It may be disabled in config or this environment.", map[string]any{"success": false}), nil
	}
	return runMemoryTool(ctx, env, store, action, target, content, oldText, newText, operations)
}

// Result JSON of one memory call, a single op or a batch
func runMemoryTool(ctx context.Context, env *tools.Env, store *Store, action, target, content, oldText, newText, operations any) (string, error) {
	if !pyTruthy(content) && pyTruthy(newText) {
		content = newText
	}
	if targetErr := memoryTargetError(store, target, operations); targetErr != nil {
		return tools.JSONASCII(targetErr.Data), nil
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
			return "", err
		}
		if denied, err := backgroundDeleteGate(tools.WithEnvHome(ctx, env), env, store, nil, list, name, nil, nil); err != nil || denied != "" {
			return denied, err
		}
		if gated, err := applyWriteGate(ctx, env, store, "batch", name, nil, nil, list); err != nil || gated != "" {
			return gated, err
		}
		res, err := store.ApplyBatch(tools.WithEnvHome(ctx, env), name, ops)
		if err != nil {
			return "", err
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
		return "", err
	}
	verb, ok := action.(string)
	if !ok || (verb != "add" && verb != "patch" && verb != "remove") {
		return invalid(fmt.Sprintf("Unknown action '%s'. Use: add, patch, remove", pyStr(action)))
	}
	if msg := validateSingleOp(store, verb, name, content, oldText); msg != "" {
		return msg, nil
	}
	if denied, err := backgroundDeleteGate(tools.WithEnvHome(ctx, env), env, store, verb, nil, name, content, oldText); err != nil || denied != "" {
		return denied, err
	}
	if gated, err := applyWriteGate(ctx, env, store, verb, name, content, oldText, nil); err != nil || gated != "" {
		return gated, err
	}
	res, err := storeCall(tools.WithEnvHome(ctx, env), store, verb, name, content, oldText, nil)
	if err != nil {
		return "", err
	}
	return applied(res)
}

// Message refusing the removed whole entry replace action
const replaceRemoved = "'replace' is not available; use 'patch'. old_text is the exact span to change and " +
	"content is its new text; the rest of the entry is kept. To rewrite a whole entry, " +
	"set old_text to the whole entry. Nothing was written."

// Result JSON of a store call
func applied(res Result) (string, error) {
	return tools.JSON(res.Data), nil
}

// Refusal JSON of an invalid call
func invalid(message string) (string, error) {
	return toolError(message, map[string]any{"success": false}), nil
}

// Rejects an invalid single op before the gate, a missing old_text returning the inventory to retry against
func validateSingleOp(store *Store, action, target string, content, oldText any) string {
	if action == "add" && !pyTruthy(content) {
		return toolError("Content is required for 'add' action.", map[string]any{"success": false})
	}
	if (action == "remove" || action == "patch") && !pyTruthy(oldText) {
		return tools.JSON(tools.Dict(
			"success", false,
			"error", fmt.Sprintf("'%s' needs old_text -- a short unique substring of the entry "+
				"to %s. None was provided. Reissue the %s with old_text "+
				"set to part of one of the current_entries below.", action, action, action),
			"current_entries", store.Entries(target), "usage", store.Usage(target)))
	}
	if action == "patch" && !pyTruthy(content) {
		return toolError("content (the new text for the old_text span) is required for 'patch' action. "+
			PatchDropHint, map[string]any{"success": false})
	}
	return ""
}

// Runs one store action, the staged replay passing the pinned entry it was reviewed against
func storeCall(ctx context.Context, store *Store, action, target string, content, oldText, matchedEntry any) (Result, error) {
	switch action {
	case "add":
		text, err := strMethodArg(content)
		if err != nil {
			return Result{}, err
		}
		return store.Add(ctx, target, text)
	case "replace":
		text, err := strMethodArg(content)
		if err != nil {
			return Result{}, err
		}
		old, err := strMethodArg(oldText)
		if err != nil {
			return Result{}, err
		}
		return store.Replace(ctx, target, old, text, matchedEntry)
	case "remove":
		old, err := strMethodArg(oldText)
		if err != nil {
			return Result{}, err
		}
		return store.Remove(ctx, target, old, matchedEntry)
	}
	old, err := strMethodArg(oldText)
	if err != nil {
		return Result{}, err
	}
	text := ""
	if content != nil {
		text = pyStr(content)
	}
	return store.Patch(ctx, target, old, text, matchedEntry)
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
				return nil, &pyError{class: "AttributeError", msg: fmt.Sprintf("memory operation is %s where a mapping was expected", errchain.Kind(op))}
			}
			out[i] = m
		}
		return out, nil
	case string, map[string]any:
		return nil, &pyError{class: "AttributeError", msg: fmt.Sprintf("memory operations are %s where a list was expected", errchain.Kind(v))}
	}
	return nil, &pyError{class: "TypeError", msg: fmt.Sprintf("memory operations are %s where a list was expected", errchain.Kind(v))}
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
	manager := ManagerFor(tools.WithEnvHome(ctx, call.Env), call.Env)
	result, err := MemoryTool(ctx, call.Env, manager.Store(), action, args["target"], args["content"], args["old_text"], args["new_text"], args["operations"])
	if err != nil {
		panic(err)
	}
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
// Logger of the tool env or the process default, named after this package
func envLog(env *tools.Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log.With("logger", logName)
	}
	return pkgLog()
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
