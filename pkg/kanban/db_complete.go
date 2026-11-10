package kanban

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const reviewApprovedNote = "Review approved without additional evidence."

// CompleteTaskOptions holds the keyword arguments of a completion
type CompleteTaskOptions struct {
	Result            *string
	Summary           *string
	Metadata          *pyjson.Object
	CreatedCards      []string
	ExpectedRunID     *int64
	SkipLifecycleHook bool
	Force             bool
	// Falsy non string result or summary the worker tool passed, Python truthiness and binding read these
	resultRaw, summaryRaw any
}

// Text a Python or chain or filter keeps of a completion field, nil for a falsy non string value
func completionText(text *string, raw any) *string {
	if raw != nil {
		return nil
	}
	return text
}

// Text str() gives a completion field, the substantive evidence check reading it
func completionStr(text *string, raw any) *string {
	if raw == nil {
		return text
	}
	s := pyrepr.Str(raw)
	return &s
}

// Goal turn budget stored on the task, nil when unset
func TaskGoalMaxTurns(task *Task) *int64 { return task.GoalMaxTurns }

// Python truthiness of an optional metadata dict
func truthyMeta(o *pyjson.Object) bool { return o != nil && o.Len() > 0 }

// Interface value of an optional metadata dict with nil for None
func metaAny(o *pyjson.Object) any {
	if o == nil {
		return nil
	}
	return o
}

// Python truthiness of an optional string
func truthyStr(s *string) bool { return s != nil && *s != "" }

// Stripped first line or nil when there is none
func firstLineOrNil(text *string, limit int) any {
	if line := firstLine(text, limit); line != "" {
		return line
	}
	return nil
}

