package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Reports the size before and after, preferring the page accounting of SQLite over the file size
func printSizeChange(r *sessionsRun, db *state.SessionDB, beforeMB float64, prefix string) {
	afterMB := sizeMB(db.Path())
	if logical, ok := db.LogicalSizeBytes(r.ctx); ok {
		afterMB = float64(logical) / (1024 * 1024)
	}
	r.printf("%sDatabase size: %.1f MB -> %.1f MB (%s)", prefix, beforeMB, afterMB, sizeDeltaLabel(beforeMB-afterMB))
}

// Merges the FTS5 segments and vacuums the store, reporting the size change
func cmdOptimize(r *sessionsRun, db *state.SessionDB) (int, error) {
	beforeMB := sizeMB(db.Path())
	r.print("Optimizing session store (FTS merge + VACUUM)…")
	n, err := db.Vacuum(r.ctx)
	if err != nil {
		r.printf("Error: optimization failed: %s", sessionsExcText(err))
		return 1, nil
	}
	r.printf("Optimized %d FTS index(es).", n)
	printSizeChange(r, db, beforeMB, "")
	return 0, nil
}

// Clears the stale tool call marker rows in place, or counts them on a dry run
func cmdCleanMarkers(r *sessionsRun, db *state.SessionDB) (int, error) {
	dryRun := nsTruthy(r.ns, "dry_run")
	if dryRun {
		r.print("Dry run — scanning for stale tool-call marker rows (#78148)…")
	} else {
		r.print("Scanning for stale tool-call marker rows (#78148)…")
	}
	report, err := db.PurgeStaleToolCallMarkers(r.ctx, dryRun, !nsTruthy(r.ns, "no_backup"))
	if err != nil {
		return 1, err
	}
	affected := rowGet(report, "rows_affected")
	switch {
	case pyrepr.Equal(affected, 0):
		r.print("✓ No affected rows found — nothing to clean.")
	case dryRun:
		r.printf("Would clear %s row(s): ids %s", pyrepr.Str(affected), pyrepr.Repr(rowGet(report, "row_ids")))
	default:
		if backup := rowGet(report, "backup_path"); pyrepr.Truthy(backup) {
			r.printf("  backup: %s", pyrepr.Str(backup))
		}
		r.printf("✓ Cleared %s row(s).", pyrepr.Str(affected))
	}
	return 0, nil
}

var backfillLabels = map[string]string{"backfill": "Rebuilding index", "backfill_cjk": "Building CJK index"}

// Whether a phase reports indexed over total counts
func countedPhase(phase any) bool {
	name, _ := phase.(string)
	_, backfill := backfillLabels[name]
	return backfill || name == "teardown"
}

// Value of a progress key, the fallback when the key is absent
func infoOr(info *pyjson.Object, key string, fallback any) any {
	if v, ok := info.Get(key); ok {
		return v
	}
	return fallback
}

// Python format of a progress value under a format spec
func progressFormat(v any, spec string) string {
	text, _ := pyrepr.Format(v, spec)
	return text
}

// Float of a progress number
func progressFloat(v any) float64 {
	switch n := v.(type) {
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case float64:
		return n
	}
	return 0
}

// One progress line of the search index optimization for the phase a report names
func optimizeProgressLine(info *pyjson.Object) string {
	phase, _ := rowGet(info, "phase").(string)
	pct := progressFormat(infoOr(info, "percent", 0), "3d") + "%"
	counts := "(" + progressFormat(infoOr(info, "indexed", 0), ",") + "/" + progressFormat(infoOr(info, "total", 0), ",")
	paced := ""
	if pyrepr.Truthy(rowGet(info, "paced")) {
		paced = " — paced: another process is using the database"
	}
	if label, ok := backfillLabels[phase]; ok {
		return "  " + label + ": " + pct + " " + counts + " messages)" + paced
	}
	if phase == "teardown" {
		table := strings.TrimPrefix(pyrepr.Str(infoOr(info, "table", "")), "fts_v22_trash_")
		return "  Reclaiming old index: table " + pyrepr.Str(infoOr(info, "table_index", 0)) + "/" + pyrepr.Str(infoOr(info, "table_count", 0)) +
			" (" + table + ") " + pct + " " + counts + " rows)" + paced
	}
	elapsed := int(progressFloat(infoOr(info, "elapsed", 0)))
	return fmt.Sprintf("  Compacting database (VACUUM): %dm %02ds elapsed, ", elapsed/60, elapsed%60) +
		progressFormat(progressFloat(infoOr(info, "size_bytes", 0))/(1<<20), ",.1f") + " MB → ~" +
		progressFormat(progressFloat(infoOr(info, "estimated_bytes", 0))/(1<<20), ",.1f") + " MB"
}

