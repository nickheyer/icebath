package gateway

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Save, export, reload, delegation pause, rollback, context breakdown and history verbs of the shared owner
func tuiOpsHandlers(c *AuthorityConnection) map[string]controlHandler {
	bind := func(fn func(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error)) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return fn(ctx, c, ref, params)
		}
	}
	rollbackOf := func(kind string) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *pyjson.Object) (any, error) {
			return rollback(ctx, c, ref, params, kind)
		}
	}
	return map[string]controlHandler{"session.save": bind(saveSession), "session.export": bind(exportSession),
		"reload.env": bind(reloadEnv), "delegation.pause": bind(pauseDelegation),
		"rollback.list": rollbackOf("list"), "rollback.diff": rollbackOf("diff"), "rollback.restore": rollbackOf("restore"),
		"rollback.session_diff": rollbackOf("session_diff"), "session.context_breakdown": bind(contextBreakdown),
		"session.history": bind(sessionHistory)}
}

// Params each rollback verb accepts
var rollbackFields = map[string][]string{"list": {"session_id", "profile"}, "diff": {"session_id", "profile", "hash"},
	"restore":      {"session_id", "profile", "hash", "file_path", "all"},
	"session_diff": {"session_id", "profile"}}

// Wire bytes the session diff stat and body may take, half the frame every client accepts
const sessionDiffBytes = 4 * 1024 * 1024

// Owning profile home, the scoped home for a symbolic profile id
func tuiOpsProfileHome(ctx context.Context, a *SessionAuthority) string {
	if filepath.IsAbs(a.ProfileID) && isDir(a.ProfileID) {
		return a.ProfileID
	}
	return home.Dir(ctx)
}

// Live session of a verb whose params stay inside the allowed fields and whose actor holds the capability
func tuiOpsLive(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, capability string, fields ...string) (*LiveSession, error) {
	if paramsOutside(params, fields...) || ref.SessionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, c.authority, c.actor, ref, params, capability); err != nil {
		return nil, err
	}
	return liveSession(c.authority, ref.SessionID)
}

// Save snapshot of a physical row, the export cap refused as export_too_large and an empty one as not found
func saveSnapshot(ctx context.Context, a *SessionAuthority, target, format string) (*pyjson.Object, error) {
	data, err := state.LoadSaveSnapshot(ctx, a.DB, target, format)
	var tooLarge *state.SessionExportTooLargeError
	if errors.As(err, &tooLarge) {
		refusal := state.NewRuntimeStoreError("export_too_large", nil, nil)
		refusal.Cause = err
		return nil, refusal
	}
	if err != nil {
		return nil, err
	}
	if data == nil || len(data.Keys) == 0 {
		return nil, runtimeRefusal("not_found")
	}
	return data, nil
}

// Writes the stored transcript as a json save snapshot in the profile's saved sessions folder
func saveSession(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if _, err := tuiOpsLive(ctx, c, ref, params, "session:read", "session_id", "profile"); err != nil {
		return nil, err
	}
	a := c.authority
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	return forwardOffThread(ctx, func() (*pyjson.Object, error) {
		data, err := saveSnapshot(ctx, a, target, "json")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(tuiOpsProfileHome(ctx, a), "sessions", "saved",
			"icebath_conversation_"+pydatetime.Strftime(time.Now(), "%Y%m%d_%H%M%S")+".json")
		rendered, err := state.RenderSessionForSave(data, "json")
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, storageRefusal(err)
		}
		if err := os.WriteFile(path, []byte(rendered), 0o644); err != nil {
			return nil, storageRefusal(err)
		}
		return pyjson.ObjectOf("file", path), nil
	})
}

// storage_unavailable refusal caused by an OS error
func storageRefusal(err error) error {
	refusal := state.NewRuntimeStoreError("storage_unavailable", nil, nil)
	refusal.Cause = err
	return refusal
}

