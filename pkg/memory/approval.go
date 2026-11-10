package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Actions that rewrite or delete an existing entry, replace kept so writes staged before its removal still replay
var bgDeleteActions = map[string]bool{"replace": true, "remove": true, "patch": true}

// True for a background review fork the user did not explicitly ask for
func IsUnattendedReview(env *tools.Env) bool {
	if tools.CurrentOrigin(env) != backgroundReviewOrigin {
		return false
	}
	if env != nil && env.Values != nil {
		if v, ok := env.Values.Load(ReviewAttendedValuesKey); ok {
			if attended, ok := v.(bool); ok && attended {
				return false
			}
		}
	}
	return true
}

// Replace, patch and remove ops of a staged memory payload in single op or batch shape
func DestructiveOps(payload map[string]any) ([]map[string]any, error) {
	var ops []map[string]any
	if payload["action"] == "batch" {
		list, err := opMaps(payload["operations"])
		if err != nil {
			return nil, err
		}
		ops = list
	} else {
		ops = []map[string]any{payload}
	}
	var out []map[string]any
	for _, op := range ops {
		if act, ok := op["action"].(string); ok && bgDeleteActions[act] {
			out = append(out, op)
		}
	}
	return out, nil
}

// Pins each staged edit to the full entry it selects now, returning the direct write's error when the search fails
func pinMatchedEntries(ctx context.Context, store *Store, payload map[string]any) (string, string, error) {
	target, _ := payload["target"].(string)
	action := payload["action"]
	var res Result
	switch {
	case action == "batch":
		raw, _ := payload["operations"].([]any)
		ops, err := opMaps(raw)
		if err != nil {
			return "", "", err
		}
		res, err = store.ResolveBatchEntries(ctx, target, ops)
		if err != nil {
			return "", "", err
		}
		if res.Success() {
			matched, _ := res.Data["matched_entries"].([]any)
			pinned := make([]any, len(raw))
			for i, op := range raw {
				pinned[i] = op
				if i < len(matched) && matched[i] != nil {
					copied := map[string]any{}
					for k, v := range op.(map[string]any) {
						copied[k] = v
					}
					copied["matched_entry"] = matched[i]
					pinned[i] = copied
				}
			}
			payload["operations"] = pinned
		}
	default:
		verb, ok := action.(string)
		if !ok || !bgDeleteActions[verb] {
			return "", "", nil
		}
		oldText, err := strMethodArg(payload["old_text"])
		if err != nil {
			return "", "", err
		}
		res, err = store.ResolveEntry(ctx, target, oldText, verb)
		if err != nil {
			return "", "", err
		}
		if res.Success() {
			payload["matched_entry"] = res.Data["matched_entry"]
		}
	}
	if res.Success() {
		return "", "", nil
	}
	return tools.JSON(res.Data), res.FailureClass, nil
}

// Result JSON when the write must not proceed because it was blocked or staged, empty to proceed
func gateOrStage(ctx context.Context, env *tools.Env, store *Store, summary, detail string, payload map[string]any) (string, string, error) {
	decision := tools.CheckWriteApproval(ctx, env, tools.WriteApprovalMemory, summary, detail)
	if decision.Allow {
		return "", "", nil
	}
	if decision.Blocked {
		return toolError(decision.Message, map[string]any{"success": false}), "gate_refused", nil
	}
	unmatched, class, err := pinMatchedEntries(tools.WithEnvHome(ctx, env), store, payload)
	if err != nil {
		return "", "", err
	}
	if unmatched != "" {
		return unmatched, class, nil
	}
	record := tools.StageWrite(tools.WithEnvHome(ctx, env), env, tools.WriteApprovalMemory, payload, summary+": "+runePrefix(detail, 120), tools.CurrentOrigin(env))
	return tools.JSON(tools.Dict("success", true, "staged", true, "pending_id", record["id"], "message", decision.Message)), "staged", nil
}