// Progress of the search index optimization, one self overwriting line per phase and old index table
type optimizeProgress struct {
	w      io.Writer
	hasKey bool
	phase  any
	table  any
	last   *pyjson.Object
	line   string
}

// Draws the line of a progress report, completing the previous phase line when the phase or table moved on
func (p *optimizeProgress) call(info *pyjson.Object) {
	phase, table := rowGet(info, "phase"), rowGet(info, "table")
	if !p.hasKey || !pyrepr.Equal(phase, p.phase) || !pyrepr.Equal(table, p.table) {
		p.close(true)
		p.hasKey, p.phase, p.table = true, phase, table
	}
	if phase == "done" || (countedPhase(phase) && !pyrepr.Truthy(rowGet(info, "total")) && !pyrepr.Truthy(table)) {
		return
	}
	p.last = info
	p.draw(optimizeProgressLine(info))
}

// Overwrites the current terminal line, padding over the tail of a longer previous line
func (p *optimizeProgress) draw(line string) {
	if line == p.line {
		return
	}
	pad := max(0, utf8.RuneCountInString(p.line)-utf8.RuneCountInString(line))
	fmt.Fprint(p.w, "\r"+line+strings.Repeat(" ", pad))
	p.line = line
}

// Ends the current line, first drawing a counted phase at one hundred percent when it ran to completion
func (p *optimizeProgress) close(completed bool) {
	last := p.last
	if completed && last != nil && last.Len() > 0 && countedPhase(rowGet(last, "phase")) {
		final := pyjson.NewObject()
		for _, k := range last.Keys {
			final.Set(k, last.Vals[k])
		}
		final.Set("percent", 100)
		final.Set("indexed", infoOr(last, "total", 0))
		p.draw(optimizeProgressLine(final))
	}
	if p.line != "" {
		fmt.Fprintln(p.w)
	}
	p.last, p.line = nil, ""
}

