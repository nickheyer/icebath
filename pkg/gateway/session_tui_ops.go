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
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Save, export, reload, delegation pause, rollback, context breakdown, history, account usage and runtime check verbs of the shared owner
func tuiOpsHandlers(c *AuthorityConnection) map[string]controlHandler {
	bind := func(fn func(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error)) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
			return fn(ctx, c, ref, params)
		}
	}
	rollbackOf := func(kind string) controlHandler {
		return func(ctx context.Context, ref SessionRef, params *jsonutil.Object) (any, error) {
			return rollback(ctx, c, ref, params, kind)
		}
	}
	return map[string]controlHandler{"session.save": bind(saveSession), "session.export": bind(exportSession),
		"reload.env": bind(reloadEnv), "delegation.pause": bind(pauseDelegation),
		"rollback.list": rollbackOf("list"), "rollback.diff": rollbackOf("diff"), "rollback.restore": rollbackOf("restore"),
		"rollback.session_diff": rollbackOf("session_diff"), "session.context_breakdown": bind(contextBreakdown),
		"session.history": bind(sessionHistory), "session.account_usage": bind(accountUsage),
		"setup.runtime_check": func(ctx context.Context, _ SessionRef, params *jsonutil.Object) (any, error) {
			return readiness(ctx, c.authority, c.actor, params, true)
		}}
}

// Params each rollback verb accepts
var rollbackFields = map[string][]string{"list": {"session_id", "profile", "undo_since", "undo_message_id"}, "diff": {"session_id", "profile", "hash"},
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
func tuiOpsLive(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object, capability string, fields ...string) (*LiveSession, error) {
	if paramsOutside(params, fields...) || ref.SessionID == "" {
		return nil, runtimeRefusal("invalid_params")
	}
	if err := authorizeBusy(ctx, c.authority, c.actor, ref, params, capability); err != nil {
		return nil, err
	}
	return liveSession(c.authority, ref.SessionID)
}

// Save snapshot of a physical row, the export cap refused as export_too_large and an empty one as not found
func saveSnapshot(ctx context.Context, a *SessionAuthority, target, format string) (*jsonutil.Object, error) {
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
	if data == nil || data.Len() == 0 {
		return nil, runtimeRefusal("not_found")
	}
	return data, nil
}

// Writes the stored transcript as a json save snapshot in the profile's saved sessions folder
func saveSession(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	if _, err := tuiOpsLive(ctx, c, ref, params, "session:read", "session_id", "profile"); err != nil {
		return nil, err
	}
	a := c.authority
	target, err := physicalTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	return forwardOffThread(ctx, func() (*jsonutil.Object, error) {
		data, err := saveSnapshot(ctx, a, target, "json")
		if err != nil {
			return nil, err
		}
		path := filepath.Join(tuiOpsProfileHome(ctx, a), "sessions", "saved",
			"icebath_conversation_"+time.Now().Format("20060102_150405")+".json")
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
		return jsonutil.ObjectOf("file", path), nil
	})
}

// storage_unavailable refusal caused by an OS error
func storageRefusal(err error) error {
	refusal := state.NewRuntimeStoreError("storage_unavailable", nil, nil)
	refusal.Cause = err
	return refusal
}

// Save transcript rendered for the client to write, json md or html, optionally force redacted
func exportSession(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	if _, err := tuiOpsLive(ctx, c, ref, params, "session:read", "session_id", "profile", "format", "redact"); err != nil {
		return nil, err
	}
	format, isStr := params.ValueOr("format", "json").(string)
	redact, isBool := params.ValueOr("redact", false).(bool)
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
	return jsonutil.ObjectOf("format", format, "filename", state.DefaultSaveFilename(target, format), "content", content), nil
}

// Display transcript of the conversation's current physical row with ancestors and durable row ids
func sessionHistory(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	if _, err := tuiOpsLive(ctx, c, ref, params, "session:read", "session_id", "profile"); err != nil {
		return nil, err
	}
	a := c.authority
	target, err := transcriptTarget(ctx, a, ref)
	if err != nil {
		return nil, err
	}
	return forwardOffThread(ctx, func() (*jsonutil.Object, error) {
		rows, err := a.DB.GetMessagesAsConversation(ctx, target, state.ConversationOptions{IncludeAncestors: true, IncludeRowIDs: true})
		if err != nil {
			return nil, err
		}
		messages, err := displayProjection(ctx, rows, a.ProfileID)
		if err != nil {
			return nil, err
		}
		if messages, err = withShellLines(messages, a.ProfileID, ref.SessionID); err != nil {
			return nil, err
		}
		return jsonutil.ObjectOf("count", len(rows), "messages", messages), nil
	})
}

// Re-reads the profile's env file, the process env on a single profile owner and the secret scope when multiplexed
func reloadEnv(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
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
	return tuigateway.Plain(tuigateway.ReloadEnvResult{Updated: int64(updated)}), nil
}

// Blocks or unblocks new delegate spawns across this owner, running children keep going
func pauseDelegation(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object) (any, error) {
	paused, isBool := params.ValueOr("paused", true).(bool)
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
		raw := params.ValueOr("profile", nil)
		profile, isStr := raw.(string)
		if raw != nil && !isStr {
			return nil, runtimeRefusal("invalid_params")
		}
		if profile != "" && !profiles.ProfileMatchesHome(ctx, profile, c.authority.ProfileID) {
			return nil, runtimeRefusal("profile_mismatch")
		}
	}
	return tuigateway.Plain(tuigateway.DelegationPauseResult{Paused: AncillaryAgent.SetSpawnPaused(paused)}), nil
}