// Gates one mutating op, or a whole batch as a single unit when operations is set
func applyWriteGate(ctx context.Context, env *tools.Env, store *Store, action, target string, content, oldText any, operations []any) (string, string, error) {
	label := "memory"
	if target == "user" {
		label = "user profile"
	}
	if operations != nil {
		lines := make([]string, len(operations))
		for i, op := range operations {
			lines[i] = batchOpLine(op)
		}
		return gateOrStage(ctx, env, store, fmt.Sprintf("apply %d op(s) to %s", len(operations), label), strings.Join(lines, "\n"),
			map[string]any{"action": "batch", "target": target, "operations": operations})
	}
	summary, detail := gateText(action, label, content, oldText)
	return gateOrStage(ctx, env, store, summary, detail, map[string]any{"action": action, "target": target, "content": content, "old_text": oldText})
}

// Stages patch and remove ops of an unattended background review instead of applying them, add stays open
func backgroundDeleteGate(ctx context.Context, env *tools.Env, store *Store, action any, operations []any, target string, content, oldText any) (string, string, error) {
	if !IsUnattendedReview(env) {
		return "", "", nil
	}
	var payload map[string]any
	if operations != nil {
		payload = map[string]any{"action": "batch", "target": target, "operations": operations}
	} else {
		payload = map[string]any{"action": action, "target": target, "content": content, "old_text": oldText}
	}
	destructive, err := DestructiveOps(payload)
	if err != nil {
		return "", "", err
	}
	if len(destructive) == 0 {
		return "", "", nil
	}
	kind := "batch"
	var detail string
	if operations != nil {
		lines := make([]string, len(operations))
		for i, op := range operations {
			lines[i] = batchOpLine(op)
		}
		detail = strings.Join(lines, "; ")
	} else {
		kind = pyStr(action)
		detail = batchOpLine(map[string]any{"action": action, "content": content, "old_text": oldText})
	}
	unmatched, class, err := pinMatchedEntries(tools.WithEnvHome(ctx, env), store, payload)
	if err != nil {
		envLog(env).Warn("Failed to stage background-review consolidation; denying", "error", err)
		return toolError("Background review may not delete memory entries ('patch'/'remove', including in a "+
			"batch); 'add' is still available.", map[string]any{"success": false}), "gate_refused", nil
	}
	if unmatched != "" {
		return unmatched, class, nil
	}
	summary := runePrefix(fmt.Sprintf("background review consolidation (%s on %s): %s", kind, target, detail), 200)
	record := tools.StageWrite(tools.WithEnvHome(ctx, env), env, tools.WriteApprovalMemory, payload, summary, tools.CurrentOrigin(env))
	return tools.JSON(tools.Dict(
		"success", true, "staged", true, "proposal_staged", true, "pending_id", record["id"],
		"message", "Background review may not delete memory entries unattended. The proposed "+
			kind+" was staged for your approval — "+
			"review it with /memory pending (approve to apply, discard to drop).",
	)), "staged", nil
}

// Replays a staged write bypassing the gate, destructive ops applying only to their pinned entry
func ApplyMemoryPending(ctx context.Context, payload map[string]any, store *Store) (Result, error) {
	action, target := payload["action"], payload["target"]
	if targetErr := memoryTargetError(store, target, payload["operations"]); targetErr != nil {
		return *targetErr, nil
	}
	destructive, err := DestructiveOps(payload)
	if err != nil {
		return Result{}, err
	}
	for _, op := range destructive {
		if !pyTruthy(op["matched_entry"]) {
			return Result{Data: map[string]any{"success": false, "error": "This destructive pending write predates entry pinning and cannot be " +
				"verified; nothing was applied. Reject it and recreate the change."}, FailureClass: "other"}, nil
		}
	}
	name := target.(string)
	if action == "batch" {
		ops, err := opMaps(payload["operations"])
		if err != nil {
			return Result{}, err
		}
		return store.ApplyBatch(ctx, name, ops)
	}
	if err := hashable(action); err != nil {
		return Result{}, err
	}
	verb, _ := action.(string)
	if verb != "add" && verb != "replace" && verb != "remove" && verb != "patch" {
		return Result{Data: map[string]any{"success": false, "error": fmt.Sprintf("Unknown staged action '%s'.", pyStr(action))}, FailureClass: "other"}, nil
	}
	content, oldText := payload["content"], payload["old_text"]
	if !pyTruthy(content) {
		content = ""
	}
	if !pyTruthy(oldText) {
		oldText = ""
	}
	return storeCall(ctx, store, verb, name, content, oldText, payload["matched_entry"])
}