// Rebuilds the search index in the compact layout after a disk space check and a confirmation
func cmdOptimizeStorage(r *sessionsRun, db *state.SessionDB) (int, error) {
	dbPath := db.Path()
	available, err := db.FTSOptimizeAvailable(r.ctx)
	if err != nil {
		return 1, err
	}
	if !available {
		r.print("Search index is already on the compact layout — nothing to do.")
		return 0, nil
	}
	var beforeBytes int64
	if info, err := os.Stat(dbPath); err == nil {
		beforeBytes = info.Size()
	}
	beforeMB := float64(beforeBytes) / (1024 * 1024)
	doVacuum := !nsTruthy(r.ns, "no_vacuum")
	_, _, freeBytes, freeErr := pyfs.DiskUsage(filepath.Dir(dbPath))
	needBytes := beforeBytes
	if !doVacuum {
		needBytes = int64(float64(beforeBytes) * 0.3)
	}
	r.printf("Search-index optimization for %s\n  Current database size: %.1f MB", dbPath, beforeMB)
	if freeErr == nil {
		vacuumNote := ""
		if doVacuum {
			vacuumNote = " incl. VACUUM"
		}
		r.printf("  Free disk: %.0f MB (need ~%.0f MB to complete%s)", float64(freeBytes)/(1024*1024), float64(needBytes)/(1024*1024), vacuumNote)
		if freeBytes < uint64(needBytes) {
			r.print("\n⚠ Not enough free disk to complete safely. Free up space, or run with --no-vacuum " +
				"(rebuilds the index but doesn't reclaim space until a later VACUUM).")
			return 1, nil
		}
	}
	if beforeMB > 500 {
		r.print("  This may take a while on a large database. It runs in the foreground with progress below; " +
			"safe to Ctrl-C and re-run (it resumes).")
	}
	if !nsTruthy(r.ns, "yes") {
		code, refused, err := confirmOrExitCode(r, "Proceed? [y/N] ", "Cancelled.", "--yes")
		if err != nil || refused {
			return code, err
		}
	}
	progress := &optimizeProgress{w: r.w}
	r.print("Optimizing search-index storage…")
	runCtx, stop := signal.NotifyContext(r.ctx, os.Interrupt)
	result, err := db.OptimizeFTSStorage(runCtx, progress.call, doVacuum)
	interrupted := runCtx.Err() != nil && r.ctx.Err() == nil
	stop()
	if interrupted || errors.Is(err, cliui.ErrInterrupt) {
		progress.close(false)
		r.print("Interrupted — no data was lost; re-run to resume.")
		return 130, nil
	}
	if err != nil {
		progress.close(false)
		r.printf("Error: optimization failed: %s\nNo data was lost. Re-run to resume.", sessionsExcText(err))
		return 1, nil
	}
	progress.close(false)
	if !pyrepr.Truthy(rowGet(result, "ok")) {
		r.printf("Could not optimize: %s", pyrepr.Str(infoOr(result, "reason", "unknown")))
		return 1, nil
	}
	r.print("✓ Search index optimized.")
	printSizeChange(r, db, beforeMB, "  ")
	if rowGet(result, "vacuum_skipped") == "in_use" {
		r.print("  (VACUUM skipped: another process is using the database. Re-run `icebath sessions optimize` " +
			"with it closed to reclaim freed space.)")
	} else if rowGet(result, "vacuumed") == false {
		r.print("  (VACUUM was skipped or failed — run `icebath sessions optimize` later to reclaim freed space.)")
	}
	return 0, nil
}

// Reports gateway sessions that lost their routing identity and adopts the unambiguous ones on apply
func cmdRepairRouting(r *sessionsRun, db *state.SessionDB) (int, error) {
	var maxGap *float64
	if v := filterNumber(r.ns["max_gap_seconds"]); v != nil {
		maxGap = v
	}
	records, err := db.FindOrphanedGatewaySessions(r.ctx, maxGap)
	if err != nil {
		return 1, err
	}
	var adoptable []*pyjson.Object
	for _, record := range records {
		r.printf("%s  (%s, %s messages)", pyrepr.Str(rowGet(record, "orphan_id")), pyrepr.Str(rowGet(record, "source")), pyrepr.Str(rowGet(record, "message_count")))
		if pyrepr.Truthy(rowGet(record, "adoptable")) {
			adoptable = append(adoptable, record)
			r.printf("  → adopt into %s (from %s, evidence: %s)", pyrepr.Str(rowGet(record, "session_key")),
				pyrepr.Str(rowGet(record, "donor_id")), pyrepr.Str(rowGet(record, "evidence")))
		} else {
			r.printf("  ✗ not repairable — %s", pyrepr.Str(rowGet(record, "reason")))
		}
	}
	if len(records) == 0 {
		r.print("✓ No gateway sessions are missing their routing identity.")
		return 0, nil
	}
	if len(adoptable) == 0 {
		r.printf("\n%d orphaned session(s) found, none unambiguously repairable. Nothing to do.", len(records))
		return 0, nil
	}
	if !nsTruthy(r.ns, "apply") {
		r.printf("\n%d of %d orphaned session(s) can be repaired. "+
			"Re-run with --apply to perform them.", len(adoptable), len(records))
		return 0, nil
	}
	r.print("\nStop the gateway before applying — a running gateway still holds the old routing mapping in memory.")
	code, refused, err := confirmOrExitCode(r, fmt.Sprintf("Adopt %d orphaned session(s)? [y/N] ", len(adoptable)), "Aborted — nothing was changed.", "")
	if err != nil || refused {
		return code, err
	}
	repaired := 0
	for _, record := range adoptable {
		orphanID := pyrepr.Str(rowGet(record, "orphan_id"))
		adopted, err := db.AdoptOrphanedGatewaySession(r.ctx, orphanID, pyrepr.Str(rowGet(record, "donor_id")))
		if err != nil {
			return 1, err
		}
		if adopted {
			repaired++
			r.printf("✓ %s now owns %s", orphanID, pyrepr.Str(rowGet(record, "session_key")))
		} else {
			r.printf("✗ %s was not adopted (the row changed since it was reported)", orphanID)
		}
	}
	r.printf("\nRepaired %d of %d session(s).", repaired, len(adoptable))
	return 0, nil
}