// Checkpoint list, diff, restore and session diff over the session's checkpoints keyed by its frozen cwd
func rollback(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *jsonutil.Object, kind string) (any, error) {
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
	if all := params.ValueOr("all", nil); all != nil {
		if _, isBool := all.(bool); !isBool {
			invalid = true
		}
	}
	undoSince := params.ValueOr("undo_since", nil)
	switch undoSince.(type) {
	case nil, int64, int, float64:
	default:
		invalid = true
	}
	undoCut, cutNamed := int64(0), false
	switch cut := params.ValueOr("undo_message_id", nil).(type) {
	case nil:
	case int64:
		undoCut, cutNamed = cut, true
	case int:
		undoCut, cutNamed = int64(cut), true
	default:
		invalid = true
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
	filePath, _ := params.ValueOr("file_path", "").(string)
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
		terminal[key] = anyutil.String(value)
	}
	var undoSpan func() (float64, bool, error)
	if kind == "list" && undoSince != nil {
		undoSpan = func() (float64, bool, error) {
			var kept []map[string]any
			if cutNamed {
				transcript, err := transcriptTarget(ctx, a, ref)
				if err != nil {
					return 0, false, err
				}
				rows, err := a.DB.GetMessagesAsConversation(ctx, transcript, state.ConversationOptions{IncludeRowIDs: true})
				if err != nil {
					return 0, false, err
				}
				for _, row := range rows {
					switch id := row["_row_id"].(type) {
					case int64:
						if id < undoCut {
							kept = append(kept, row)
						}
					case int:
						if int64(id) < undoCut {
							kept = append(kept, row)
						}
					}
				}
			}
			start, known := UndoSpanStart(undoSince, kept)
			return start, known, nil
		}
	}
	result, err := forwardOffThread(ctx, func() (any, error) {
		return runRollback(ctx, kind, kwargs, terminal, policy.Cwd, taskID, params, filePath, undoSpan)
	})
	if err != nil {
		return nil, err
	}
	if restored, isRestore := result.(tuigateway.RollbackRestoreResult); isRestore && restored.Success && filePath == "" {
		removed, err := rewindLastTurn(ctx, c, ref)
		if err != nil {
			return nil, err
		}
		restored.HistoryRemoved = &removed
		result = restored
	}
	return tuigateway.Plain(result), nil
}