// Moves a task to done with its result, refusing a live claim without proof of ownership and gating evidence first
func CompleteTask(ctx context.Context, conn state.SQLiteConn, taskID string, o CompleteTaskOptions) (bool, error) {
	now := time.Now().Unix()
	ok, err := parentsSatisfied(ctx, conn, taskID)
	if err != nil || !ok {
		return false, err
	}
	preview := completionText(o.Result, o.resultRaw)
	if truthyStr(completionText(o.Summary, o.summaryRaw)) {
		preview = o.Summary
	}
	verifiedCards, err := gateCreatedCards(ctx, conn, taskID, o.CreatedCards, preview)
	if err != nil {
		return false, err
	}
	if err := gateEmptyCompletion(ctx, conn, taskID, completionStr(o.Result, o.resultRaw), completionStr(o.Summary, o.summaryRaw)); err != nil {
		return false, err
	}
	metadata, err := mergeCompletionProseArtifacts(ctx, conn, taskID, o.Metadata, completionText(o.Summary, o.summaryRaw), completionText(o.Result, o.resultRaw))
	if err != nil {
		return false, err
	}
	handoffSummary, handoffRaw := o.Result, o.resultRaw
	if o.Summary != nil || o.summaryRaw != nil {
		handoffSummary, handoffRaw = o.Summary, o.summaryRaw
	}
	acceptance, refused, err := prepareAcceptance(ctx, conn, taskID, o.ExpectedRunID, metaAny(metadata))
	if err != nil || refused {
		return false, err
	}
	var runID *int64
	completed := false
	err = WriteTxn(ctx, conn, false, func() error {
		ok, err := parentsSatisfied(ctx, conn, taskID)
		if err != nil || !ok {
			return err
		}
		if acceptance != nil {
			recorded, err := recordAcceptance(ctx, conn, taskID, acceptance)
			if err != nil || !recorded {
				return err
			}
		}
		trow, err := QueryOne(ctx, conn, "SELECT status, claim_lock, worker_pid, worker_started_at FROM tasks WHERE id = ?", taskID)
		if err != nil {
			return err
		}
		var priorStatus any
		if trow != nil {
			priorStatus = trow.Get("status")
		}
		if o.ExpectedRunID == nil && !o.Force && trow != nil && claimIsLive(trow) {
			return &LiveClaimError{taskID}
		}
		sql := `
                UPDATE tasks
                   SET status       = 'done',
                       result       = ?,
                       completed_at = ?,
                       claim_lock   = NULL,
                       claim_expires= NULL,
                       worker_pid   = NULL,
                       block_kind   = NULL,
                       block_recurrences = 0
                 WHERE id = ?
                   AND status IN ('running', 'ready', 'blocked', 'review')
                `
		if o.resultRaw != nil {
			if _, err := sqliteBind(o.resultRaw, 1); err != nil {
				return err
			}
		}
		params := []any{optStrValue(o.Result), now, taskID}
		if o.ExpectedRunID != nil {
			if err := runIDBindable(o.ExpectedRunID); err != nil {
				return err
			}
			sql += " AND current_run_id = ?"
			params = append(params, *o.ExpectedRunID)
		}
		res, err := conn.ExecContext(ctx, sql, params...)
		if err != nil {
			return err
		}
		if rowsAffected(res) != 1 {
			return nil
		}
		if metadata != nil {
			if _, err := stageCompletionArtifacts(ctx, conn, taskID, metadata, now, "kanban_complete"); err != nil {
				return err
			}
		}
		if handoffRaw != nil {
			active, err := currentRunID(ctx, conn, taskID)
			if err != nil {
				return err
			}
			if _, err := sqliteBind(handoffRaw, 3); active != nil && err != nil {
				return err
			}
		}
		if runID, err = endRun(ctx, conn, taskID, "completed", handoffSummary, nil, metaAny(metadata), "done"); err != nil {
			return err
		}
		fromReview := priorStatus == "review"
		handoffTruthy := truthyStr(completionText(handoffSummary, handoffRaw))
		if runID == nil && (truthyStr(completionText(o.Summary, o.summaryRaw)) || truthyMeta(metadata) || truthyStr(completionText(o.Result, o.resultRaw)) || fromReview) {
			synthSummary, synthMetadata := handoffSummary, metaAny(metadata)
			if fromReview && !handoffTruthy && !truthyMeta(metadata) {
				note := reviewApprovedNote
				synthSummary = &note
				synthMetadata = payload("source_status", "review", "approval", "manual")
			} else if handoffRaw != nil {
				if _, err := sqliteBind(handoffRaw, 6); err != nil {
					return err
				}
			}
			id, err := synthesizeEndedRun(ctx, conn, taskID, "completed", synthSummary, nil, synthMetadata, runProfile{})
			if err != nil {
				return err
			}
			runID = &id
		}
		eventSummary := completionText(handoffSummary, handoffRaw)
		if fromReview && !handoffTruthy {
			note := reviewApprovedNote
			eventSummary = &note
		}
		completed = true
		return appendEvent(ctx, conn, taskID, "completed", completedEventPayload(completionText(o.Result, o.resultRaw), eventSummary, verifiedCards, metaAny(metadata)), runID)
	})
	if err != nil || !completed {
		return false, err
	}
	if err := flagPhantomProseRefs(ctx, conn, taskID, runID, completionText(o.Summary, o.summaryRaw), completionText(o.Result, o.resultRaw), verifiedCards); err != nil {
		return false, err
	}
	if err := clearFailureCounter(ctx, conn, taskID); err != nil {
		return false, err
	}
	if _, err := RecomputeReady(ctx, conn, nil); err != nil {
		return false, err
	}
	cleanupWorkspace(ctx, conn, taskID)
	doneTask, err := GetTask(ctx, conn, taskID)
	if err != nil {
		return false, err
	}
	if !o.SkipLifecycleHook {
		hookSummary := optStrValue(handoffSummary)
		if handoffRaw != nil {
			hookSummary = handoffRaw
		}
		fireTaskHook(ctx, "kanban_task_completed", doneTask, taskID, runID, map[string]any{"summary": hookSummary})
	}
	return true, nil
}

// Verifies claimed created cards before the main transaction, a phantom id recorded then raised
func gateCreatedCards(ctx context.Context, conn state.SQLiteConn, taskID string, createdCards []string, previewText *string) ([]string, error) {
	if len(createdCards) == 0 {
		return nil, nil
	}
	verified, phantom, err := verifyCreatedCards(ctx, conn, taskID, createdCards)
	if err != nil {
		return nil, err
	}
	if len(phantom) > 0 {
		err := WriteTxn(ctx, conn, false, func() error {
			return appendEvent(ctx, conn, taskID, "completion_blocked_hallucination",
				payload("phantom_cards", phantom, "verified_cards", verified, "summary_preview", firstLineOrNil(previewText, 200)), nil)
		})
		if err != nil {
			return nil, err
		}
		return nil, &HallucinatedCardsError{Phantom: phantom, CompletingTaskID: taskID}
	}
	return verified, nil
}