// Save transcript rendered for the client to write, json md or html, optionally force redacted
func exportSession(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if _, err := tuiOpsLive(ctx, c, ref, params, "session:read", "session_id", "profile", "format", "redact"); err != nil {
		return nil, err
	}
	format, isStr := params.GetOr("format", "json").(string)
	redact, isBool := params.GetOr("redact", false).(bool)
	if !isStr || !isBool {
		return nil, runtimeRefusal("invalid_params")
	}
	format, err := state.NormalizeSaveFormat(format)
	if err != nil {
		return nil, runtimeRefusal("invalid_params")
	}
	a := c.authority
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	content, err := forwardOffThread(ctx, func() (string, error) {
		data, err := saveSnapshot(ctx, a, target, format)
		if err != nil {
			return "", err
		}
		if redact {
			data = memory.RedactSessionData(data)
		}
		return state.RenderSessionForSave(data, format)
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("format", format, "filename", state.DefaultSaveFilename(target, format), "content", content), nil
}

// Display transcript of the conversation's current physical row with ancestors and durable row ids
func sessionHistory(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if _, err := tuiOpsLive(ctx, c, ref, params, "session:read", "session_id", "profile"); err != nil {
		return nil, err
	}
	a := c.authority
	target, err := transcriptTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	return forwardOffThread(ctx, func() (*pyjson.Object, error) {
		rows, err := a.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true})
		if err != nil {
			return nil, err
		}
		messages, err := displayProjection(ctx, rows, a.ProfileID)
		if err != nil {
			return nil, err
		}
		return pyjson.ObjectOf("count", len(rows), "messages", messages), nil
	})
}

// Re-reads the profile's env file, the process env on a single profile owner and the secret scope when multiplexed
func reloadEnv(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	if _, err := tuiOpsLive(ctx, c, ref, params, "session:control", "session_id", "profile"); err != nil {
		return nil, err
	}
	a := c.authority
	updated, err := forwardOffThread(ctx, func() (int, error) {
		if !a.Runner.MultiplexProfiles() {
			return config.ReloadEnv(home.Dir(ctx))
		}
		profileHome := tuiOpsProfileHome(ctx, a)
		before := provider.BuildProfileSecretScope(profileHome)
		config.InvalidateEnvFileCache(filepath.Join(profileHome, ".env"))
		provider.ResetSecretSourceCache(profileHome)
		provider.HydrateProfileSecretSources(profileHome)
		after := provider.BuildProfileSecretScope(profileHome)
		changed := 0
		for key, value := range before {
			if other, ok := after[key]; !ok || other != value {
				changed++
			}
		}
		for key := range after {
			if _, ok := before[key]; !ok {
				changed++
			}
		}
		return changed, nil
	})
	if err != nil {
		return nil, err
	}
	return pyjson.ObjectOf("updated", updated), nil
}

// Blocks or unblocks new delegate spawns across this owner, running children keep going
func pauseDelegation(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	paused, isBool := params.GetOr("paused", true).(bool)
	if paramsOutside(params, "paused", "profile", "session_id") || !isBool {
		return nil, runtimeRefusal("invalid_params")
	}
	if ref.SessionID != "" {
		if err := authorizeBusy(ctx, c.authority, c.actor, ref, params, "session:control"); err != nil {
			return nil, err
		}
	} else {
		if !c.actor.Capabilities["session:control"] {
			return nil, runtimeRefusal("permission_denied")
		}
		raw := params.GetOr("profile", nil)
		profile, isStr := raw.(string)
		if raw != nil && !isStr {
			return nil, runtimeRefusal("invalid_params")
		}
		if profile != "" && !profiles.ProfileMatchesHome(ctx, profile, c.authority.ProfileID) {
			return nil, runtimeRefusal("profile_mismatch")
		}
	}
	return pyjson.ObjectOf("paused", AncillaryAgent.SetSpawnPaused(paused)), nil
}

// Checkpoint list, diff, restore and session diff over the session's checkpoints keyed by its frozen cwd
func rollback(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object, kind string) (any, error) {
	invalid := false
	for _, key := range []string{"hash", "file_path"} {
		if value, present := params.Get(key); present {
			text, isStr := value.(string)
			if !isStr || (key == "hash" && text == "") {
				invalid = true
			}
		}
	}
	if _, hasHash := params.Get("hash"); (kind == "diff" || kind == "restore") && !hasHash {
		invalid = true
	}
	if all := params.GetOr("all", nil); all != nil {
		if _, isBool := all.(bool); !isBool {
			invalid = true
		}
	}
	if invalid {
		return nil, runtimeRefusal("invalid_params")
	}
	capability := "session:read"
	if kind == "restore" {
		capability = "session:control"
	}
	live, err := tuiOpsLive(ctx, c, ref, params, capability, rollbackFields[kind]...)
	if err != nil {
		return nil, err
	}
	a := c.authority
	policy, err := policyForSource(ctx, a.Runner, live.Source)
	if err != nil {
		return nil, err
	}
	if policy == nil {
		return nil, runtimeRefusal("permission_denied")
	}
	filePath, _ := params.GetOr("file_path", "").(string)
	if kind == "restore" && filePath == "" {
		handle, err := a.handle(ctx, ref)
		if err != nil {
			return nil, err
		}
		if handle.ExecutionState != "idle" {
			return nil, runtimeRefusal("session_busy")
		}
	}
	taskID, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	cfg, err := policy.config(ctx, a, false)
	if err != nil {
		return nil, err
	}
	kwargs := CheckpointAgentKwargs(cfg)
	terminalFields, err := loadsPolicyMap(policy.TerminalJSON)
	if err != nil {
		return nil, err
	}
	terminal := tools.TerminalPolicy{}
	for key, value := range terminalFields {
		terminal[key] = pyrepr.Str(value)
	}
	result, err := forwardOffThread(ctx, func() (*pyjson.Object, error) {
		return runRollback(ctx, kind, kwargs, terminal, policy.Cwd, taskID, params, filePath)
	})
	if err != nil {
		return nil, err
	}
	if kind == "restore" && pyrepr.Truthy(result.GetOr("success", nil)) && filePath == "" {
		removed, err := rewindLastTurn(ctx, c, ref)
		if err != nil {
			return nil, err
		}
		result.Set("history_removed", removed)
	}
	return result, nil
}

