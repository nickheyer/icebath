package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/state"
)

// Deletes one session by id after a confirmation that names a pin
func cmdDelete(r *sessionsRun, db *state.SessionDB) (int, error) {
	raw := nsText(r.ns, "session_id")
	resolved, err := db.ResolveSessionID(raw)
	if err != nil {
		return 1, err
	}
	if resolved == "" {
		return notFound(r, raw), nil
	}
	row, err := db.GetSession(r.ctx, resolved)
	if err != nil {
		return 1, err
	}
	pinnedNote := ""
	if pyrepr.Truthy(rowGet(row, "pinned")) {
		pinnedNote = " (this session is PINNED)"
	}
	if !nsTruthy(r.ns, "yes") {
		code, refused, err := confirmOrExitCode(r, "Delete session '"+resolved+"'"+pinnedNote+" and all its messages? [y/N] ", "Cancelled.", "--yes")
		if err != nil || refused {
			return code, err
		}
	} else if pinnedNote != "" {
		r.printf("Warning: deleting a pinned session '%s'.", resolved)
	}
	deleted, err := db.DeleteSession(r.ctx, resolved, state.DeleteSessionOptions{SessionsDir: r.sessionsDir(), ExcludeActiveWriteGuards: true})
	var guard *state.SessionActiveWriteGuardError
	var runtimeErr *state.RuntimeStoreError
	switch {
	case errors.As(err, &guard):
		r.printf("Cannot delete active session: %s", guard.Error())
		return 1, nil
	case errors.As(err, &runtimeErr):
		switch runtimeErr.Reason {
		case "unknown_execution":
			r.printf("Cannot delete session '%s': a turn was lost across a gateway restart. "+
				"Acknowledge it first with: icebath sessions discard %s", resolved, resolved)
		case "session_busy":
			r.printf("Cannot delete session '%s': it has a queued or running turn. "+
				"Wait for it to finish (or stop it), then retry.", resolved)
		default:
			r.printf("Cannot delete session '%s': %s", resolved, runtimeErr.Reason)
		}
		return 1, nil
	case err != nil:
		return 1, err
	}
	if !deleted {
		return notFound(r, raw), nil
	}
	r.printf("Deleted session '%s'.", resolved)
	return 0, nil
}

const neverActiveDefaultDays = 30.0

// Whether a gateway owner call failed the way the client, the socket or its deadline fails
func ownerCallFailed(err error) bool {
	var clientErr *gateway.GatewayClientError
	return errors.As(err, &clientErr) || pypath.IsOSError(err) || errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err)
}

// Text of a row value, the fallback when it is missing, None or empty
func rowTextOr(row *pyjson.Object, key, fallback string) string {
	if v := rowGet(row, key); pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return fallback
}