// PendingOptions binds the store memory approvals apply to, the staged skill replayer and the gate setter
type PendingOptions struct {
	MemoryStore *Store
	ApplySkill  func(payload map[string]any) string
	SetMode     func(enabled bool) error
}

// Setter persisting subsystem write_approval in the home config
func ConfigModeSetter(ctx context.Context, env *tools.Env, subsystem string) func(enabled bool) error {
	return func(enabled bool) error {
		return config.SetKey(tools.WithEnvHome(ctx, env), envHome(env), subsystem+".write_approval", enabled)
	}
}

func fmtState(env *tools.Env, subsystem string) string {
	state := "off"
	if tools.WriteApprovalEnabled(env, subsystem) {
		state = "on"
	}
	return subsystem + ".write_approval = " + state
}

// Value of a record key, absent read as the default like dict get
func recordValue(r map[string]any, key string, def any) any {
	if v, ok := r[key]; ok {
		return v
	}
	return def
}

// Value of a required record key, a KeyError crash when absent
func recordKey(r map[string]any, key string) (any, error) {
	v, ok := r[key]
	if !ok {
		return nil, &pyError{class: "KeyError", msg: pyRepr(key)}
	}
	return v, nil
}

func fmtPendingList(env *tools.Env, subsystem string) (string, error) {
	records := tools.ListPending(env, subsystem)
	if len(records) == 0 {
		return fmt.Sprintf("No pending %s writes.", subsystem), nil
	}
	lines := []string{fmt.Sprintf("Pending %s writes (%d):", subsystem, len(records))}
	for _, r := range records {
		tag := ""
		if recordValue(r, "origin", "foreground") == backgroundReviewOrigin {
			tag = " [auto]"
		}
		id, err := recordKey(r, "id")
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("  %s%s  %s", pyStr(id), tag, pyStr(recordValue(r, "summary", ""))))
		if subsystem == tools.WriteApprovalMemory {
			payload, err := recordKey(r, "payload")
			if err != nil {
				return "", err
			}
			matched, err := matchedEntryLines(payload)
			if err != nil {
				return "", err
			}
			for _, line := range matched {
				lines = append(lines, "      "+line)
			}
		}
	}
	lines = append(lines, "", fmt.Sprintf("Apply: /%s approve <id>   Reject: /%s reject <id>", subsystem, subsystem))
	if subsystem == tools.WriteApprovalSkills {
		lines = append(lines, "Review full diff: /skills diff <id>")
	}
	return strings.Join(lines, "\n"), nil
}

// Full entry each staged destructive op is pinned to, since approval applies to it and not to old_text
func matchedEntryLines(payload any) ([]string, error) {
	m, ok := payload.(map[string]any)
	if !ok {
		return nil, &pyError{class: "AttributeError", msg: fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(payload))}
	}
	ops, err := DestructiveOps(m)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		if pyTruthy(op["matched_entry"]) {
			out = append(out, fmt.Sprintf("%ss entry: %s", pyStr(op["action"]), pyStr(op["matched_entry"])))
		} else {
			out = append(out, fmt.Sprintf("%s: unpinned legacy target — reject and recreate before approving", pyStr(op["action"])))
		}
	}
	return out, nil
}

// Dispatches a memory or skills write approval subcommand, false when the args are not one so the caller falls through
func HandlePendingSubcommand(ctx context.Context, env *tools.Env, subsystem string, args []string, opts PendingOptions) (string, bool, error) {
	if len(args) == 0 {
		list, err := fmtPendingList(env, subsystem)
		if err != nil {
			return "", true, err
		}
		return fmtState(env, subsystem) + "\n\n" + list, true, nil
	}
	sub, rest := pyLower(args[0]), args[1:]
	switch {
	case sub == "pending":
		list, err := fmtPendingList(env, subsystem)
		return list, true, err
	case sub == "approve" || sub == "apply":
		out, err := approvePending(tools.WithEnvHome(ctx, env), env, subsystem, rest, opts)
		return out, true, err
	case sub == "reject" || sub == "deny" || sub == "drop":
		out, err := rejectPending(env, subsystem, rest)
		return out, true, err
	case sub == "diff" && subsystem == tools.WriteApprovalSkills:
		out, err := skillPendingDiff(env, rest)
		return out, true, err
	case sub == "approval" || sub == "mode":
		return setApproval(env, subsystem, rest, opts.SetMode), true, nil
	}
	return "", false, nil
}