// One rollback verb against a checkpoint manager built from the session's frozen checkpoints policy
func runRollback(ctx context.Context, kind string, kwargs map[string]any, terminal tools.TerminalPolicy, cwd, taskID string, params *pyjson.Object, filePath string) (*pyjson.Object, error) {
	options, err := tools.CheckpointManagerOptionsFromKwargs(pyrepr.Truthy(kwargs["checkpoints_enabled"]), kwargs)
	if err != nil {
		return nil, err
	}
	env := &tools.Env{Home: home.Dir(ctx)}
	mgr := tools.NewCheckpointManager(env, options)
	if !mgr.Enabled && (kind == "list" || kind == "session_diff") {
		if kind == "list" {
			return pyjson.ObjectOf("enabled", false, "checkpoints", []any{}), nil
		}
		return pyjson.ObjectOf("enabled", false), nil
	}
	if kind == "list" {
		rows := []any{}
		for _, entry := range mgr.ListCheckpoints(cwd) {
			rows = append(rows, pyjson.ObjectOf("hash", entry.Hash, "timestamp", entry.Timestamp, "message", entry.Reason))
		}
		return pyjson.ObjectOf("enabled", true, "checkpoints", rows), nil
	}
	scoped := *env
	scoped.TerminalScope = &tools.TerminalScope{Policy: terminal}
	refusal := tools.NewCheckpointManager(&scoped, options).UnsupportedBackendReason(taskID)
	if kind == "session_diff" {
		return sessionDiff(mgr, cwd, refusal), nil
	}
	hash, _ := params.GetOr("hash", "").(string)
	target, err := resolveCheckpointHash(mgr, cwd, hash)
	if err != nil {
		if kind == "diff" {
			invalid := state.NewRuntimeStoreError("invalid_checkpoint", nil, nil)
			invalid.Cause = err
			return nil, invalid
		}
		return pyjson.ObjectOf("success", false, "error", err.Error()), nil
	}
	if kind == "diff" {
		if refusal != "" {
			return nil, runtimeRefusal("unsupported_terminal_backend")
		}
		result := mgr.Diff(cwd, target)
		raw := ""
		if text, ok := result["diff"]; ok {
			raw = pyrepr.Str(text)
		}
		if runes := []rune(raw); len(runes) > 4000 {
			raw = string(runes[:4000])
		}
		stat, ok := result["stat"]
		if !ok {
			stat = ""
		}
		payload := pyjson.ObjectOf("stat", stat, "diff", raw)
		if rendered, ok := tuigateway.RenderDiff(raw, 80); ok && rendered != "" {
			payload.Set("rendered", rendered)
		}
		return payload, nil
	}
	if refusal != "" {
		return pyjson.ObjectOf("success", false, "error", refusal), nil
	}
	all := pyrepr.Truthy(params.GetOr("all", nil))
	restored, _ := pyjson.AsObject(mgr.Restore(cwd, target, filePath, filePath == "" && !all))
	return restored, nil
}

// A rollback number names its checkpoint's hash, anything that is not a number passes through
func resolveCheckpointHash(mgr *tools.CheckpointManager, cwd, ref string) (string, error) {
	checkpoints := mgr.ListCheckpoints(cwd)
	number, err := pyIntLiteral(ref)
	if err != nil {
		return ref, nil
	}
	index := number - 1
	if index >= 0 && index < int64(len(checkpoints)) {
		return checkpoints[index].Hash, nil
	}
	return "", &PyError{Kind: "ValueError", Msg: "Invalid checkpoint number. Use 1-" + strconv.Itoa(len(checkpoints)) + "."}
}

