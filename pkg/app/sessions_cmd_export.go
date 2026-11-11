package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/state"
)

// Number of a filter dict value as the store binds it, nil for None
func filterNumber(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case float64:
		f = n
	default:
		return nil
	}
	return &f
}

// Store keyword arguments of a filter dict, the retention bound, the source and the remaining filters
func pruneKwargs(filters map[string]any) (*float64, string, state.PruneFilters) {
	text := func(key string) string {
		s, _ := filters[key].(string)
		return s
	}
	f := state.PruneFilters{
		IncludePinned: pyrepr.Truthy(filters["include_pinned"]), LineageTipsOnly: pyrepr.Truthy(filters["lineage_tips_only"]),
		IncludeOpen:      pyrepr.Truthy(filters["include_open"]),
		LastActiveBefore: filterNumber(filters["last_active_before"]), LastActiveAfter: filterNumber(filters["last_active_after"]),
		StartedBefore: filterNumber(filters["started_before"]), StartedAfter: filterNumber(filters["started_after"]),
		TitleLike: text("title_like"), EndReason: text("end_reason"), CwdPrefix: text("cwd_prefix"),
		MinMessages: filterNumber(filters["min_messages"]), MaxMessages: filterNumber(filters["max_messages"]),
		ModelLike: text("model_like"), Provider: text("provider"), UserID: text("user_id"), ChatID: text("chat_id"),
		ChatType: text("chat_type"), BranchLike: text("branch_like"),
		MinTokens: filterNumber(filters["min_tokens"]), MaxTokens: filterNumber(filters["max_tokens"]),
		MinCost: filterNumber(filters["min_cost"]), MaxCost: filterNumber(filters["max_cost"]),
		MinToolCalls: filterNumber(filters["min_tool_calls"]), MaxToolCalls: filterNumber(filters["max_tool_calls"]),
	}
	if archived, ok := filters["archived"].(bool); ok {
		f.Archived = &archived
	}
	return filterNumber(filters["older_than_days"]), text("source"), f
}

// Messages of an exported session or segment as a list of rows, empty when unset
func exportMessages(segment *pyjson.Object) []*pyjson.Object {
	switch list := rowGet(segment, "messages").(type) {
	case []*pyjson.Object:
		return list
	case []any:
		out := make([]*pyjson.Object, 0, len(list))
		for _, item := range list {
			if row, ok := item.(*pyjson.Object); ok {
				out = append(out, row)
			}
		}
		return out
	}
	return nil
}

// Outcome of collecting the sessions of a flat export, stop carrying the exit code of a preview or an error
type exportCollection struct {
	sessions []*pyjson.Object
	code     int
	stop     bool
}

