package kanban

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const (
	ctxMaxPriorAttempts = 10
	ctxMaxComments      = 30
	ctxMaxFieldBytes    = 4 * 1024
	ctxMaxBodyBytes     = 8 * 1024
	ctxMaxCommentBytes  = 2 * 1024
)

const KanbanTerminalTimeoutGraceSeconds = 30

// Worker scoped TERMINAL_TIMEOUT override when the task max runtime exceeds the current terminal default
func workerTerminalTimeoutEnv(maxRuntimeSeconds *int64, currentTimeout *string) *string {
	if maxRuntimeSeconds == nil {
		return nil
	}
	runtime := *maxRuntimeSeconds
	if runtime <= 0 {
		return nil
	}
	desired := runtime - KanbanTerminalTimeoutGraceSeconds
	if desired < 1 {
		desired = 1
	}
	existing := int64(0)
	if currentTimeout != nil && *currentTimeout != "" {
		if n, err := pyIntOf(pystr.Strip(*currentTimeout)); err == nil {
			existing = n
		}
	}
	if existing >= desired {
		return nil
	}
	s := fmt.Sprintf("%d", desired)
	return &s
}

// Python truthiness of any decoded value including ordered objects and lists
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case *pyjson.Object:
		return x != nil && x.Len() > 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return truthy(v)
}

// Relative age such as 18h ago, empty for a missing or invalid stamp, never in the future
func relativeAge(ts any, now int64) string {
	if ts == nil {
		return ""
	}
	n, err := pyIntOf(ts)
	if err != nil {
		return ""
	}
	delta := now - n
	switch {
	case delta < 60:
		return "just now"
	case delta < 3600:
		return fmt.Sprintf("%dm ago", delta/60)
	case delta < 86400:
		return fmt.Sprintf("%dh ago", delta/3600)
	}
	return fmt.Sprintf("%dd ago", delta/86400)
}

// Stripped text truncated to limit characters with a visible ellipsis
func ctxCap(s *string, limit int) string {
	if !truthyStr(s) {
		return ""
	}
	text := pystr.Strip(*s)
	rs := []rune(text)
	if len(rs) <= limit {
		return text
	}
	return string(rs[:limit]) + fmt.Sprintf("… [truncated, %d chars omitted]", len(rs)-limit)
}

// Local date and time of a stamp plus its relative age when available
func ctxStamp(ts, now int64) string {
	disp := time.Unix(ts, 0).Local().Format("2006-01-02 15:04")
	if age := relativeAge(ts, now); age != "" {
		return disp + ", " + age
	}
	return disp
}

// Metadata line with sorted JSON, nil for empty or unserializable metadata
func ctxMetadataLine(metadata any) *string {
	if !pyTruthy(metadata) {
		return nil
	}
	text, err := pyjson.Dumps(metadata, pyjson.EnsureASCII(false), pyjson.SortKeys(true))
	if err != nil {
		return nil
	}
	capped := ctxCap(&text, ctxMaxFieldBytes)
	line := "_metadata_: `" + capped + "`"
	return &line
}

// Newest cap items with a note describing the omitted head
func ctxTail[T any](items []T, limit int, noun string) ([]T, *string) {
	omitted := len(items) - limit
	if omitted <= 0 {
		return items, nil
	}
	plural := ""
	if omitted != 1 {
		plural = "s"
	}
	note := fmt.Sprintf("_(%d earlier %s%s omitted; showing most recent %d)_", omitted, noun, plural, limit)
	return items[len(items)-limit:], &note
}