// Deletes keyed gateway rows that were opened and never used once they pass the age floor
func pruneNeverActiveKeyed(r *sessionsRun, db *state.SessionDB) (int, error) {
	days := neverActiveDefaultDays
	if olderThan := r.ns["older_than"]; olderThan != nil {
		seconds, ok := parseDurationSeconds(pyrepr.Str(olderThan))
		if !ok {
			r.printf("Error: --older-than '%s' is not a duration. "+
				"Use a bare number of days or a form like '2d' / '1w'.", pyrepr.Str(olderThan))
			return 1, nil
		}
		days = seconds / 86400.0
	}
	candidates, err := db.ListNeverActiveKeyedSessions(r.ctx, days)
	if err != nil {
		return 1, err
	}
	daysText, _ := pyrepr.Format(days, "g")
	if len(candidates) == 0 {
		r.printf("No never-active keyed sessions older than %s day(s).", daysText)
		return 0, nil
	}
	dryRun := nsTruthy(r.ns, "dry_run")
	shown := candidates
	if !dryRun {
		shown = candidates[:min(len(candidates), 15)]
	}
	r.printf("%d never-active keyed session(s) older than %s day(s) "+
		"— no messages, tokens, tool calls or title:", len(candidates), daysText)
	for _, s := range shown {
		r.printf("  %s  %s %s %s", pyrepr.Str(rowGet(s, "id")), padRight(formatEpoch(rowGet(s, "started_at")), 17),
			padRight(rowTextOr(s, "source", "-"), 10), rowTextOr(s, "session_key", "-"))
	}
	if len(candidates) > len(shown) {
		more := len(candidates) - len(shown)
		sessionsPrintTruncated(r, &more, "")
	}
	if dryRun {
		r.print("Dry run — nothing deleted.")
		return 0, nil
	}
	if !nsTruthy(r.ns, "yes") {
		code, refused, err := confirmOrExitCode(r, fmt.Sprintf("Delete %d session(s)? [y/N] ", len(candidates)), "Aborted.", "--yes")
		if err != nil || refused {
			return code, err
		}
	}
	var deleted, routingDeleted, skipped any
	if r.ownerEndpoint != nil {
		result, err := ownerPrune(r.ctx, r.ownerEndpoint, map[string]any{"never_active_days": days})
		if err != nil {
			if ownerCallFailed(err) {
				return reportOwnerFailure(r.w, err), nil
			}
			return 1, err
		}
		deleted, routingDeleted, skipped = result["deleted"], result["routing_deleted"], result["skipped"]
	} else {
		d, routed, skip, err := db.PruneNeverActiveKeyedSessions(r.ctx, state.NeverActivePruneOptions{OlderThanDays: days, SessionsDir: r.sessionsDir()})
		if err != nil {
			return 1, err
		}
		deleted, routingDeleted, skipped = d, routed, skip
	}
	r.printf("Deleted %s never-active session(s) and %s stale routing entr(ies).", pyrepr.Str(deleted), pyrepr.Str(routingDeleted))
	if pyrepr.Truthy(skipped) {
		r.printf("Skipped %s session(s) with a live turn or compression lock.", pyrepr.Str(skipped))
	}
	return 0, nil
}

// Tells how many pinned rows a bulk prune or archive spares and how to include them
func notePinnedSkipped(r *sessionsRun, db *state.SessionDB, filters map[string]any, action string) error {
	days, source, f := pruneKwargs(filters)
	skipped, err := db.CountPruneMatches(r.ctx, days, source, true, f)
	if err != nil || skipped == 0 {
		return err
	}
	verb, optin := "archived", "Unpin first with `icebath sessions unpin <id>` to include them."
	if action == "prune" {
		verb = "deleted"
		optin = "Pass --include-pinned to delete them anyway, or unpin first with `icebath sessions unpin <id>`."
	}
	r.printf("Note: %d pinned session%s also match these filters but will NOT be %s "+
		"(pin is a keep flag). %s", skipped, pluralS(skipped), verb, optin)
	return nil
}