func approvalUsage(subsystem string) string {
	return fmt.Sprintf("Usage: /%s approve|reject <id>  (or 'all')", subsystem)
}

func approvePending(ctx context.Context, env *tools.Env, subsystem string, rest []string, opts PendingOptions) (string, error) {
	if len(rest) == 0 {
		return approvalUsage(subsystem), nil
	}
	target := rest[0]
	records := tools.ListPending(env, subsystem)
	if len(records) == 0 {
		return fmt.Sprintf("No pending %s writes.", subsystem), nil
	}
	var targets []map[string]any
	if pyLower(target) == "all" {
		targets = records
	} else {
		rec := tools.GetPending(env, subsystem, target)
		if len(rec) == 0 {
			return fmt.Sprintf("No pending %s write with id '%s'.", subsystem, target), nil
		}
		targets = []map[string]any{rec}
	}
	applied := 0
	var failed, overwritten, patched, removed []string
	for _, rec := range targets {
		ok, msg, result := applyOnePending(tools.WithEnvHome(ctx, env), subsystem, rec, opts)
		id, err := recordKey(rec, "id")
		if err != nil {
			return "", err
		}
		if !ok {
			failed = append(failed, fmt.Sprintf("%s: %s", pyStr(id), msg))
			continue
		}
		idText := pyStr(id)
		tools.DiscardPending(env, subsystem, idText)
		applied++
		for _, text := range changedEntries(result, "replaced") {
			overwritten = append(overwritten, fmt.Sprintf("  %s: %s", idText, text))
		}
		for _, text := range changedEntries(result, "patched") {
			patched = append(patched, fmt.Sprintf("  %s: %s", idText, text))
		}
		for _, text := range changedEntries(result, "removed") {
			removed = append(removed, fmt.Sprintf("  %s: %s", idText, text))
		}
	}
	out := []string{fmt.Sprintf("Approved %d %s write(s).", applied, subsystem)}
	if len(overwritten) > 0 {
		out = append(out, "Overwrote entire entry (re-add anything you still need):")
		out = append(out, overwritten...)
	}
	if len(patched) > 0 {
		out = append(out, "Patched entry (as it was before the edit):")
		out = append(out, patched...)
	}
	if len(removed) > 0 {
		out = append(out, "Removed entry (re-add anything you still need):")
		out = append(out, removed...)
	}
	if len(failed) > 0 {
		out = append(out, "Failed:")
		for _, f := range failed {
			out = append(out, "  "+f)
		}
	}
	return strings.Join(out, "\n"), nil
}

// Full text of every entry a memory write changed, single op or batch shape
func changedEntries(result map[string]any, kind string) []string {
	single := result[kind+"_entry"]
	if kind == "patched" && single == nil {
		single = result["patched_entry_before"]
	}
	var out []string
	if pyTruthy(single) {
		out = append(out, pyStr(single))
	}
	batch, _ := result[kind+"_entries"].(map[string]any)
	keys := make([]string, 0, len(batch))
	for k := range batch {
		keys = append(keys, k)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, _ := strconv.Atoi(pyStrip(keys[i]))
		b, _ := strconv.Atoi(pyStrip(keys[j]))
		return a < b
	})
	for _, k := range keys {
		out = append(out, pyStr(batch[k]))
	}
	return out
}

// Applies one staged record, a crash in the applier reported as its message with an empty result
func applyOnePending(ctx context.Context, subsystem string, rec map[string]any, opts PendingOptions) (ok bool, msg string, result map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			ok, msg, result = false, fmt.Sprint(r), map[string]any{}
		}
	}()
	payload, isMap := recordValue(rec, "payload", map[string]any{}).(map[string]any)
	if subsystem == tools.WriteApprovalMemory && opts.MemoryStore == nil {
		return false, "memory store unavailable", map[string]any{}
	}
	if !isMap {
		return false, fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(rec["payload"])), map[string]any{}
	}
	if subsystem == tools.WriteApprovalMemory {
		res, err := ApplyMemoryPending(ctx, payload, opts.MemoryStore)
		if err != nil {
			return false, osErrorText(err), map[string]any{}
		}
		result = res.Data
	} else {
		raw := opts.ApplySkill(payload)
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			return false, err.Error(), map[string]any{}
		}
		m, isObject := decoded.(map[string]any)
		if !isObject {
			return false, fmt.Sprintf("'%s' object has no attribute 'get'", pyTypeName(decoded)), map[string]any{}
		}
		result = m
	}
	errText := ""
	if v, present := result["error"]; present {
		errText = pyStr(v)
	}
	return pyTruthy(result["success"]), errText, result
}