// Value of a Python int() of text, a ValueError for anything int() refuses
func pyIntLiteral(text string) (int64, error) {
	if !pyIntPattern.MatchString(text) {
		return 0, &PyError{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(text)}
	}
	value, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(text), "_", ""), 10, 64)
	if err != nil {
		return 0, &PyError{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(text)}
	}
	return value, nil
}

// Decimal literal int() accepts, surrounding whitespace and single underscores between digits allowed
var pyIntPattern = regexp.MustCompile(`^\s*[+-]?\d+(_\d+)*\s*$`)

// Earliest retained checkpoint against the tree as data, cut to the frame budget by whole lines
func sessionDiff(mgr *tools.CheckpointManager, cwd, refusal string) *pyjson.Object {
	if refusal != "" {
		return pyjson.ObjectOf("enabled", true, "success", false, "error", refusal)
	}
	result := mgr.SessionDiff(cwd)
	if !pyrepr.Truthy(result["success"]) {
		message := ""
		if pyrepr.Truthy(result["error"]) {
			message = pyrepr.Str(result["error"])
		}
		return pyjson.ObjectOf("enabled", true, "success", false, "error", message)
	}
	diff, stat := "", ""
	if value, ok := result["diff"]; ok {
		diff = pyrepr.Str(value)
	}
	if value, ok := result["stat"]; ok {
		stat = pyrepr.Str(value)
	}
	stat = fitFrame(stat, sessionDiffBytes)
	body := fitFrame(diff, sessionDiffBytes-wireBytes(stat))
	return pyjson.ObjectOf("enabled", true, "success", true, "stat", stat, "diff", body,
		"diff_lines", len(pystr.Splitlines(diff, false)), "empty", pyrepr.Truthy(result["empty"]))
}

// Bytes text takes inside a frame, JSON escaped without ASCII escaping and then UTF-8
func wireBytes(text string) int {
	encoded, err := pyjson.Dumps(text, pyjson.EnsureASCII(false))
	if err != nil {
		return len(text)
	}
	return len(encoded) - 2
}

// Longest whole line prefix of text whose wire size fits the budget
func fitFrame(text string, budget int) string {
	runes := []rune(text)
	if len(runes)*6 <= budget || (len(runes) <= budget && wireBytes(text) <= budget) {
		return text
	}
	head := runes
	if len(head) > budget+1 {
		head = head[:budget+1]
	}
	var kept strings.Builder
	used := 0
	for _, line := range strings.SplitAfter(string(head), "\n") {
		if line == "" {
			continue
		}
		used += wireBytes(line)
		if used > budget {
			break
		}
		kept.WriteString(line)
	}
	return strings.TrimSuffix(kept.String(), "\n")
}

// Drops the newest user turn and everything after it through the fenced rewind mutation, 0 without a user turn
func rewindLastTurn(ctx context.Context, c *AuthorityConnection, ref SessionRef) (int64, error) {
	a := c.authority
	rows, err := forwardOffThread(ctx, func() ([]map[string]any, error) { return localHistory(ctx, a, ref, nil) })
	if err != nil {
		return 0, err
	}
	var target any
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if _, isInt := row["row_id"].(int64); !isInt {
			if _, isInt = row["row_id"].(int); !isInt {
				continue
			}
		}
		if pyrepr.Truthy(row["archived"]) || state.UserOriginatedTurnView(row) == nil {
			continue
		}
		target = row["row_id"]
		break
	}
	if target == nil {
		return 0, nil
	}
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return 0, err
	}
	id := uuid.New()
	result, err := mutateSession(ctx, a, c.actor, ref, pyjson.ObjectOf("session_id", ref.SessionID,
		"request_id", "rollback:"+hex.EncodeToString(id[:]), "expected_revision", handle.Revision,
		"expected_generation", handle.ExecutionGeneration, "operation", "rewind",
		"payload", pyjson.ObjectOf("target_message_id", target)))
	if err != nil {
		if state.IsMutationGuardRefusal(err) {
			refusal := state.NewRuntimeStoreError("session_busy", nil, nil)
			refusal.Cause = err
			return 0, refusal
		}
		return 0, err
	}
	count := result.GetOr("rewound_count", nil)
	if !pyrepr.Truthy(count) {
		return 0, nil
	}
	return anyInt64(count), nil
}