// Exports sessions as jsonl, html, a trace, a prompt view or one markdown file per session
func cmdExport(r *sessionsRun, db *state.SessionDB) (int, error) {
	var filters map[string]any
	if anyFilterArgs(r.ns) {
		built, err := buildPruneFilters(r.ns)
		if err != nil {
			r.printf("Error: %s", sessionsExcText(err))
			return 1, nil
		}
		filters = built
		filters["archived"] = nil
		filters["include_pinned"] = true
	}
	sessionID, format := nsText(r.ns, "session_id"), nsText(r.ns, "format")
	if sessionID != "" {
		resolved, err := db.ResolveSessionID(sessionID)
		if err != nil {
			return 1, err
		}
		if resolved == "" {
			return notFound(r, sessionID), nil
		}
	}
	redact := func(data *pyjson.Object) *pyjson.Object {
		if !nsTruthy(r.ns, "redact") || data == nil {
			return data
		}
		return memory.RedactSessionData(data)
	}
	shown := state.IsSaveTranscriptFormat(format) || nsTruthy(r.ns, "only")
	includeCompacted, includeInactive := state.ExportProjection(shown)
	tooLarge := func(sessionIDs []string, bare bool) (bool, error) {
		if shown {
			return false, nil
		}
		limit := state.ResolvedMaxExportMessages(r.ctx)
		if limit == 0 {
			return false, nil
		}
		if bare {
			rows, err := db.SearchSessions(r.ctx, state.SearchSessionsOptions{Limit: 100000})
			if err != nil {
				return false, err
			}
			for _, s := range rows {
				sessionIDs = append(sessionIDs, rowText(s, "id"))
			}
		}
		err := db.AssertExportsSafe(r.ctx, sessionIDs, &limit)
		var large *state.SessionExportTooLargeError
		if errors.As(err, &large) {
			r.printf("Error: %s", large.Error())
			return true, nil
		}
		return false, err
	}
	one := func(id string) (*pyjson.Object, error) {
		data, err := db.ExportSession(r.ctx, id, includeCompacted, includeInactive)
		return redact(data), err
	}
	collect := func() (exportCollection, error) {
		failed := exportCollection{code: 1, stop: true}
		if sessionID != "" {
			resolved, err := db.ResolveSessionID(sessionID)
			if err != nil {
				return failed, err
			}
			var data *pyjson.Object
			if resolved != "" {
				if large, err := tooLarge([]string{resolved}, false); err != nil || large {
					return failed, err
				}
				if data, err = one(resolved); err != nil {
					return failed, err
				}
			}
			if data == nil || data.Len() == 0 {
				return exportCollection{code: notFound(r, sessionID), stop: true}, nil
			}
			return exportCollection{sessions: []*pyjson.Object{data}}, nil
		}
		if len(filters) > 0 {
			days, source, f := pruneKwargs(filters)
			candidates, err := db.ListPruneCandidates(r.ctx, days, source, false, f)
			if err != nil {
				return failed, err
			}
			if nsTruthy(r.ns, "dry_run") {
				printDryRunPreview(r, candidates, filters)
				return exportCollection{stop: true}, nil
			}
			ids := make([]string, 0, len(candidates))
			for _, row := range candidates {
				ids = append(ids, rowText(row, "id"))
			}
			if large, err := tooLarge(ids, false); err != nil || large {
				return failed, err
			}
			sessions := []*pyjson.Object{}
			for _, id := range ids {
				s, err := one(id)
				if err != nil {
					return failed, err
				}
				if s != nil && s.Len() > 0 {
					sessions = append(sessions, s)
				}
			}
			return exportCollection{sessions: sessions}, nil
		}
		if nsTruthy(r.ns, "dry_run") {
			r.print("--dry-run requires at least one filter.")
			return failed, nil
		}
		if large, err := tooLarge(nil, true); err != nil || large {
			return failed, err
		}
		all, err := db.ExportAll(r.ctx, "", includeCompacted, includeInactive)
		if err != nil {
			return failed, err
		}
		sessions := make([]*pyjson.Object, 0, len(all))
		for _, s := range all {
			sessions = append(sessions, redact(s))
		}
		return exportCollection{sessions: sessions}, nil
	}
	if nsTruthy(r.ns, "only") {
		return exportFlat(r, "only", collect)
	}
	if format == "trace" {
		return exportTrace(r, db, filters)
	}
	if _, flat := flatExporters[format]; flat {
		return exportFlat(r, format, collect)
	}
	return exportMarkdown(r, db, filters, redact)
}

// Plural suffix of a count
func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Prompt view of the sessions, one record per line for jsonl or headed sections for markdown
func renderOnly(r *sessionsRun, sessions []*pyjson.Object) (string, string, error) {
	format, only := "jsonl", r.ns["only"]
	if nsText(r.ns, "format") == "md" {
		format = "markdown"
	}
	rendered, err := state.RenderSessionsExport(sessions, format, only)
	if err != nil {
		return "", "", err
	}
	count, noun, err := state.ExportRecordCount(sessions, only)
	if err != nil {
		return "", "", err
	}
	return rendered, fmt.Sprintf("Exported %d %s%s to %s", count, noun, pluralS(count), nsText(r.ns, "output")), nil
}

// One self contained HTML file, a single session or several behind a sidebar
func renderHTML(r *sessionsRun, sessions []*pyjson.Object) (string, string, error) {
	single := len(sessions) == 1
	noun := "sessions"
	generate := func() (string, error) { return state.GenerateMultiSessionHTMLExport(sessions) }
	if single {
		noun = "session"
		generate = func() (string, error) { return state.GenerateHTMLExport(sessions[0]) }
	}
	content, err := generate()
	if err != nil {
		return "", "", err
	}
	return content, fmt.Sprintf("Exported %d %s to %s (HTML)", len(sessions), noun, nsText(r.ns, "output")), nil
}

// One JSON object per session per line, non ASCII text kept as written
func renderJSONL(r *sessionsRun, sessions []*pyjson.Object) (string, string, error) {
	lines := ""
	for _, s := range sessions {
		text, err := state.DumpsExportedSession(s)
		if err != nil {
			return "", "", err
		}
		lines += text + "\n"
	}
	noun := "sessions"
	if nsTruthy(r.ns, "session_id") {
		noun = "session"
	}
	return lines, fmt.Sprintf("Exported %d %s to %s", len(sessions), noun, nsText(r.ns, "output")), nil
}

