package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Rollback result keys of files the safe restore left alone, with their reply line key
var rollbackSkipLines = [][2]string{{"skipped_user_edits", "gateway.rollback.kept_user_edits"},
	{"skipped_oversize", "gateway.rollback.kept_oversize"}, {"failed_deletes", "gateway.rollback.failed_deletes"}}

// Diff argument to its diff mode, unknown arguments leaving the mode unchanged
var diffModeByArg = map[string]string{"staged": "staged", "--staged": "staged", "cached": "staged", "--cached": "staged",
	"all": "all", "--all": "all", "head": "all", "session": "session"}

// Lists filesystem checkpoints or restores one by number or hash, safely unless all or force is given
func (r *GatewayRunner) handleRollbackCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	mgr, err := r.checkpointManager(ctx, event.Source)
	if err != nil {
		return "", err
	}
	if mgr == nil {
		return i18n.T(ctx, "gateway.rollback.not_enabled", "", nil), nil
	}
	cwd, err := terminalCwd(ctx)
	if err != nil {
		return "", err
	}
	tokens := pystr.SplitWhitespace(pystr.Strip(event.GetCommandArgs()), -1)
	restoreAll := false
	var kept []string
	for _, tok := range tokens {
		if low := pystr.Lower(tok); low == "--all" || low == "--force" {
			restoreAll = true
		} else {
			kept = append(kept, tok)
		}
	}
	arg := strings.Join(kept, " ")
	reason := mgr.UnsupportedBackendReason("")
	if reason != "" && arg != "" {
		return reason, nil
	}
	checkpoints := mgr.ListCheckpoints(cwd)
	if arg == "" {
		listing := tools.FormatCheckpointList(checkpoints, cwd)
		if reason != "" {
			return reason + "\n" + listing, nil
		}
		return listing, nil
	}
	if len(checkpoints) == 0 {
		return i18n.T(ctx, "gateway.rollback.none_found", "", map[string]any{"cwd": cwd}), nil
	}
	targetHash := arg
	if n, ok := pystr.IntText(arg); ok {
		idx := n.Int64() - 1
		if !n.IsInt64() || idx < 0 || idx >= int64(len(checkpoints)) {
			return i18n.T(ctx, "gateway.rollback.invalid_number", "", map[string]any{"max": len(checkpoints)}), nil
		}
		targetHash = checkpoints[idx].Hash
	}
	result := mgr.Restore(cwd, targetHash, "", !restoreAll)
	if !pyrepr.Truthy(result["success"]) {
		return i18n.T(ctx, "gateway.rollback.restore_failed", "", map[string]any{"error": result["error"]}), nil
	}
	msg := i18n.T(ctx, "gateway.rollback.restored", "", map[string]any{"hash": result["restored_to"], "reason": result["reason"]})
	for _, line := range rollbackSkipLines {
		files := stringItems(result[line[0]])
		if len(files) == 0 {
			continue
		}
		more := ""
		shown := files
		if len(files) > 5 {
			more = fmt.Sprintf(" (+%d)", len(files)-5)
			shown = files[:5]
		}
		msg += "\n" + i18n.T(ctx, line[1], "", map[string]any{"files": strings.Join(shown, ", ") + more})
	}
	return msg, nil
}

// Git changes of the working directory or the session checkpoint baseline, truncated for chat
func (r *GatewayRunner) handleDiffCommand(ctx context.Context, event *gateway.MessageEvent) (string, error) {
	var args []string
	for _, a := range pystr.SplitWhitespace(pystr.Strip(event.GetCommandArgs()), -1) {
		args = append(args, pystr.Lower(a))
	}
	statOnly, mode := false, "working"
	for _, low := range args {
		if low == "--stat" || low == "stat" {
			statOnly = true
		}
		if m, ok := diffModeByArg[low]; ok {
			mode = m
		}
	}
	cwd, err := terminalCwd(ctx)
	if err != nil {
		return "", err
	}
	var result map[string]any
	if mode == "session" {
		mgr, err := r.checkpointManager(ctx, event.Source)
		if err != nil {
			return "", err
		}
		if mgr == nil {
			return i18n.T(ctx, "gateway.diff.not_enabled", "", nil), nil
		}
		if reason := mgr.UnsupportedBackendReason(""); reason != "" {
			return reason, nil
		}
		result = mgr.SessionDiff(cwd)
	} else {
		d := tools.CollectWorkingDiff(cwd, mode, nil)
		result = map[string]any{"success": d.Success, "stat": d.Stat, "diff": d.Diff, "untracked": d.Untracked, "empty": d.Empty}
		if d.Error != "" {
			result["error"] = d.Error
		}
	}
	if !pyrepr.Truthy(result["success"]) {
		errText := result["error"]
		if !pyrepr.Truthy(errText) {
			errText = i18n.T(ctx, "gateway.diff.err_default", "", nil)
		}
		return i18n.T(ctx, "gateway.diff.failed", "", map[string]any{"error": errText}), nil
	}
	return renderDiffResult(ctx, result, statOnly), nil
}

// Stat block, untracked list and fenced truncated diff of a working or session diff result
func renderDiffResult(ctx context.Context, result map[string]any, statOnly bool) string {
	stat, _ := result["stat"].(string)
	diff, _ := result["diff"].(string)
	untracked := stringItems(result["untracked"])
	if pyrepr.Truthy(result["empty"]) || (stat == "" && diff == "" && len(untracked) == 0) {
		return i18n.T(ctx, "gateway.diff.no_changes", "", nil)
	}
	var out []string
	if stat != "" {
		out = append(out, "```\n"+stat+"\n```")
	}
	if len(untracked) > 0 {
		shownItems := untracked
		if len(shownItems) > 15 {
			shownItems = shownItems[:15]
		}
		lines := make([]string, len(shownItems))
		for i, rel := range shownItems {
			lines[i] = "+ " + rel
		}
		more := ""
		if len(untracked) > 15 {
			more = i18n.T(ctx, "gateway.diff.more_untracked", "", map[string]any{"count": len(untracked) - 15})
		}
		out = append(out, i18n.T(ctx, "gateway.diff.untracked_label", "", nil)+"\n```\n"+strings.Join(lines, "\n")+more+"\n```")
	}
	if !statOnly && diff != "" {
		out = append(out, fencedTruncatedDiff(ctx, diff, 60, 3000))
	}
	return strings.Join(out, "\n\n")
}

// Diff body fenced and truncated to the line and character limits with a note when cut
func fencedTruncatedDiff(ctx context.Context, diff string, maxLines, maxChars int) string {
	diffLines := pystr.Splitlines(diff, false)
	truncated := len(diffLines) > maxLines
	if truncated {
		diff = strings.Join(diffLines[:maxLines], "\n")
	}
	if pystr.Len(diff) > maxChars {
		diff = pystr.Slice(diff, 0, maxChars)
		truncated = true
	}
	note := ""
	if truncated {
		note = i18n.T(ctx, "gateway.diff.truncated_note", "", map[string]any{"lines": len(diffLines)})
	}
	return "```diff\n" + diff + note + "\n```"
}