// Tool names of a stored tools pin, legacy name list or versioned object, false when unreadable
func repairPromptsPinNames(row *pyjson.Object) ([]string, bool) {
	raw := rowGet(row, "tool_names")
	text := "null"
	if pyrepr.Truthy(raw) {
		switch v := raw.(type) {
		case string:
			text = v
		case []byte:
			text = string(v)
		default:
			return nil, false
		}
	}
	pin, err := pyjson.Loads(text)
	if err != nil {
		return nil, false
	}
	tools := pin
	if obj, ok := pin.(*pyjson.Object); ok {
		tools = rowGet(obj, "tools")
	}
	list, ok := tools.([]any)
	if !ok {
		return nil, false
	}
	names := []string{}
	for _, item := range list {
		var name any
		switch v := item.(type) {
		case string:
			name = v
		case *pyjson.Object:
			if function, ok := rowGet(v, "function").(*pyjson.Object); ok {
				name = rowGet(function, "name")
			}
		default:
			return nil, false
		}
		text, ok := name.(string)
		if !ok || text == "" {
			return nil, false
		}
		names = append(names, text)
	}
	return names, true
}

// Whether a stored prompt carries neither the skill_manage guidance nor the skill safety heading
func repairPromptsLacksSkillSafety(row *pyjson.Object) bool {
	prompt := pystr.Strip(rowText(row, "system_prompt"))
	return prompt != "" && !strings.Contains(prompt, agent.SkillsGuidance) && !strings.Contains(prompt, agent.SkillSafetyHeading)
}

// A finding of the stored prompt scan as the ordered JSON object the report prints
func promptFinding(row *pyjson.Object, reason string, reasonFirst bool) *pyjson.Object {
	chars := utf8.RuneCountInString(rowText(row, "system_prompt"))
	if reasonFirst {
		return pyjson.ObjectOf("id", rowGet(row, "id"), "reason", reason, "prompt_chars", chars)
	}
	return pyjson.ObjectOf("id", rowGet(row, "id"), "prompt_chars", chars, "reason", reason)
}