// Bulk delete or archive of the ended sessions the filters match, previewed and confirmed first
func cmdPruneOrArchive(r *sessionsRun, db *state.SessionDB, action string) (int, error) {
	prune := action == "prune"
	if prune && nsTruthy(r.ns, "never_active") {
		return pruneNeverActiveKeyed(r, db)
	}
	if prune && !anyFilterArgs(r.ns) {
		r.ns["older_than"] = "90"
	}
	filters, err := buildPruneFilters(r.ns)
	if err != nil {
		r.printf("Error: %s", sessionsExcText(err))
		return 1, nil
	}
	if !prune {
		anyFilter := false
		for k, v := range filters {
			anyFilter = anyFilter || (k != "older_than_days" && pyrepr.Truthy(v))
		}
		if !anyFilter {
			r.print("Refusing to archive every ended session: pass at least one " +
				"filter (e.g. --newer-than 5h, --source cli, --title codex).")
			return 1, nil
		}
	}
	if prune && nsTruthy(r.ns, "include_archived") {
		filters["archived"] = nil
	} else {
		filters["archived"] = false
	}
	filters["include_pinned"] = nsTruthy(r.ns, "include_pinned")
	filters["lineage_tips_only"] = !prune
	includeOpen := !prune && nsTruthy(r.ns, "include_open")
	if includeOpen {
		filters["include_open"] = true
	}
	if !nsTruthy(r.ns, "include_pinned") {
		if err := notePinnedSkipped(r, db, filters, action); err != nil {
			return 1, err
		}
	}
	days, source, f := pruneKwargs(filters)
	candidates, err := db.ListPruneCandidates(r.ctx, days, source, prune, f)
	if err != nil {
		return 1, err
	}
	skippedOpen := 0
	if prune || !includeOpen {
		if skippedOpen, err = db.CountOpenPruneMatches(r.ctx, days, source, f); err != nil {
			return 1, err
		}
	}
	if skippedOpen > 0 && prune {
		r.printf("Note: %d open session%s also match these filters but "+
			"will be skipped because prune only deletes ended sessions. Use `icebath sessions delete <id>` "+
			"to remove one explicitly.", skippedOpen, pluralS(skippedOpen))
	} else if skippedOpen > 0 {
		r.printf("Note: %d matching session%s still open — "+
			"skipped because archive defaults to ended sessions. Use --include-open to archive "+
			"them too.", skippedOpen, pluralS(skippedOpen))
	}
	if len(candidates) == 0 {
		r.printf("No sessions match (%s).", describeFilters(filters))
		return 0, nil
	}
	span := "oldest activity " + formatEpoch(rowGet(candidates[0], "last_active")) + ", " +
		"newest activity " + formatEpoch(rowGet(candidates[len(candidates)-1], "last_active"))
	dryRun, yes := nsTruthy(r.ns, "dry_run"), nsTruthy(r.ns, "yes")
	if dryRun || !yes {
		shown := candidates
		if !dryRun {
			shown = candidates[:min(len(candidates), 15)]
		}
		r.printf("%d session(s) match (%s; %s):", len(candidates), describeFilters(filters), span)
		for _, s := range shown {
			parts := strings.Split(rowTextOr(s, "model", "-"), "/")
			model := runePrefix(parts[len(parts)-1], 24)
			r.printf("  %s  %s %s %s %s msgs  %s", pyrepr.Str(rowGet(s, "id")), padRight(formatEpoch(rowGet(s, "last_active")), 17),
				padRight(pyrepr.Str(rowGet(s, "source")), 10), padRight(model, 24), padLeft(pyrepr.Str(rowGet(s, "message_count")), 4),
				runePrefix(rowTextOr(s, "title", ""), 36))
		}
		if len(candidates) > len(shown) {
			more := len(candidates) - len(shown)
			sessionsPrintTruncated(r, &more, "")
		}
		if dryRun {
			if prune {
				r.print("Dry run — nothing deleted.")
			} else {
				r.print("Dry run — nothing archived.")
			}
			return 0, nil
		}
	}
	verb := "Archive"
	if prune {
		verb = "Delete"
	}
	if !yes {
		code, refused, err := confirmOrExitCode(r, fmt.Sprintf("%s these %d session(s) (%s)? [y/N] ", verb, len(candidates), span), "Cancelled.", "--yes")
		if err != nil || refused {
			return code, err
		}
	}
	switch {
	case prune && r.ownerEndpoint != nil:
		result, err := ownerPrune(r.ctx, r.ownerEndpoint, map[string]any{"filters": filters})
		if err != nil {
			if ownerCallFailed(err) {
				return reportOwnerFailure(r.w, err), nil
			}
			return 1, err
		}
		r.printf("Pruned %s session(s).", pyrepr.Str(result["deleted"]))
	case prune:
		n, err := db.PruneSessions(r.ctx, state.PruneSessionsOptions{OlderThanDays: days, Source: source, SessionsDir: r.sessionsDir(),
			ExcludeActiveWriteGuards: true, Filters: f})
		if err != nil {
			return 1, err
		}
		r.printf("Pruned %d session(s).", n)
	default:
		n, err := db.ArchiveSessions(r.ctx, days, source, f)
		if err != nil {
			return 1, err
		}
		r.printf("Archived %d session(s). They're hidden from listings "+
			"but fully recoverable (nothing was deleted).", n)
	}
	return 0, nil
}