// Reports whether a value is a string with non whitespace content
func substantiveText(value any) bool {
	if value == nil {
		return false
	}
	return pystr.Strip(cellStr(value)) != ""
}

// Refuses a completion that would leave a non review card without evidence, recording the refusal first
func gateEmptyCompletion(ctx context.Context, conn state.SQLiteConn, taskID string, result, summary *string) error {
	row, err := QueryOne(ctx, conn, "SELECT status, result FROM tasks WHERE id = ?", taskID)
	if err != nil || row == nil {
		return err
	}
	if row.Get("status") == "review" {
		return nil
	}
	if substantiveText(optStrValue(result)) || substantiveText(optStrValue(summary)) || substantiveText(row.Get("result")) {
		return nil
	}
	err = WriteTxn(ctx, conn, false, func() error {
		return appendEvent(ctx, conn, taskID, "completion_blocked_empty_result",
			payload("result_preview", firstLineOrNil(result, 200), "summary_preview", firstLineOrNil(summary, 200)), nil)
	})
	if err != nil {
		return err
	}
	return &EmptyCompletionError{taskID}
}

// Copies scratch artifacts into the attachments dir and records each as an attachment row, returning the copies
func stageCompletionArtifacts(ctx context.Context, conn state.SQLiteConn, taskID string, metadata *pyjson.Object, now int64, uploadedBy string) ([]string, error) {
	if err := persistScratchCompletionArtifacts(ctx, conn, taskID, metadata); err != nil {
		return nil, err
	}
	var staged []string
	if raw, ok := metadata.Pop("_staged_artifacts"); ok {
		for _, item := range jiter(raw) {
			staged = append(staged, pypath.PathStr(cellStr(item)))
		}
	}
	for _, p := range staged {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if err := insertCompletionAttachment(ctx, conn, taskID, pypath.PathName(p), p, st.Size(), now, uploadedBy); err != nil {
			return nil, err
		}
	}
	return staged, nil
}

// Non blank string paths declared under the artifacts key of a metadata dict
func cleanedArtifactPaths(metadata any) []string {
	if !isDict(metadata) {
		return nil
	}
	raw, _ := dictGet(metadata, "artifacts")
	if _, isList := raw.([]any); !isList {
		return nil
	}
	var out []string
	for _, p := range raw.([]any) {
		s, ok := p.(string)
		if !ok {
			continue
		}
		if stripped := pystr.Strip(s); stripped != "" {
			out = append(out, stripped)
		}
	}
	return out
}

// Completed event payload with the first summary line, verified cards and promoted artifact paths
func completedEventPayload(result, eventSummary *string, verifiedCards []string, metadata any) *pyjson.Object {
	resultLen := 0
	if truthyStr(result) {
		resultLen = utf8.RuneCountInString(*result)
	}
	body := payload("result_len", resultLen, "summary", firstLineOrNil(eventSummary, 400))
	if len(verifiedCards) > 0 {
		body.Set("verified_cards", verifiedCards)
	}
	if isDict(metadata) {
		if cleaned := cleanedArtifactPaths(metadata); len(cleaned) > 0 {
			body.Set("artifacts", cleaned)
		}
	}
	return body
}

// Advisory post commit scan of summary and result for unresolvable task id references
func flagPhantomProseRefs(ctx context.Context, conn state.SQLiteConn, taskID string, runID *int64, summary, result *string, verifiedCards []string) error {
	var parts []string
	for _, p := range []*string{summary, result} {
		if truthyStr(p) {
			parts = append(parts, *p)
		}
	}
	scanText := strings.Join(parts, " ")
	if scanText == "" {
		return nil
	}
	found, err := scanProseForPhantomIDs(ctx, conn, scanText)
	if err != nil {
		return err
	}
	verified := map[string]bool{}
	for _, c := range verifiedCards {
		verified[c] = true
	}
	var phantomRefs []string
	for _, p := range found {
		if !verified[p] {
			phantomRefs = append(phantomRefs, p)
		}
	}
	if len(phantomRefs) == 0 {
		return nil
	}
	return WriteTxn(ctx, conn, false, func() error {
		return appendEvent(ctx, conn, taskID, "suspected_hallucinated_references",
			payload("phantom_refs", phantomRefs, "source", "completion_summary"), runID)
	})
}