// A single file export kind with the usage error of an unusable output and format pairing
type flatExporter struct {
	unusable func(ns map[string]any) bool
	message  string
	render   func(r *sessionsRun, sessions []*pyjson.Object) (string, string, error)
}

var flatExporters = map[string]flatExporter{
	"only": {func(ns map[string]any) bool {
		f := nsText(ns, "format")
		return f != "jsonl" && f != "md"
	}, "--only user-prompts supports --format jsonl or md.", renderOnly},
	"html": {func(ns map[string]any) bool {
		o := nsText(ns, "output")
		return o == "" || o == "-"
	}, "HTML export requires an output file path.", renderHTML},
	"jsonl": {func(ns map[string]any) bool { return nsText(ns, "output") == "" },
		"JSONL export requires an output path (use - for stdout).", renderJSONL},
}

// Single file export validating the output target, collecting the sessions, rendering and writing them
func exportFlat(r *sessionsRun, kind string, collect func() (exportCollection, error)) (int, error) {
	exporter := flatExporters[kind]
	if exporter.unusable(r.ns) {
		r.print(exporter.message)
		return 1, nil
	}
	collected, err := collect()
	if err != nil {
		return 1, err
	}
	if collected.stop {
		return collected.code, nil
	}
	sessions, format := collected.sessions, nsText(r.ns, "format")
	name := "icebath_sessions." + format
	if len(sessions) == 1 {
		id := ""
		if v, ok := sessions[0].Get("id"); ok {
			id = pyrepr.Str(v)
		}
		name = state.DefaultSaveFilename(id, format)
	}
	output, err := outputFileInDir(nsText(r.ns, "output"), name)
	if err != nil {
		return 1, err
	}
	if _, given := r.ns["output"].(string); given {
		r.ns["output"] = output
	}
	text, summary, err := exporter.render(r, sessions)
	if err != nil {
		return 1, err
	}
	if err := writeOutput(r, output, text, summary); err != nil {
		return 1, err
	}
	return 0, nil
}

// Claude Code JSONL trace export to a local file or an upload, redacted unless opted out
func exportTrace(r *sessionsRun, db *state.SessionDB, filters map[string]any) (int, error) {
	sessionID := nsText(r.ns, "session_id")
	if sessionID == "" && len(filters) == 0 {
		one := 1
		rows, err := db.ListSessionsRich(r.ctx, state.ListSessionsRichOptions{Limit: &one, OrderByLastActive: true})
		if err != nil {
			return 1, err
		}
		if len(rows) > 0 {
			sessionID = rowText(rows[0], "id")
		}
		if sessionID == "" {
			r.print("No session found to export. Pass --session-id.")
			return 1, nil
		}
	}
	redactTrace := !nsTruthy(r.ns, "no_redact")
	if nsTruthy(r.ns, "upload") {
		if sessionID == "" {
			r.print("--upload exports one session: pass --session-id (or drop filters to use the most recent).")
			return 1, nil
		}
		resolved, err := db.ResolveSessionID(sessionID)
		if err != nil {
			return 1, err
		}
		if err := db.Close(); err != nil {
			return 1, err
		}
		status, err := uploadSessionTrace(r.ctx, resolved, "", redactTrace, !nsTruthy(r.ns, "public"))
		if err != nil {
			return 1, err
		}
		r.print(status)
		return 0, nil
	}
	var ids []string
	if sessionID != "" {
		resolved, err := db.ResolveSessionID(sessionID)
		if err != nil {
			return 1, err
		}
		ids = []string{resolved}
	} else {
		days, source, f := pruneKwargs(filters)
		candidates, err := db.ListPruneCandidates(r.ctx, days, source, false, f)
		if err != nil {
			return 1, err
		}
		if nsTruthy(r.ns, "dry_run") {
			printDryRunPreview(r, candidates, filters)
			return 0, nil
		}
		for _, row := range candidates {
			ids = append(ids, rowText(row, "id"))
		}
	}
	renderTrace := func(sid string) (string, error) {
		meta, err := db.GetSession(r.ctx, sid)
		if err != nil {
			return "", err
		}
		messages, err := db.GetMessagesAsConversation(r.ctx, sid, state.ConversationOptions{})
		if err != nil || len(messages) == 0 {
			return "", err
		}
		return buildTraceJSONL(r.ctx, messages, sid, rowText(meta, "model"), "", redactTrace)
	}
	code, err := exportTraceFiles(r, ids, renderTrace)
	var redaction *traceRedactionError
	if errors.As(err, &redaction) {
		r.print("Redaction failed; refusing to export unredacted trace content.")
		return 1, nil
	}
	return code, err
}