// Whether an error stands for a Python ValueError
func isValueError(err error) bool {
	var typed *pyint.Error
	if errors.As(err, &typed) {
		return typed.Kind == "ValueError"
	}
	return err != nil && pyrepr.ExceptionName(err) == "ValueError"
}

// Sets the title of one session, refusing an empty or multi line title
func cmdRename(r *sessionsRun, db *state.SessionDB) (int, error) {
	raw := nsText(r.ns, "session_id")
	resolved, err := db.ResolveSessionID(raw)
	if err != nil {
		return 1, err
	}
	if resolved == "" {
		return notFound(r, raw), nil
	}
	words, _ := r.ns["title"].([]string)
	title := strings.Join(words, " ")
	if pystr.Strip(title) == "" {
		r.print("Error: title cannot be empty or whitespace-only.")
		return 1, nil
	}
	if strings.ContainsAny(title, "\n\r") {
		r.print("Error: title cannot contain newlines.")
		return 1, nil
	}
	changed, err := db.SetSessionTitle(r.ctx, resolved, title)
	if isValueError(err) {
		r.printf("Error: %s", sessionsExcText(err))
		return 1, nil
	}
	if err != nil {
		return 1, err
	}
	if !changed {
		return notFound(r, raw), nil
	}
	r.printf("Session '%s' renamed to: %s", resolved, title)
	return 0, nil
}

// Sets or clears the durable keep flag on each named session, failing when any id is unknown
func cmdPin(r *sessionsRun, db *state.SessionDB, pinning bool) (int, error) {
	failures := 0
	ids, _ := r.ns["session_ids"].([]string)
	for _, rawID := range ids {
		resolved, err := db.ResolveSessionID(rawID)
		if err != nil {
			return 1, err
		}
		changed := false
		if resolved != "" {
			if changed, err = db.SetSessionPinned(r.ctx, resolved, pinning); err != nil {
				return 1, err
			}
		}
		if !changed {
			failures += notFound(r, rawID)
			continue
		}
		title, _, err := db.GetSessionTitle(r.ctx, resolved)
		if err != nil {
			return 1, err
		}
		verb, suffix := "Unpinned", ""
		if pinning {
			verb = "Pinned"
		}
		if title != "" {
			suffix = "  (" + title + ")"
		}
		r.printf("%s session '%s'.%s", verb, resolved, suffix)
	}
	if failures > 0 {
		return 1, nil
	}
	return 0, nil
}

// Lists the pinned sessions as a table or as JSON for backup scripting
func cmdPinned(r *sessionsRun, db *state.SessionDB) (int, error) {
	one := 1
	rows, err := db.ListSessionsRich(r.ctx, state.ListSessionsRichOptions{Limit: &one, IncludePinned: true, ExcludeSources: defaultExclude(r.ns)})
	if err != nil {
		return 1, err
	}
	var pinnedRows []*pyjson.Object
	for _, s := range rows {
		if pyrepr.Truthy(rowGet(s, "pinned")) {
			pinnedRows = append(pinnedRows, s)
		}
	}
	if nsTruthy(r.ns, "json") {
		out := make([]any, 0, len(pinnedRows))
		for _, s := range pinnedRows {
			entry := pyjson.NewObject()
			for _, k := range []string{"id", "title", "source", "last_active", "message_count"} {
				entry.Set(k, rowGet(s, k))
			}
			out = append(out, entry)
		}
		text, err := pyjson.Dumps(out, pyjson.Indent(2))
		if err != nil {
			return 1, err
		}
		r.print(text)
		return 0, nil
	}
	if len(pinnedRows) == 0 {
		r.print("No pinned sessions. Pin one with: icebath sessions pin <session_id>")
		return 0, nil
	}
	r.print(padRight("Title", 32) + " " + padRight("Last Active", 13) + " " + padRight("Src", 9) + " ID\n" + strings.Repeat("─", 100))
	now := time.Now()
	for _, s := range pinnedRows {
		title := rowText(s, "title")
		if title == "" {
			title = rowText(s, "preview")
		}
		if title == "" {
			title = "—"
		}
		id := pyrepr.Str(rowGet(s, "id"))
		r.print(padRight(runePrefix(title, 30), 32) + " " + padRight(pydatetime.RelativeTime(rowGet(s, "last_active"), id, now), 13) + " " +
			padRight(rowTextOr(s, "source", "-"), 9) + " " + id)
	}
	return 0, nil
}