// Reports stored system prompts degraded to a reduced toolset build and clears the verified ones on apply
func cmdRepairPrompts(r *sessionsRun, db *state.SessionDB) (int, error) {
	findings, unverifiable := []any{}, []any{}
	target, sessionID := nsText(r.ns, "session_id"), ""
	if target != "" {
		var err error
		if sessionID, err = db.ResolveSessionID(target); err != nil {
			return 1, err
		}
		if sessionID == "" {
			r.printf("No session matches %s.", pyrepr.Repr(target))
			return 1, nil
		}
		row, err := db.GetSession(r.ctx, sessionID)
		if err != nil {
			return 1, err
		}
		if row != nil && pyrepr.Truthy(rowGet(row, "system_prompt")) {
			findings = append(findings, promptFinding(row, "targeted clear", true))
		}
	} else {
		seen := map[string]bool{}
		offset, pageSize := 0, 200
		for {
			batch, err := db.ListSessionsRich(r.ctx, state.ListSessionsRichOptions{Limit: &pageSize, Offset: offset, IncludeChildren: true,
				IncludeArchived: true, IncludeHidden: true, CompactRows: true})
			if err != nil {
				return 1, err
			}
			if len(batch) == 0 {
				break
			}
			offset += len(batch)
			for _, summary := range batch {
				id := rowText(summary, "id")
				if seen[id] {
					continue
				}
				seen[id] = true
				row, err := db.GetSession(r.ctx, id)
				if err != nil {
					return 1, err
				}
				if row == nil || row.Len() == 0 || !repairPromptsLacksSkillSafety(row) {
					continue
				}
				pin, readable := repairPromptsPinNames(row)
				memoryOnly := len(pin) > 0
				hasSkillManage := false
				for _, name := range pin {
					hasSkillManage = hasSkillManage || name == "skill_manage"
					memoryOnly = memoryOnly && name == "memory"
				}
				switch {
				case hasSkillManage:
					findings = append(findings, promptFinding(row, "skill_manage guidance missing while the tools[] pin carries skill_manage", false))
				case !readable:
					unverifiable = append(unverifiable, promptFinding(row, "skills markers missing but the tools[] pin is unavailable or unreadable", false))
				case memoryOnly:
					unverifiable = append(unverifiable, promptFinding(row, "memory-only tools[] pin may be a user toolset; clear explicitly by SESSION_ID", false))
				}
			}
		}
	}
	apply, asJSON := nsTruthy(r.ns, "apply"), nsTruthy(r.ns, "json")
	printPayload := func(cleared []any) error {
		text, err := pyjson.Dumps(pyjson.ObjectOf("findings", findings, "unverifiable", unverifiable, "apply", apply, "cleared", cleared), pyjson.Indent(2))
		if err == nil {
			r.print(text)
		}
		return err
	}
	if len(findings) == 0 {
		switch {
		case asJSON:
			if err := printPayload([]any{}); err != nil {
				return 1, err
			}
		case target != "":
			r.printf("%s already has no stored prompt; nothing to clear.", sessionID)
		default:
			r.print("No degraded stored system prompts found.")
			if len(unverifiable) > 0 {
				r.printf("%d row(s) lacked enough tools[] evidence and were left unchanged.", len(unverifiable))
			}
		}
		return 0, nil
	}
	if !asJSON {
		for _, finding := range findings {
			f := finding.(*pyjson.Object)
			r.printf("  %s  (%s chars) - %s", pyrepr.Str(rowGet(f, "id")), pyrepr.Str(rowGet(f, "prompt_chars")), pyrepr.Str(rowGet(f, "reason")))
		}
		if len(unverifiable) > 0 {
			r.printf("\nSkipped %d unverifiable row(s) without enough tools[] evidence.", len(unverifiable))
		}
	}
	if !apply {
		if asJSON {
			if err := printPayload([]any{}); err != nil {
				return 1, err
			}
		} else {
			r.printf("\n%d stored prompt(s) can be repaired. Re-run with --apply to clear them; "+
				"the next turn rebuilds a healthy prompt (one prefix-cache break per session).", len(findings))
		}
		return 0, nil
	}
	if !asJSON {
		code, refused, err := confirmOrExitCode(r, fmt.Sprintf("Clear %d stored prompt(s)? [y/N] ", len(findings)), "Aborted - nothing was changed.", "--json")
		if err != nil || refused {
			return code, err
		}
	}
	cleared := []any{}
	for _, finding := range findings {
		id := rowGet(finding.(*pyjson.Object), "id")
		if err := db.UpdateSystemPrompt(r.ctx, pyrepr.Str(id), nil); err != nil {
			return 1, err
		}
		cleared = append(cleared, id)
	}
	if asJSON {
		if err := printPayload(cleared); err != nil {
			return 1, err
		}
	} else {
		r.printf("\nCleared %d stored prompt(s); the next turn for each rebuilds and persists "+
			"a healthy prompt.", len(cleared))
		r.print("One 'Stored system prompt ... is null' warning per repaired session is expected on that rebuild.")
	}
	return 0, nil
}