// Adds absolute scratch workspace paths named in prose to the artifacts of the metadata before cleanup can erase them
func mergeCompletionProseArtifacts(ctx context.Context, conn state.SQLiteConn, taskID string, metadata *pyjson.Object, summary, result *string) (*pyjson.Object, error) {
	workspace, err := scratchWorkspace(ctx, conn, taskID)
	if err != nil || workspace == nil {
		return metadata, err
	}
	managed, err := isManagedScratchPath(*workspace)
	if err != nil || !managed {
		return metadata, err
	}
	var parts []string
	for _, p := range []*string{summary, result} {
		if truthyStr(p) {
			parts = append(parts, *p)
		}
	}
	text := strings.Join(parts, "\n")
	if text == "" {
		return metadata, nil
	}
	re, err := regexp2.Compile(regexp2.Escape(*workspace)+"(?:[/\\\\][^\\s`\"'<>]+)", regexp2.None)
	if err != nil {
		return nil, err
	}
	var discovered []string
	for m, err := re.FindStringMatch(text); m != nil && err == nil; m, err = re.FindNextMatch(m) {
		raw := pystr.RstripChars(m.String(), ".,;:!?)]}")
		if isFile(raw) {
			discovered = append(discovered, pypath.PathStr(raw))
		}
	}
	if len(discovered) == 0 {
		return metadata, nil
	}
	updated := pyjson.NewObject()
	if metadata != nil {
		updated = metadata.Copy()
	}
	var merged []any
	if existing, _ := updated.Get("artifacts"); existing != nil {
		if list, ok := existing.([]any); ok {
			merged = append(merged, list...)
		}
	}
	seen := map[string]bool{}
	for _, p := range merged {
		seen[pyrepr.Str(p)] = true
	}
	for _, p := range discovered {
		if !seen[p] {
			merged = append(merged, p)
			seen[p] = true
		}
	}
	if merged == nil {
		merged = []any{}
	}
	updated.Set("artifacts", merged)
	return updated, nil
}

