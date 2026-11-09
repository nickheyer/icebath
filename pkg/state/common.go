package state

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
)

// Persisted title provenance, automatic labels rank below a user chosen title
const (
	TitleSourceDerived = "derived"
	TitleSourceLLM     = "llm"
	TitleSourceUser    = "user"
)

const (
	previewHeadChars      = 63
	previewScaffoldWindow = 400
	previewMaxChars       = 60
)

// Sessions setting for the profile whose store this context touches, nil when neither config nor env sets it
func RoutedSessionsSetting(ctx context.Context, key, envVar string) any {
	if home.Override(ctx) != "" {
		cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
		if err != nil {
			return nil
		}
		switch sessions := cfg["sessions"].(type) {
		case map[string]any:
			return sessions[key]
		case config.Tree:
			return sessions[key]
		}
		return nil
	}
	if v, ok := os.LookupEnv(envVar); ok {
		return v
	}
	return nil
}

// LIKE wildcards and the escape character escaped so text matches literally under ESCAPE backslash
func EscapeLike(text string) string {
	text = strings.ReplaceAll(text, `\`, `\\`)
	text = strings.ReplaceAll(text, "%", `\%`)
	return strings.ReplaceAll(text, "_", `\_`)
}

const previewContentSQL = "REPLACE(REPLACE(m.content, X'0A', ' '), X'0D', ' ')"

var previewScaffoldedSQL = "(m.content LIKE '" + SkillScaffoldSQLLike + "' OR m.content LIKE '" + AutoLoadScaffoldSQLLike + "')"

const sqlWhitespace = "CHAR(9) || CHAR(10) || CHAR(13) || CHAR(32)"

// Single quoted SQL literal with embedded quotes doubled
func sqlLiteral(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}

// Display metadata flag on the user row a busy queue accept writes
const QueuedPromptMetadataKey = "_queued_prompt"

// Non throwing JSON marker lookup over a JSON text column
func sqlJSONExtract(expression, path string) string {
	safe := "(CASE WHEN json_valid(" + expression + ") THEN " + expression + " ELSE json_object() END)"
	return "json_extract(" + safe + ", " + sqlLiteral(path) + ")"
}

func sqlLtrimWhitespace(expression string) string {
	return "LTRIM(" + expression + ", " + sqlWhitespace + ")"
}

func sqlTrimWhitespace(expression string) string {
	return "TRIM(" + expression + ", " + sqlWhitespace + ")"
}

// Whether the whitespace trimmed expression starts with any prefix, lengths counted in code points
func sqlStartsWith(expression string, prefixes []string) string {
	trimmed := sqlLtrimWhitespace(expression)
	parts := make([]string, len(prefixes))
	for i, p := range prefixes {
		parts[i] = "SUBSTR(" + trimmed + ", 1, " + strconv.Itoa(pyLen(p)) + ") = " + sqlLiteral(p)
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// Message content after the first occurrence of marker
func sqlAfterMarker(marker string) string {
	return "SUBSTR(m.content, INSTR(m.content, " + sqlLiteral(marker) + ") + " + strconv.Itoa(pyLen(marker)) + ")"
}

var (
	previewLongFormPrefix, _, _   = strings.Cut(SummaryPrefix, "Do NOT answer")
	previewSummaryPrefixes        = []string{previewLongFormPrefix, LegacySummaryPrefix}
	previewStandaloneSummarySQL   = sqlStartsWith("m.content", previewSummaryPrefixes)
	previewMergedAfterSQL         = sqlAfterMarker(MergedSummaryDelimiter)
	previewMergedSummarySQL       = "(INSTR(m.content, " + sqlLiteral(MergedSummaryDelimiter) + ") > 0 AND " + sqlStartsWith(previewMergedAfterSQL, previewSummaryPrefixes) + ")"
	previewMergedPriorSQL         = sqlTrimWhitespace("SUBSTR(m.content, 1, INSTR(m.content, " + sqlLiteral(MergedSummaryDelimiter) + ") - 1)")
	previewMergedPriorLtrimmedSQL = sqlLtrimWhitespace(previewMergedPriorSQL)
	previewMergedPriorUnwrapped   = "CASE WHEN SUBSTR(" + previewMergedPriorLtrimmedSQL + ", 1, " + strconv.Itoa(pyLen(MergedPriorContextHeader)) + ") = " + sqlLiteral(MergedPriorContextHeader) +
		" THEN " + sqlLtrimWhitespace("SUBSTR("+previewMergedPriorLtrimmedSQL+", "+strconv.Itoa(pyLen(MergedPriorContextHeader)+1)+")") +
		" ELSE " + previewMergedPriorSQL + " END"
	previewForceUserRemainderSQL = sqlAfterMarker(SummaryEndMarker)
	previewEligibleSQL           = "(COALESCE(m.display_kind, '') <> 'hidden'" +
		" AND ((NOT " + previewStandaloneSummarySQL + " AND NOT " + previewMergedSummarySQL + ")" +
		" OR (" + previewStandaloneSummarySQL + " AND INSTR(m.content, " + sqlLiteral(SummaryEndMarker) + ") > 0" +
		" AND LENGTH(" + sqlTrimWhitespace(previewForceUserRemainderSQL) + ") > 0)" +
		" OR (" + previewMergedSummarySQL +
		" AND LENGTH(" + sqlTrimWhitespace(previewMergedPriorUnwrapped) + ") > 0)))"
	previewRawSelect = "CASE WHEN " + previewStandaloneSummarySQL + " THEN " + previewForceUserRemainderSQL +
		" WHEN " + previewMergedSummarySQL + " THEN " + previewMergedPriorUnwrapped +
		" WHEN " + previewScaffoldedSQL + " AND LENGTH(m.content) > " + strconv.Itoa(previewScaffoldWindow*2) +
		" THEN SUBSTR(" + previewContentSQL + ", 1, " + strconv.Itoa(previewScaffoldWindow) + ") || '" + SkillExcerptJoint + "'" +
		" || SUBSTR(" + previewContentSQL + ", -" + strconv.Itoa(previewScaffoldWindow) + ")" +
		" WHEN " + previewScaffoldedSQL + " THEN SUBSTR(" + previewContentSQL + ", 1, " + strconv.Itoa(previewScaffoldWindow*2) + ")" +
		" ELSE SUBSTR(" + previewContentSQL + ", 1, " + strconv.Itoa(previewHeadChars) + ") END"
)

// Short preview from a preview raw column, skill turns rendered as typed and long text cut with an ellipsis
func shapePreview(raw any) string {
	text := ""
	switch x := raw.(type) {
	case nil:
	case string:
		text = x
	case []byte:
		if len(x) > 0 {
			text = pyBytesRepr(x)
		}
	default:
		if s := pyStr(x); s != "0" && s != "0.0" && s != "-0.0" && s != "False" {
			text = s
		}
	}
	if text = pyStrip(text); text == "" {
		return ""
	}
	text = strings.NewReplacer("\n", " ", "\r", " ").Replace(text)
	if described, ok := DescribeSkillInvocation(text, " — "); ok {
		text = described
	} else {
		text, _, _ = strings.Cut(text, SkillExcerptJoint)
	}
	if runes := []rune(text); len(runes) > previewMaxChars {
		return string(runes[:previewMaxChars]) + "..."
	}
	return text
}

// Correlated preview raw column holding the first eligible user message of the session expression
func sqlPreviewRaw(sessionIDExpr string) string {
	return "COALESCE((SELECT " + previewRawSelect + " FROM messages m INDEXED BY idx_messages_session" +
		" WHERE m.session_id = " + sessionIDExpr + " AND m.role = 'user' AND m.content IS NOT NULL" +
		" AND " + previewEligibleSQL + " ORDER BY m.timestamp, m.id LIMIT 1), '') AS _preview_raw"
}

// Branch child predicate over a sessions alias, stable marker or legacy branched end reason
func branchChildSQL(a string) string {
	return sqlJSONExtract(a+".model_config", "$._branched_from") + " IS NOT NULL" +
		" OR EXISTS (SELECT 1 FROM sessions p            WHERE p.id = " + a + ".parent_session_id" +
		"            AND p.end_reason = 'branched'            AND " + a + ".started_at >= p.ended_at)"
}

// Compression continuation predicate over a sessions alias
func compressionChildSQL(a string) string {
	return "EXISTS (SELECT 1 FROM sessions p        WHERE p.id = " + a + ".parent_session_id" +
		"        AND p.end_reason = 'compression')"
}

// End reasons of a deliberate reset boundary
var resetEndReasons = []string{"session_reset", "session_switch", "idle", "daily", "suspended", "resume_pending_expired"}

var resetEndReasonsSQL = quotedSQLList(resetEndReasons)

// Deliberate conversation boundaries a compression rotation never heals
var boundaryEndReasons = setOf(append([]string{"new_session"}, resetEndReasons...)...)

// BoundaryEndReasons is a copy of the deliberate conversation boundary end reasons
func BoundaryEndReasons() map[string]bool {
	out := make(map[string]bool, len(boundaryEndReasons))
	for k := range boundaryEndReasons {
		out[k] = true
	}
	return out
}

// Accidental end reasons recovery treats as resumable
var recoverableEndReasons = []string{"agent_close", "ws_orphan_reap", "superseded_by_resume", "startup_orphan_reap"}

var recoverableEndReasonsSQL = quotedSQLList(recoverableEndReasons)

// End reasons automatic cleanup writes, the recoverable set plus the TUI gateway reasons
var automaticEndReasons = setOf(append([]string{"tui_shutdown", "ws_disconnect", "idle_timeout", "lru_evict"}, recoverableEndReasons...)...)

func quotedSQLList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "'" + item + "'"
	}
	return strings.Join(quoted, ", ")
}

func setOf(items ...string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

// Whether reason is an automatic cleanup end stamp rather than a deliberate boundary
func IsAutomaticEndReason(reason any) bool {
	s, ok := reason.(string)
	return ok && automaticEndReasons[s]
}

// Pre marker reset continuation, the child riding its parent's exact routing key past a reset boundary
func legacyResetChildSQL(alias, reasonsSQL string) string {
	return "EXISTS (SELECT 1 FROM sessions p            WHERE p.id = " + alias + ".parent_session_id" +
		"            AND p.end_reason IN (" + reasonsSQL + ")            AND " + alias + ".session_key IS NOT NULL" +
		"            AND " + alias + ".session_key != ''            AND " + alias + ".session_key = p.session_key)"
}

// AND clauses rejecting children that are forks or tool sessions rather than compression continuations of parent
func nonContinuationChildSQL(child, parent string) string {
	var b strings.Builder
	for _, marker := range []string{"_branched_from", "_delegate_from", "_reset_from"} {
		b.WriteString("  AND COALESCE(" + sqlJSONExtract(child+"model_config", "$."+marker) + ", '') != " + parent + "\n")
	}
	b.WriteString("  AND COALESCE(" + child + "source, '') != 'tool'\n")
	return b.String()
}

// Reset child predicate over a sessions alias, stable marker or same key legacy fallback
func resetChildSQL(a string) string {
	return sqlJSONExtract(a+".model_config", "$._reset_from") + " IS NOT NULL OR " + legacyResetChildSQL(a, resetEndReasonsSQL)
}

// Picker visible rows, roots plus branch and reset children
var listableChildSQL = "(s.parent_session_id IS NULL OR " + branchChildSQL("s") + " OR " + resetChildSQL("s") + ")"

// Subagent runs, children that are not branch, reset or compression continuations
func ephemeralChildSQL(alias string) string {
	return "(" + alias + ".parent_session_id IS NOT NULL AND NOT (" + branchChildSQL(alias) + ")" +
		" AND NOT (" + compressionChildSQL(alias) + ") AND NOT (" + resetChildSQL(alias) + "))"
}

var sqlInWindowClause = "BETWEEN " + pyFloatRepr(EpochMin) + " AND " + pyFloatRepr(EpochMax)

// The expression when inside the coerce_epoch window, else NULL
func sqlInWindow(expr string) string {
	return "(SELECT _win.v FROM (SELECT " + expr + " AS v) _win WHERE _win.v " + sqlInWindowClause + ")"
}

// Freshest in window activity or message timestamp of a session, else its in window start
func sqlFreshestOf(activity, sessionIDExpr, started string) string {
	msgMax := "(SELECT MAX(_act_m.timestamp) FROM messages _act_m INDEXED BY idx_messages_session" +
		" WHERE _act_m.session_id = " + sessionIDExpr +
		" AND _act_m.timestamp " + sqlInWindowClause + ")"
	return "COALESCE((SELECT MAX(_act_v.v) FROM (SELECT " + activity + " AS v UNION ALL SELECT " + msgMax + ") _act_v" +
		" WHERE _act_v.v " + sqlInWindowClause + "), " + sqlInWindow(started) + ")"
}

// Session recency expression for a sessions alias row
func sqlSessionLastActive(alias string) string {
	return sqlFreshestOf(alias+".last_activity_at", alias+".id", alias+".started_at")
}

// Session recency expression keyed by a session id SQL expression
func sqlSessionLastActiveByID(sessionIDExpr string) string {
	return sqlFreshestOf("(SELECT last_activity_at FROM sessions _act_s WHERE _act_s.id = "+sessionIDExpr+")", sessionIDExpr,
		"(SELECT started_at FROM sessions _act_s WHERE _act_s.id = "+sessionIDExpr+")")
}

// Declared schema version, drift detected by the read probe rather than this number
const SchemaVersion = 32

// Freelist fraction above which auto maintenance runs VACUUM
const AutoVacuumMinFreelistRatio = 0.25

// FTS storage layout version tracked in state_meta fts_storage_version
const FTSStorageVersion = 3

// Prefix of a tool row the base FTS index stores
const FTSToolContentPrefixChars = 8_192

// Indexed content of a message row alias, tool rows cut to the stored prefix
func ftsIndexedContentSQL(alias string) string {
	return "CASE WHEN " + alias + ".role = 'tool'\n" +
		"         THEN substr(COALESCE(" + alias + ".content, ''), 1, " + strconv.Itoa(FTSToolContentPrefixChars) + ")\n" +
		"         ELSE " + alias + ".content END"
}

var (
	ftsNewIndexedContentSQL = ftsIndexedContentSQL("new")
	ftsOldIndexedContentSQL = ftsIndexedContentSQL("old")
)

// Cap on user controlled FTS5 query input before sanitizing
const MaxFTS5QueryChars = 2_048

// Device and inode of a file, false when stat fails or either is zero
func StatDBFileIdentity(path string) (FileIdentity, bool) {
	id, ok := statFileIdentity(path)
	if !ok || id.Dev == 0 || id.Ino == 0 {
		return FileIdentity{}, false
	}
	return id, true
}

// Device and inode pair naming one file
type FileIdentity struct{ Dev, Ino uint64 }

const (
	endedRowSQL           = "SELECT ended_at, end_reason FROM sessions WHERE id = ?"
	compressionLockRowSQL = "SELECT holder, expires_at FROM compression_locks WHERE session_id = ?"
)

// Whether a sessions row ended through a compression rotation
func endedByCompression(row map[string]any) bool {
	return row != nil && row["ended_at"] != nil && row["end_reason"] == "compression"
}

// One comma separated placeholder per bound parameter
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

const notALaunchStubSQL = "(s.message_count > 0 OR EXISTS (SELECT 1 FROM session_admissions a WHERE a.target_session_id = s.id))"

// Sessions predicate dropping empty launch stubs, messages alone deciding on stores without admissions
func (d *DB) launchStubFilterSQL() (string, error) {
	found := false
	err := d.read(func(q querier) error {
		rows, err := q.Query("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'session_admissions'")
		if err != nil {
			return err
		}
		defer rows.Close()
		found = rows.Next()
		return rows.Err()
	})
	if err != nil {
		return "", err
	}
	if found {
		return notALaunchStubSQL, nil
	}
	return "s.message_count > 0", nil
}

// Sessions source predicate, local rows matching only their listed creating surfaces when given
func sourceMatchClause(sources, localCreatedSources []string) (string, []any) {
	if len(sources) == 0 {
		return "", []any{}
	}
	labels := stringsToAny(sources)
	if len(localCreatedSources) == 0 {
		return "s.source IN (" + placeholders(len(sources)) + ")", labels
	}
	var nonLocal []string
	for _, item := range sources {
		if item != "local" {
			nonLocal = append(nonLocal, item)
		}
	}
	localSQL := "(s.source = 'local' AND s.created_source IN (" + placeholders(len(localCreatedSources)) + "))"
	if len(nonLocal) == 0 {
		return localSQL, stringsToAny(localCreatedSources)
	}
	return "(s.source IN (" + placeholders(len(nonLocal)) + ") OR " + localSQL + ")",
		append(stringsToAny(nonLocal), stringsToAny(localCreatedSources)...)
}

func stringsToAny(items []string) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// Ids bound per IN list, under every SQLite build's variable cap
const sqlInChunk = 900

// Ids split into lists of at most size elements
func idChunks[T any](ids []T, size int) [][]T {
	var out [][]T
	for start := 0; start < len(ids); start += size {
		out = append(out, ids[start:min(start+size, len(ids))])
	}
	return out
}

// Dispatches a lifecycle hook to observers and plugins, returning their results
type LifecycleHookInvoker func(ctx context.Context, hookName string, kwargs map[string]any) ([]any, error)

var (
	lifecycleHookMu      sync.RWMutex
	lifecycleHookInvoker LifecycleHookInvoker
)

// Installs the lifecycle dispatcher the session delete hook fires through
func SetLifecycleHookInvoker(fn LifecycleHookInvoker) {
	lifecycleHookMu.Lock()
	lifecycleHookInvoker = fn
	lifecycleHookMu.Unlock()
}

// Fires on_session_delete under the store's own profile home after a committed delete or archive, never failing it
func (d *DB) EmitSessionDeleted(ctx context.Context, deletedIDs []string, reason, primary, sessionsDir string) {
	seen := map[string]bool{}
	var ids []string
	for _, sid := range deletedIDs {
		if sid != "" && !seen[sid] {
			seen[sid] = true
			ids = append(ids, sid)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("on_session_delete dispatch failed", "error", fmt.Sprint(r))
		}
	}()
	abs, err := filepath.Abs(d.path)
	if err != nil {
		slog.Warn("on_session_delete dispatch failed", "error", err)
		return
	}
	storeHome := filepath.Dir(abs)
	profile := home.ProfileNameFor(storeHome)
	sessionID := ids[0]
	if seen[primary] {
		sessionID = primary
	}
	payload := map[string]any{"session_id": sessionID, "reason": reason, "profile": nil}
	if profile != "" {
		payload["profile"] = profile
	}
	if len(ids) > 1 {
		payload["deleted_ids"] = ids
	}
	if sessionsDir != "" {
		payload["sessions_dir"] = sessionsDir
	}
	if profile != "" && realpathLax(storeHome) != realpathLax(home.Dir(ctx)) {
		ctx = home.WithOverride(ctx, storeHome)
	}
	lifecycleHookMu.RLock()
	invoke := lifecycleHookInvoker
	lifecycleHookMu.RUnlock()
	if invoke == nil {
		return
	}
	if _, err := invoke(ctx, "on_session_delete", payload); err != nil {
		slog.Warn("on_session_delete dispatch failed", "error", err)
	}
}

var ftsTriggers = []string{"messages_fts_insert", "messages_fts_delete", "messages_fts_update",
	"messages_fts_trigram_insert", "messages_fts_trigram_delete", "messages_fts_trigram_update"}

// Sources whose transcripts stay out of the trigram index
var FTSTrigramExcludedSources = []string{"cron", "subagent"}

// Sessions predicate selecting rows that belong in the trigram index, columns qualified by alias when given
func FTSTrigramSessionSQLFor(alias string) string {
	q := ""
	if alias != "" {
		q = alias + "."
	}
	return q + "source NOT IN (" + quotedSQLList(FTSTrigramExcludedSources) + ") AND " +
		sqlJSONExtract(q+"model_config", "$._delegate_from") + " IS NULL"
}

// Unqualified trigram session predicate the sync triggers use
var FTSTrigramSessionSQL = FTSTrigramSessionSQLFor("")

var ftsCJKTriggers = []string{"messages_fts_cjk_insert", "messages_fts_cjk_delete", "messages_fts_cjk_update"}

// State meta breadcrumbs of the FTS maintenance protocol
const (
	FTSCJKStaleKey        = "fts_cjk_stale"
	FTSStaleKey           = "fts_stale"
	FTSRebuildDeferralKey = "fts_rebuild_deferral"
)

// Decoded JSON of raw, fallback with a logged warning when it is not JSON text, decode errors of bytes propagating
func jsonOr(raw, fallback any, warning string) (any, error) {
	var text string
	switch x := raw.(type) {
	case string:
		text = x
	case []byte:
		decoded, err := pyJSONDecodeBytes(x)
		if err != nil {
			return nil, err
		}
		text = decoded
	default:
		slog.Warn(warning)
		return fallback, nil
	}
	v, err := pyLoads(text)
	if err != nil {
		slog.Warn(warning)
		return fallback, nil
	}
	return v, nil
}