// Asks continuing sessions to rebuild their stored system prompt once on their next turn
func cmdRefreshPrompts(r *sessionsRun, db *state.SessionDB) (int, error) {
	target := nsText(r.ns, "session")
	var sessionID *string
	count := 0
	if target != "" {
		resolved, err := db.ResolveSessionID(target)
		if err != nil {
			return 1, err
		}
		if resolved == "" {
			r.printf("No session matches %s.", pyrepr.Repr(target))
			return 1, nil
		}
		tip, err := db.GetCompressionTip(r.ctx, resolved)
		if err != nil {
			return 1, err
		}
		if tip != "" {
			resolved = tip
		}
		row, err := db.GetSession(r.ctx, resolved)
		if err != nil {
			return 1, err
		}
		if pyrepr.Truthy(rowGet(row, "system_prompt")) {
			count = 1
		}
		sessionID = &resolved
	} else {
		var err error
		if count, err = db.CountSessionsWithStoredPrompt(r.ctx); err != nil {
			return 1, err
		}
	}
	epoch, err := db.RequestPromptRefresh(r.ctx, sessionID)
	if err != nil {
		return 1, err
	}
	if nsTruthy(r.ns, "json") {
		var id any
		if sessionID != nil {
			id = *sessionID
		}
		text, err := pyjson.Dumps(pyjson.ObjectOf("epoch", epoch, "session_id", id, "sessions", count))
		if err != nil {
			return 1, err
		}
		r.print(text)
		return 0, nil
	}
	scope := "every session in this profile"
	if sessionID != nil && *sessionID != "" {
		scope = *sessionID
	}
	r.printf("Requested a system prompt refresh for %s (%d session(s) with a stored prompt).", scope, count)
	r.print("Each rebuilds its prompt from the current SOUL.md, skills and context files on its next turn " +
		"(one prompt-cache miss per session); conversation history is kept.")
	return 0, nil
}

// Prints the totals of the store, the per surface counts and the database size
func cmdStats(r *sessionsRun, db *state.SessionDB) (int, error) {
	reader := surfaceReader(db)
	sessions, err := db.SessionCount(r.ctx, state.SessionCountOptions{})
	if err != nil {
		return 1, err
	}
	messages, err := db.MessageCount(r.ctx, "")
	if err != nil {
		return 1, err
	}
	r.printf("Total sessions: %d\nTotal messages: %d", sessions, messages)
	for _, src := range []string{"cli", "telegram", "discord", "whatsapp", "slack"} {
		c, err := reader.SessionCount(r.ctx, state.SessionCountOptions{Source: src})
		if err != nil {
			return 1, err
		}
		if c > 0 {
			r.printf("  %s: %d sessions", src, c)
		}
	}
	if _, err := os.Stat(db.Path()); err == nil {
		r.printf("Database size: %.1f MB", sizeMB(db.Path()))
	}
	return 0, nil
}

// Reports an empty store for a profile that never created state.db
func printEmptyStore(r *sessionsRun, action string) {
	switch action {
	case "stats":
		r.print("Total sessions: 0\nTotal messages: 0")
	case "pinned":
		if nsTruthy(r.ns, "json") {
			r.print("[]")
		} else {
			r.print("No pinned sessions. Pin one with: icebath sessions pin <session_id>")
		}
	default:
		r.print("No sessions found.")
	}
}

var (
	observationalDBActions = map[string]bool{"list": true, "stats": true, "pinned": true}
	heldStoreActions       = map[string]bool{"optimize": true, "optimize-storage": true, "prune": true}
)