// Typed outcome of a checkpoint restore, a key the contract does not declare refused
func RestoreOutcome(res map[string]any) (tuigateway.RollbackRestoreResult, error) {
	var out tuigateway.RollbackRestoreResult
	return out, tuigateway.Strict(res, &out)
}

// One rollback verb against a checkpoint manager built from the session's frozen checkpoints policy, a list also naming the files an undone span changed when its start is given
func runRollback(ctx context.Context, kind string, kwargs map[string]any, terminal tools.TerminalPolicy, cwd, taskID string, params *jsonutil.Object, filePath string, undoSpan func() (float64, bool, error)) (any, error) {
	options, err := tools.CheckpointManagerOptionsFromKwargs(anyutil.IsTrue(kwargs["checkpoints_enabled"]), kwargs)
	if err != nil {
		return nil, err
	}
	env := &tools.Env{Home: home.Dir(ctx)}
	mgr := tools.NewCheckpointManager(env, options)
	if !mgr.Enabled && (kind == "list" || kind == "session_diff") {
		if kind == "list" {
			return tuigateway.RollbackListResult{Checkpoints: []tuigateway.RollbackCheckpoint{}}, nil
		}
		return tuigateway.RollbackSessionDiffResult{}, nil
	}
	backendRefusal := func() string {
		scoped := *env
		scoped.TerminalScope = &tools.TerminalScope{Policy: terminal}
		return tools.NewCheckpointManager(&scoped, options).UnsupportedBackendReason(taskID)
	}
	if kind == "list" {
		rows := []tuigateway.RollbackCheckpoint{}
		for _, entry := range mgr.ListCheckpoints(cwd) {
			rows = append(rows, tuigateway.RollbackCheckpoint{Hash: entry.Hash, Timestamp: entry.Timestamp, Message: entry.Reason})
		}
		listed := tuigateway.RollbackListResult{Enabled: true, Checkpoints: rows}
		if undoSpan != nil && backendRefusal() == "" {
			since, known, err := undoSpan()
			if err != nil {
				return nil, err
			}
			listed.UndoHint = UndoHint(mgr, cwd, since, known)
		}
		return listed, nil
	}
	refusal := backendRefusal()
	if kind == "session_diff" {
		return sessionDiff(mgr, cwd, refusal), nil
	}
	hash, _ := params.ValueOr("hash", "").(string)
	target, err := resolveCheckpointHash(mgr, cwd, hash)
	if err != nil {
		if kind == "diff" {
			invalid := state.NewRuntimeStoreError("invalid_checkpoint", nil, nil)
			invalid.Cause = err
			return nil, invalid
		}
		return tuigateway.RollbackRestoreResult{Error: err.Error()}, nil
	}
	if kind == "diff" {
		if refusal != "" {
			return nil, runtimeRefusal("unsupported_terminal_backend")
		}
		result := mgr.Diff(cwd, target)
		raw := ""
		if text, ok := result["diff"]; ok {
			raw = anyutil.String(text)
		}
		if runes := []rune(raw); len(runes) > 4000 {
			raw = string(runes[:4000])
		}
		stat := ""
		if value, ok := result["stat"]; ok {
			stat = anyutil.String(value)
		}
		payload := tuigateway.RollbackDiffResult{Stat: stat, Diff: raw}
		if rendered, ok := tuihost.RenderDiff(raw, 80); ok {
			payload.Rendered = rendered
		}
		return payload, nil
	}
	if refusal != "" {
		return tuigateway.RollbackRestoreResult{Error: refusal}, nil
	}
	all := anyutil.IsTrue(params.ValueOr("all", nil))
	return RestoreOutcome(mgr.Restore(cwd, target, filePath, filePath == "" && !all))
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
		return 0, &PyError{Kind: "ValueError", Msg: text + " is not a whole number"}
	}
	value, err := strconv.ParseInt(strings.ReplaceAll(strings.TrimSpace(text), "_", ""), 10, 64)
	if err != nil {
		return 0, &PyError{Kind: "ValueError", Msg: text + " is not a whole number"}
	}
	return value, nil
}