func rejectPending(env *tools.Env, subsystem string, rest []string) (string, error) {
	if len(rest) == 0 {
		return approvalUsage(subsystem), nil
	}
	target := rest[0]
	if pyLower(target) == "all" {
		n := 0
		for _, rec := range tools.ListPending(env, subsystem) {
			id, err := recordKey(rec, "id")
			if err != nil {
				return "", err
			}
			if tools.DiscardPending(env, subsystem, pyStr(id)) {
				n++
			}
		}
		return fmt.Sprintf("Rejected %d pending %s write(s).", n, subsystem), nil
	}
	if tools.DiscardPending(env, subsystem, target) {
		return fmt.Sprintf("Rejected pending %s write '%s'.", subsystem, target), nil
	}
	return fmt.Sprintf("No pending %s write with id '%s'.", subsystem, target), nil
}

func skillPendingDiff(env *tools.Env, rest []string) (string, error) {
	if len(rest) == 0 {
		return "Usage: /skills diff <id>", nil
	}
	rec := tools.GetPending(env, tools.WriteApprovalSkills, rest[0])
	if len(rec) == 0 {
		return fmt.Sprintf("No pending skill write with id '%s'.", rest[0]), nil
	}
	id, err := recordKey(rec, "id")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# Pending skill write %s: %s\n\n", pyStr(id), pyStr(recordValue(rec, "summary", ""))) + tools.SkillPendingDiff(env, rec, nil), nil
}

// Spellings the approval toggle accepts for on and off
var approvalValues = map[string]bool{
	"on": true, "true": true, "yes": true, "1": true, "enable": true, "enabled": true,
	"off": false, "false": false, "no": false, "0": false, "disable": false, "disabled": false,
}

// Turns the subsystem approval gate on or off
func setApproval(env *tools.Env, subsystem string, rest []string, setMode func(bool) error) string {
	if len(rest) == 0 {
		return fmt.Sprintf("%s\nSet with: /%s approval <on|off>", fmtState(env, subsystem), subsystem)
	}
	arg := pyLower(pyStrip(rest[0]))
	enabled, ok := approvalValues[arg]
	if !ok {
		return fmt.Sprintf("Invalid value '%s'. Use: on or off.", arg)
	}
	state := "off"
	if enabled {
		state = "on"
	}
	if setMode == nil {
		val := "false"
		if enabled {
			val = "true"
		}
		return fmt.Sprintf("To change the %s approval gate, run:\n  icebath config set %s.write_approval %s", subsystem, subsystem, val)
	}
	if err := setMode(enabled); err != nil {
		return fmt.Sprintf("Failed to set %s.write_approval: %s", subsystem, osErrorText(err))
	}
	return fmt.Sprintf("%s.write_approval set to '%s'.", subsystem, state)
}

// Reply for a memory subcommand the review handler does not own
const memoryUnknownSubcommand = "Unknown /memory subcommand. Use: pending, approve <id>, reject <id>, approval <on|off>."

// Handles the memory slash command against the live store or a freshly loaded on disk one
func MemoryCommand(ctx context.Context, env *tools.Env, args []string, store *Store) (string, error) {
	if store == nil {
		loaded, err := LoadOnDiskStore(tools.WithEnvHome(ctx, env), env)
		if err != nil {
			return "", err
		}
		store = loaded
	}
	out, handled, err := HandlePendingSubcommand(tools.WithEnvHome(ctx, env), env, tools.WriteApprovalMemory, args, PendingOptions{
		MemoryStore: store, SetMode: ConfigModeSetter(tools.WithEnvHome(ctx, env), env, tools.WriteApprovalMemory)})
	if err != nil {
		return "", err
	}
	if !handled {
		return memoryUnknownSubcommand, nil
	}
	return out, nil
}

// Write approval commands and the gate register nothing at import
func registerWriteApprovalGate() {}