// Header lines of the worker context with assignee, status, workspace, runtime and body
func ctxHeader(lines *[]string, task *Task) {
	add := func(s string) { *lines = append(*lines, s) }
	add(fmt.Sprintf("# Kanban task %s: %s", task.ID, task.Title))
	add("")
	assignee := "(unassigned)"
	if truthyStr(task.Assignee) {
		assignee = *task.Assignee
	}
	add("Assignee: " + assignee)
	add("Status:   " + task.Status)
	if truthyStr(task.Tenant) {
		add("Tenant:   " + *task.Tenant)
	}
	workspace := "(unresolved)"
	if truthyStr(task.WorkspacePath) {
		workspace = *task.WorkspacePath
	}
	add(fmt.Sprintf("Workspace: %s @ %s", task.WorkspaceKind, workspace))
	if task.MaxRuntimeSeconds != nil {
		var current *string
		if v, ok := os.LookupEnv("TERMINAL_TIMEOUT"); ok {
			current = &v
		}
		effective := workerTerminalTimeoutEnv(task.MaxRuntimeSeconds, current)
		if !truthyStr(effective) {
			effective = current
		}
		add(fmt.Sprintf("Max runtime: %ds", *task.MaxRuntimeSeconds))
		if truthyStr(effective) {
			add(fmt.Sprintf("Terminal timeout: %ss", *effective))
		}
	}
	if truthyStr(task.BranchName) {
		add("Branch:   " + *task.BranchName)
	}
	add("")
	if truthyStr(task.Body) && pystr.Strip(*task.Body) != "" {
		add("## Body")
		add(ctxCap(task.Body, ctxMaxBodyBytes))
		add("")
	}
}

// Attachment lines with absolute on disk paths the worker file tools read directly
func ctxAttachments(lines *[]string, attachments []*Attachment) {
	if len(attachments) == 0 {
		return
	}
	*lines = append(*lines, "## Attachments",
		"Files attached to this task. Read them with the file/terminal "+
			"tools at the absolute paths below:")
	for _, att := range attachments {
		sizeKB := int64(0)
		if att.Size != 0 {
			sizeKB = (att.Size + 1023) / 1024
			if sizeKB < 1 {
				sizeKB = 1
			}
		}
		sizeStr := ""
		if sizeKB != 0 {
			sizeStr = fmt.Sprintf(", %d KB", sizeKB)
		}
		ctype := ""
		if truthyStr(att.ContentType) {
			ctype = ", " + *att.ContentType
		}
		*lines = append(*lines, fmt.Sprintf("- `%s`%s%s → `%s`", att.Filename, ctype, sizeStr, att.StoredPath))
	}
	*lines = append(*lines, "")
}

// Closed runs on the task, the newest shown in full and older ones as a one line marker
func ctxPriorAttempts(ctx context.Context, lines *[]string, conn state.SQLiteConn, taskID string, now int64) error {
	runs, err := ListRuns(ctx, conn, taskID, true, nil, nil)
	if err != nil {
		return err
	}
	var allPrior []*Run
	for _, r := range runs {
		if r.EndedAt != nil {
			allPrior = append(allPrior, r)
		}
	}
	shown, omittedNote := ctxTail(allPrior, ctxMaxPriorAttempts, "attempt")
	if len(shown) == 0 {
		return nil
	}
	firstShownIdx := len(allPrior) - len(shown) + 1
	*lines = append(*lines, "## Prior attempts on this task")
	if omittedNote != nil {
		*lines = append(*lines, *omittedNote)
	}
	for offset, run := range shown {
		profile := "(unknown)"
		if truthyStr(run.Profile) {
			profile = *run.Profile
		}
		outcome := run.Status
		if truthyStr(run.Outcome) {
			outcome = *run.Outcome
		}
		*lines = append(*lines, fmt.Sprintf("### Attempt %d — %s (%s, %s)", firstShownIdx+offset, outcome, profile, ctxStamp(run.StartedAt, now)))
		if truthyStr(run.Summary) && pystr.Strip(*run.Summary) != "" {
			*lines = append(*lines, ctxCap(run.Summary, ctxMaxFieldBytes))
		}
		if truthyStr(run.Error) && pystr.Strip(*run.Error) != "" {
			*lines = append(*lines, "_error_: "+ctxCap(run.Error, ctxMaxFieldBytes))
		}
		if metaLine := ctxMetadataLine(run.Metadata); metaLine != nil {
			*lines = append(*lines, *metaLine)
		}
		*lines = append(*lines, "")
	}
	return nil
}