// Decimal literal int() accepts, surrounding whitespace and single underscores between digits allowed
var pyIntPattern = regexp.MustCompile(`^\s*[+-]?\d+(_\d+)*\s*$`)

// Earliest retained checkpoint against the tree as data, cut to the frame budget by whole lines
func sessionDiff(mgr *tools.CheckpointManager, cwd, refusal string) tuigateway.RollbackSessionDiffResult {
	if refusal != "" {
		return tuigateway.RollbackSessionDiffResult{Enabled: true, Error: refusal}
	}
	result := mgr.SessionDiff(cwd)
	if !anyutil.IsTrue(result["success"]) {
		message := ""
		if anyutil.IsTrue(result["error"]) {
			message = anyutil.String(result["error"])
		}
		return tuigateway.RollbackSessionDiffResult{Enabled: true, Error: message}
	}
	diff, stat := "", ""
	if value, ok := result["diff"]; ok {
		diff = anyutil.String(value)
	}
	if value, ok := result["stat"]; ok {
		stat = anyutil.String(value)
	}
	stat = fitFrame(stat, sessionDiffBytes)
	body := fitFrame(diff, sessionDiffBytes-wireBytes(stat))
	return tuigateway.RollbackSessionDiffResult{Enabled: true, Success: true, Stat: stat, Diff: body,
		DiffLines: int64(len(textutil.Lines(diff))), Empty: anyutil.IsTrue(result["empty"])}
}

// Bytes text takes inside a frame, JSON escaped without ASCII escaping and then UTF-8
func wireBytes(text string) int {
	encoded, err := jsonutil.Encode(text, jsonutil.Spaced())
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
	count, _, err := rewindLastUserTurn(ctx, c.authority, c.actor, ref, "")
	return count, err
}

// Drops the newest user turn and everything after it as actor, the count removed and the user row it dropped, 0 and nil without a user turn
// or, when only is given, when the newest user turn says something else
func rewindLastUserTurn(ctx context.Context, a *SessionAuthority, actor Principal, ref SessionRef, only string) (int64, map[string]any, error) {
	rows, err := forwardOffThread(ctx, func() ([]map[string]any, error) { return localHistory(ctx, a, ref, nil) })
	if err != nil {
		return 0, nil, err
	}
	var target any
	var dropped map[string]any
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if _, isInt := row["row_id"].(int64); !isInt {
			if _, isInt = row["row_id"].(int); !isInt {
				continue
			}
		}
		if anyutil.IsTrue(row["archived"]) || state.UserOriginatedTurnView(row) == nil {
			continue
		}
		target, dropped = row["row_id"], row
		break
	}
	if target == nil {
		return 0, nil, nil
	}
	if only != "" {
		if text, err := state.RetryableUserText(dropped["content"]); err != nil || text != only {
			return 0, nil, nil
		}
	}
	handle, err := a.handle(ctx, ref)
	if err != nil {
		return 0, nil, err
	}
	id := uuid.New()
	result, err := mutateSession(ctx, a, actor, ref, jsonutil.ObjectOf("session_id", ref.SessionID,
		"request_id", "rollback:"+hex.EncodeToString(id[:]), "expected_revision", handle.Revision,
		"expected_generation", handle.ExecutionGeneration, "operation", "rewind",
		"payload", jsonutil.ObjectOf("target_message_id", target)))
	if err != nil {
		if state.IsMutationGuardRefusal(err) {
			refusal := state.NewRuntimeStoreError("session_busy", nil, nil)
			refusal.Cause = err
			return 0, nil, refusal
		}
		return 0, nil, err
	}
	count := result.ValueOr("rewound_count", nil)
	if !anyutil.IsTrue(count) {
		return 0, nil, nil
	}
	return anyInt64(count), dropped, nil
}