// Writes one trace to the output or a trace file per session into the export directory
func exportTraceFiles(r *sessionsRun, ids []string, renderTrace func(string) (string, error)) (int, error) {
	if len(ids) == 1 {
		jsonl, err := renderTrace(ids[0])
		if err != nil {
			return 1, err
		}
		shownID := ids[0]
		if shownID == "" {
			shownID = "None"
		}
		if jsonl == "" {
			r.printf("No transcript to export for session '%s'.", shownID)
			return 1, nil
		}
		output, err := outputFileInDir(nsText(r.ns, "output"), shownID+".trace.jsonl")
		if err != nil {
			return 1, err
		}
		if _, given := r.ns["output"].(string); given {
			r.ns["output"] = output
		}
		if err := writeOutput(r, output, jsonl, "Exported 1 session trace to "+output); err != nil {
			return 1, err
		}
		return 0, nil
	}
	outDir, err := exportDir(r, nsText(r.ns, "output"))
	if err != nil {
		return 1, err
	}
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		return 1, err
	}
	exported := 0
	for _, sid := range ids {
		jsonl, err := renderTrace(sid)
		if err != nil {
			return 1, err
		}
		if jsonl != "" {
			if err := os.WriteFile(filepath.Join(outDir, sid+".trace.jsonl"), []byte(jsonl), 0o666); err != nil {
				return 1, err
			}
			exported++
		}
	}
	r.printf("Exported %d session trace(s) to %s", exported, outDir)
	return 0, nil
}

// One exported session with the file it was written to and the message snapshot of each segment
type exportedItem struct {
	data      *pyjson.Object
	path      string
	snapshots map[string][]*pyjson.Object
}

// Markdown or QMD export, one file per session plus a manifest entry
func exportMarkdown(r *sessionsRun, db *state.SessionDB, filters map[string]any, redact func(*pyjson.Object) *pyjson.Object) (int, error) {
	output, format := nsText(r.ns, "output"), nsText(r.ns, "format")
	if output == "-" {
		r.print("Markdown/QMD export writes files; stdout (-) is only supported with --format jsonl.")
		return 1, nil
	}
	outputDir, err := exportDir(r, output)
	if err != nil {
		return 1, err
	}
	exportOne := func(sessionID string, includeLineage bool) (*exportedItem, error) {
		export := db.ExportSession
		if includeLineage {
			export = db.ExportSessionLineage
		}
		rawData, err := export(r.ctx, sessionID, true, false)
		if err != nil || rawData == nil || rawData.Len() == 0 {
			return nil, err
		}
		segments := []*pyjson.Object{rawData}
		if list, ok := rowGet(rawData, "segments").([]any); ok && len(list) > 0 {
			segments = segments[:0]
			for _, item := range list {
				if segment, ok := item.(*pyjson.Object); ok {
					segments = append(segments, segment)
				}
			}
		} else if typed, ok := rowGet(rawData, "segments").([]*pyjson.Object); ok && len(typed) > 0 {
			segments = typed
		}
		snapshots := map[string][]*pyjson.Object{}
		for _, segment := range segments {
			if id := rowText(segment, "id"); id != "" {
				snapshots[id] = exportMessages(segment)
			}
		}
		data := redact(rawData)
		path, err := memory.WriteSessionMarkdown(data, outputDir, format, nsTruthy(r.ns, "force"))
		if err != nil {
			return nil, err
		}
		if _, err := memory.AppendManifestEntry(outputDir, data, path, format); err != nil {
			return nil, err
		}
		return &exportedItem{data: data, path: path, snapshots: snapshots}, nil
	}
	deleteAfter, sessionID := nsTruthy(r.ns, "delete_after_verified"), nsText(r.ns, "session_id")
	if deleteAfter && !nsTruthy(r.ns, "yes") {
		r.print("--delete-after-verified requires --yes.")
		return 1, nil
	}
	if deleteAfter && sessionID == "" {
		r.print("--delete-after-verified is only supported with --session-id.")
		return 1, nil
	}
	lineage, hasLineage := r.ns["lineage"]
	lineageIsLogical := hasLineage && lineage == "logical"
	if sessionID != "" {
		return exportMarkdownSingle(r, db, exportOne, outputDir, lineageIsLogical)
	}
	if len(filters) == 0 {
		r.print("Refusing bulk export without a filter. Pass --session-id or " +
			"at least one filter (e.g. --older-than 90, --source telegram).")
		return 1, nil
	}
	days, source, f := pruneKwargs(filters)
	candidates, err := db.ListPruneCandidates(r.ctx, days, source, false, f)
	if err != nil {
		return 1, err
	}
	if nsTruthy(r.ns, "dry_run") {
		printDryRunPreview(r, candidates, filters)
		return 0, nil
	}
	exported := 0
	for _, row := range candidates {
		item, err := exportOne(rowText(row, "id"), lineageIsLogical)
		var exists *memory.ExportExistsError
		if errors.As(err, &exists) {
			r.printf("Skipping existing export: %s. Pass --force to overwrite.", exists.Path)
			continue
		}
		if err != nil {
			return 1, err
		}
		if item != nil && item.path != "" {
			exported++
		}
	}
	r.printf("Exported %d session(s) to %s", exported, outputDir)
	return 0, nil
}