// Regenerates titles that came from the text of a /skill instead of what the user typed
func cmdRetitleSkills(r *sessionsRun, db *state.SessionDB) (int, error) {
	limit, _ := nsInt(r.ns, "limit")
	if limit == 0 {
		limit = 200
	}
	limit = max(1, limit)
	applyChanges := nsTruthy(r.ns, "apply")
	candidates, err := db.ListSkillScaffoldedSessions(r.ctx, limit)
	if err != nil {
		return 1, err
	}
	if len(candidates) == 0 {
		r.print("No sessions were titled from a /skill invocation.")
		return 0, nil
	}
	mode := " (dry run — pass --apply to write)"
	if applyChanges {
		mode = ""
	}
	r.printf("%d session(s) opened with a /skill%s:", len(candidates), mode)
	changed := 0
	for _, row := range candidates {
		sessionID := rowText(row, "id")
		typed, _ := skills.DescribeSkillInvocation(rowText(row, "content"), " — ")
		newTitle := agent.GenerateTitle(r.ctx, typed)
		oldTitle := rowGet(row, "title")
		if newTitle == "" || pyrepr.Equal(newTitle, oldTitle) {
			continue
		}
		if first := []rune(newTitle)[0]; !pystr.IsAlnum(first) {
			r.printf("  %s\n    kept %s — got %s", sessionID, pyrepr.Repr(oldTitle), pyrepr.Repr(newTitle))
			continue
		}
		r.printf("  %s\n    %s\n    → %s", sessionID, pyrepr.Repr(oldTitle), pyrepr.Repr(newTitle))
		changed++
		if !applyChanges {
			continue
		}
		_, err := db.SetSessionTitle(r.ctx, sessionID, newTitle)
		if err == nil {
			continue
		}
		if !isValueError(err) {
			return 1, err
		}
		deduped, err := db.GetNextTitleInLineage(r.ctx, newTitle)
		if err != nil {
			return 1, err
		}
		_, err = db.SetSessionTitle(r.ctx, sessionID, deduped)
		switch {
		case err == nil:
			r.printf("    (renamed to %s — title was taken)", pyrepr.Repr(deduped))
		case isValueError(err):
			r.printf("    skipped: %s", sessionsExcText(err))
			changed--
		default:
			return 1, err
		}
	}
	if changed == 0 {
		r.print("  every title already reflects the user's request.")
	} else if applyChanges {
		r.printf("✓ Re-titled %d session(s).", changed)
	}
	return 0, nil
}

// Opens the interactive picker and relaunches into the session it returns
func cmdBrowse(r *sessionsRun, db *state.SessionDB) (int, error) {
	limit, _ := nsInt(r.ns, "limit")
	if limit == 0 {
		limit = 500
	}
	sessions, err := surfaceReader(db).ListSessionsRich(r.ctx, state.ListSessionsRichOptions{
		Source: nsText(r.ns, "source"), ExcludeSources: defaultExclude(r.ns), Limit: &limit})
	if err != nil {
		return 1, err
	}
	if len(sessions) == 0 {
		if err := db.Close(); err != nil {
			return 1, err
		}
		r.print("No sessions found.")
		return 0, nil
	}
	selectedID, pickErr := sessionBrowsePicker(r, sessions, db)
	if err := db.Close(); err != nil && pickErr == nil {
		pickErr = err
	}
	if pickErr != nil {
		return 1, pickErr
	}
	if selectedID == "" {
		r.print("Cancelled.")
		return 0, nil
	}
	r.printf("Resuming session: %s", selectedID)
	relaunch([]string{"--resume", selectedID}, true)
	return 0, nil
}