// Handler of an action that runs before the session store is opened, nil for any other action
func preDBHandler(action string) func(r *sessionsRun) (int, error) {
	switch action {
	case "repair":
		return func(r *sessionsRun) (int, error) { return state.CmdRepair(r.ctx, r.ns, r.w) }
	case "recover":
		return cmdRecover
	case "import":
		return cmdImport
	case "repair-profiles":
		return cmdRepairProfiles
	case "set-journal-mode":
		return func(r *sessionsRun) (int, error) { return state.CmdSetJournalMode(r.ctx, r.ns, r.w) }
	case "discard":
		return func(r *sessionsRun) (int, error) { return cmdDiscard(r.ctx, r.h, r.ns, r.w) }
	case "adopt":
		return func(r *sessionsRun) (int, error) { return cmdAdopt(r.ctx, r.h, r.ns, r.w) }
	}
	return nil
}

// Handler of an action that shares the one open session store, nil for any other action
func dbHandler(action string) func(r *sessionsRun, db *state.SessionDB) (int, error) {
	switch action {
	case "list":
		return cmdList
	case "export":
		return cmdExport
	case "delete":
		return cmdDelete
	case "rename":
		return cmdRename
	case "pinned":
		return cmdPinned
	case "prune", "archive":
		return func(r *sessionsRun, db *state.SessionDB) (int, error) { return cmdPruneOrArchive(r, db, action) }
	case "pin", "unpin":
		return func(r *sessionsRun, db *state.SessionDB) (int, error) { return cmdPin(r, db, action == "pin") }
	case "retitle-skills":
		return cmdRetitleSkills
	case "browse":
		return cmdBrowse
	case "optimize":
		return cmdOptimize
	case "clean-markers":
		return cmdCleanMarkers
	case "optimize-storage":
		return cmdOptimizeStorage
	case "repair-routing":
		return cmdRepairRouting
	case "repair-prompts":
		return cmdRepairPrompts
	case "stats":
		return cmdStats
	case "refresh-prompts":
		return cmdRefreshPrompts
	}
	return nil
}

// Runs icebath sessions, routing the action to its handler and returning the exit code
func CmdSessions(ctx context.Context, h string, ns map[string]any, w io.Writer, printHelp func() error) (int, error) {
	r := &sessionsRun{ctx: ctx, h: h, ns: ns, w: w, printHelp: printHelp}
	action := nsText(ns, "sessions_action")
	if pre := preDBHandler(action); pre != nil {
		return pre(r)
	}
	format := nsText(ns, "format")
	deletingExport := action == "export" && nsTruthy(ns, "delete_after_verified") && nsTruthy(ns, "yes") &&
		nsTruthy(ns, "session_id") && (format == "md" || format == "qmd")
	observational := observationalDBActions[action] || (action == "export" && !deletingExport)
	if action == "prune" {
		r.ownerEndpoint = liveOwner(ctx, h)
		observational = r.ownerEndpoint != nil
	}
	path := filepath.Join(h, "state.db")
	db, err := state.NewSessionDB(ctx, path, observational)
	if err != nil {
		if _, statErr := os.Stat(path); observational && statErr != nil {
			printEmptyStore(r, action)
			return 0, nil
		}
		r.print("Could not open your session history database. " +
			"Run: icebath sessions repair to fix it (a backup is made first).")
		r.printf("Details: %s", sessionsExcText(err))
		return 1, nil
	}
	defer db.Close()
	handler := dbHandler(action)
	if handler == nil {
		return 0, printHelp()
	}
	if heldStoreActions[action] && !nsTruthy(ns, "dry_run") && !nsTruthy(ns, "force") && r.ownerEndpoint == nil {
		refusal, refused, err := state.HeldStoreRefusal(ctx, path, action, "--force")
		if err != nil {
			return 1, err
		}
		if refused {
			r.print(refusal)
			return 1, nil
		}
	}
	code, err := handler(r, db)
	if class, ok := state.SQLiteClassOf(err); ok && class == state.SQLiteOperationalError && observational && state.SchemaNotBuilt(err) {
		r.printf("Error: session database needs migration — run any writing icebath command first (%s)", sessionsExcText(err))
		return 1, nil
	}
	return code, err
}