// Markdown export of one session id, optionally followed by a verified delete of it and its delegates
func exportMarkdownSingle(r *sessionsRun, db *state.SessionDB, exportOne func(string, bool) (*exportedItem, error), outputDir string, lineageIsLogical bool) (int, error) {
	resolved, err := db.ResolveSessionID(nsText(r.ns, "session_id"))
	if err != nil {
		return 1, err
	}
	deleteAfter := nsTruthy(r.ns, "delete_after_verified")
	deleteTargetIDs := []string{resolved}
	if deleteAfter {
		if deleteTargetIDs, err = db.GetSessionDeleteTargets(r.ctx, resolved); err != nil {
			return 1, err
		}
	}
	var items []*exportedItem
	for _, targetID := range deleteTargetIDs {
		item, err := exportOne(targetID, targetID == resolved && lineageIsLogical)
		var exists *memory.ExportExistsError
		if errors.As(err, &exists) {
			r.printf("Export already exists: %s. Pass --force to overwrite.", exists.Path)
			return 1, nil
		}
		if err != nil {
			return 1, err
		}
		if item == nil || item.path == "" {
			r.printf("Session '%s' disappeared during export; nothing was deleted.", targetID)
			return 1, nil
		}
		items = append(items, item)
	}
	messageCount := 0
	for _, item := range items {
		messageCount += len(exportMessages(item.data))
	}
	n := len(items)
	destination := outputDir
	if n == 1 {
		destination = items[0].path
	}
	r.printf("Exported %d session%s (%d message%s) to %s", n, pluralS(n), messageCount, pluralS(messageCount), destination)
	if !deleteAfter {
		return 0, nil
	}
	expectedMessages := map[string][]*pyjson.Object{}
	for _, item := range items {
		ok, reason, err := memory.VerifyExportFile(item.path, item.data)
		if err != nil {
			return 1, err
		}
		if !ok {
			r.printf("Export verification failed; not deleting session '%s': %s", pyrepr.Str(rowGet(item.data, "id")), reason)
			return 1, nil
		}
		for id, messages := range item.snapshots {
			expectedMessages[id] = messages
		}
	}
	deleted, err := db.DeleteSession(r.ctx, resolved, state.DeleteSessionOptions{SessionsDir: r.sessionsDir(),
		ExpectedDeleteIDs: deleteTargetIDs, ExpectedDisplayMessages: expectedMessages, ExcludeActiveWriteGuards: true})
	var guard *state.SessionActiveWriteGuardError
	if errors.As(err, &guard) {
		r.printf("Exported, but not deleted: %s", guard.Error())
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	if !deleted {
		r.printf("Exported, but session '%s' was not deleted because its history or delegate set "+
			"changed after export.", resolved)
		return 1, nil
	}
	delegates := len(deleteTargetIDs) - 1
	delegateSuffix := ""
	if delegates > 0 {
		delegateSuffix = fmt.Sprintf(" and %d delegate session%s", delegates, pluralS(delegates))
	}
	r.printf("Deleted exported session '%s'%s.", resolved, delegateSuffix)
	return 0, nil
}