// Done parent handoffs from the newest completed run, falling back to the stored task result
func ctxParentResults(ctx context.Context, lines *[]string, conn state.SQLiteConn, taskID string, now int64) error {
	parentRows, err := QueryRows(ctx, conn, "SELECT parent_id FROM task_links WHERE child_id = ? ORDER BY parent_id", taskID)
	if err != nil {
		return err
	}
	wroteHeader := false
	for _, prow := range parentRows {
		pid := cellStr(prow.Get("parent_id"))
		pt, err := GetTask(ctx, conn, pid)
		if err != nil {
			return err
		}
		if pt == nil || pt.Status != "done" {
			continue
		}
		all, err := ListRuns(ctx, conn, pid, true, nil, nil)
		if err != nil {
			return err
		}
		var runs []*Run
		for _, r := range all {
			if r.Outcome != nil && *r.Outcome == "completed" {
				runs = append(runs, r)
			}
		}
		sort.SliceStable(runs, func(i, j int) bool { return runs[i].StartedAt > runs[j].StartedAt })
		var run *Run
		if len(runs) > 0 {
			run = runs[0]
		}
		if !wroteHeader {
			*lines = append(*lines, "## Parent task results",
				"_Handoffs from upstream tasks, captured when each parent "+
					"completed (see age below). These are point-in-time "+
					"snapshots, not live state — if a result drives your "+
					"current work and it's not recent, re-verify against the "+
					"source before acting on it as current._")
			wroteHeader = true
		}
		var doneTS any
		if run != nil && run.EndedAt != nil && *run.EndedAt != 0 {
			doneTS = *run.EndedAt
		} else if pt.CompletedAt != nil && *pt.CompletedAt != 0 {
			doneTS = *pt.CompletedAt
		}
		heading := "### " + pid
		if age := relativeAge(doneTS, now); age != "" {
			heading += fmt.Sprintf(" (completed %s)", age)
		}
		*lines = append(*lines, heading)
		switch {
		case run != nil && truthyStr(run.Summary) && pystr.Strip(*run.Summary) != "":
			*lines = append(*lines, ctxCap(run.Summary, ctxMaxFieldBytes))
		case truthyStr(pt.Result):
			*lines = append(*lines, ctxCap(pt.Result, ctxMaxFieldBytes))
		default:
			*lines = append(*lines, "(no result recorded)")
		}
		if run != nil {
			if metaLine := ctxMetadataLine(run.Metadata); metaLine != nil {
				*lines = append(*lines, *metaLine)
			}
		}
		*lines = append(*lines, "")
	}
	return nil
}

// The five most recent completed runs of the assignee on other tasks
func ctxRoleHistory(ctx context.Context, lines *[]string, conn state.SQLiteConn, task *Task, now int64) error {
	if !truthyStr(task.Assignee) {
		return nil
	}
	rows, err := QueryRows(ctx, conn, "SELECT t.id, t.title, r.summary, r.ended_at "+
		"FROM task_runs r JOIN tasks t ON r.task_id = t.id "+
		"WHERE r.profile = ? AND r.task_id != ? "+
		"  AND r.outcome = 'completed' "+
		"ORDER BY r.ended_at DESC LIMIT 5", *task.Assignee, task.ID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	*lines = append(*lines, fmt.Sprintf("## Recent work by @%s", *task.Assignee))
	for _, row := range rows {
		first := firstLine(cellOptStr(row.Get("summary")), 200)
		if first == "" {
			first = "(no summary)"
		}
		ended := int64(0)
		if e := toEpoch(row.Get("ended_at")); e != nil {
			ended = *e
		}
		*lines = append(*lines, fmt.Sprintf("- %s — %s (%s): %s", cellStr(row.Get("id")), cellStr(row.Get("title")), ctxStamp(ended, now), first))
	}
	*lines = append(*lines, "")
	return nil
}

// Newest comments framed explicitly as worker comments so an author name is never read as a directive
func ctxComments(lines *[]string, comments []*Comment, now int64) {
	shown, omittedNote := ctxTail(comments, ctxMaxComments, "comment")
	if len(shown) == 0 {
		return
	}
	*lines = append(*lines, "## Comment thread")
	if omittedNote != nil {
		*lines = append(*lines, *omittedNote)
	}
	for _, c := range shown {
		safeAuthor := strings.ReplaceAll(c.Author, "`", "")
		*lines = append(*lines, fmt.Sprintf("comment from worker `%s` at %s:", safeAuthor, ctxStamp(c.CreatedAt, now)))
		body := c.Body
		*lines = append(*lines, ctxCap(&body, ctxMaxCommentBytes))
		*lines = append(*lines, "")
	}
}