// Copies declared scratch workspace artifacts into the task attachments dir before cleanup removes them
func persistScratchCompletionArtifacts(ctx context.Context, conn state.SQLiteConn, taskID string, metadata *pyjson.Object) error {
	rawArtifacts, _ := metadata.Get("artifacts")
	items, isList := rawArtifacts.([]any)
	if !isList {
		return nil
	}
	workspace, err := scratchWorkspace(ctx, conn, taskID)
	if err != nil || workspace == nil {
		return err
	}
	managed, board, err := managedScratchPathInfo(*workspace)
	if err != nil || !managed {
		return err
	}
	workspaceRoot, err := pypath.Resolve(*workspace)
	if err != nil {
		return nil
	}
	attachmentDir, err := TaskAttachmentsDir(ctx, taskID, board)
	if err != nil {
		return err
	}
	var persisted []string
	used := map[string]bool{}
	changed := false
	discardCopies := func() { discardStagedCopies(used, attachmentDir) }
	for _, item := range items {
		artifact := ""
		if s, ok := item.(string); ok {
			artifact = pystr.Strip(s)
		}
		if artifact == "" {
			continue
		}
		src, err := pypath.PathExpanduser(artifact)
		if err != nil {
			return err
		}
		resolvedSrc, err := pypath.Resolve(src)
		if err != nil {
			persisted = append(persisted, artifact)
			continue
		}
		if _, ok := pypath.PathRelativeTo(resolvedSrc, workspaceRoot); !ok {
			persisted = append(persisted, artifact)
			continue
		}
		problem := ""
		if !isFile(src) {
			problem = fmt.Sprintf("declared scratch artifact is unavailable or not a regular file: %s", artifact)
		} else {
			st, err := os.Stat(resolvedSrc)
			if err != nil {
				return err
			}
			if st.Size() > KanbanAttachmentMaxBytes {
				problem = fmt.Sprintf("declared scratch artifact exceeds the %d-byte limit: %s", KanbanAttachmentMaxBytes, artifact)
			}
		}
		if problem != "" {
			discardCopies()
			return &ArtifactPreservationError{problem}
		}
		dest := ""
		err = os.MkdirAll(attachmentDir, 0o777)
		if err == nil {
			dest = uniqueAttachmentPath(attachmentDir, pypath.PathName(resolvedSrc), used)
			err = copyCapped(resolvedSrc, dest, artifact)
		}
		if err != nil {
			if dest != "" {
				_ = os.Remove(dest)
			}
			discardCopies()
			if _, preservation := err.(*ArtifactPreservationError); preservation {
				return err
			}
			return &ArtifactPreservationError{fmt.Sprintf("could not preserve declared scratch artifact %s: %s", artifact, excText(err))}
		}
		used[dest] = true
		resolvedDest, err := pypath.Resolve(dest)
		if err != nil {
			return err
		}
		persisted = append(persisted, resolvedDest)
		changed = true
	}
	if !changed {
		return nil
	}
	dirResolved, err := pypath.Resolve(attachmentDir)
	if err != nil {
		return err
	}
	artifacts := make([]any, 0, len(persisted))
	staged := make([]any, 0, len(persisted))
	for _, p := range persisted {
		artifacts = append(artifacts, p)
		if strings.HasPrefix(p, dirResolved) {
			staged = append(staged, p)
		}
	}
	metadata.Set("artifacts", artifacts)
	metadata.Set("_staged_artifacts", staged)
	return nil
}

// Removes staged attachment copies whose rows never committed and the directory when empty
func discardStagedCopies(copies map[string]bool, attachmentDir string) {
	for copied := range copies {
		_ = os.Remove(copied)
	}
	_ = os.Remove(attachmentDir)
}

// Removes a list of staged copies and their directory when empty
func discardStagedCopyList(copies []string, attachmentDir string) {
	set := map[string]bool{}
	for _, c := range copies {
		set[c] = true
	}
	discardStagedCopies(set, attachmentDir)
}

// Chunked copy that aborts when the source grows past the attachment cap mid copy
func copyCapped(src, dest, artifact string) error {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer source.Close()
	destination, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	defer destination.Close()
	buf := make([]byte, 1024*1024)
	copied := int64(0)
	for {
		n, err := source.Read(buf)
		if n > 0 {
			copied += int64(n)
			if copied > KanbanAttachmentMaxBytes {
				return &ArtifactPreservationError{fmt.Sprintf("declared scratch artifact grew beyond the size limit: %s", artifact)}
			}
			if _, werr := destination.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Records a worker produced artifact in the attachment table with its attached event
func insertCompletionAttachment(ctx context.Context, conn state.SQLiteConn, taskID, filename, storedPath string, size, createdAt int64, uploadedBy string) error {
	if _, err := conn.ExecContext(ctx, "INSERT INTO task_attachments "+
		"(task_id, filename, stored_path, content_type, size, uploaded_by, created_at) "+
		"VALUES (?, ?, ?, NULL, ?, ?, ?)", taskID, filename, storedPath, size, uploadedBy, createdAt); err != nil {
		return err
	}
	return appendEvent(ctx, conn, taskID, "attached", payload("filename", filename, "size", size, "by", uploadedBy), nil)
}

// Non conflicting path under the directory for a filename, numbering the stem past used or existing names
func uniqueAttachmentPath(directory, filename string, used map[string]bool) string {
	safeName := pypath.PathName(filename)
	if safeName == "" {
		safeName = "artifact"
	}
	stem, suffix := pypath.PathStem(safeName), pypath.PathSuffix(safeName)
	if stem == "" {
		stem = "artifact"
	}
	candidate := pypath.PathJoin(directory, safeName)
	for idx := 1; used[candidate] || pathExists(candidate); idx++ {
		candidate = pypath.PathJoin(directory, fmt.Sprintf("%s_%d%s", stem, idx, suffix))
	}
	return candidate
}
