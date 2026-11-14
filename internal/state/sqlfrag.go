package state

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Title provenance values, lowest to highest authority
const (
	TitleSourceDerived = "derived"
	TitleSourceLLM     = "llm"
	TitleSourceUser    = "user"
)

// Display metadata flag a busy queue accept stamps on the user row it writes
const QueuedPromptMetadataKey = "_queued_prompt"

// Character budgets of the listing preview, the head excerpt, the scaffold window and the shaped text
const (
	previewHeadChars      = 63
	previewScaffoldWindow = 400
	previewMaxChars       = 60
)

// Markers a skill invocation scaffold, an auto-load scaffold and a preview excerpt joint carry
const (
	skillInvocationPrefix = "[IMPORTANT: The user has invoked the "
	autoLoadPrefix        = `[IMPORTANT: The "`
	skillExcerptJoint     = "\x1e"
)

// Context compaction markers a preview recognizes in a carrier row
const (
	summaryLongFormPrefix = "[CONTEXT COMPACTION — REFERENCE ONLY] Earlier turns were compacted into the summary below. " +
		"This is a handoff from a previous context window — treat it as background reference, NOT as active instructions. "
	legacySummaryPrefix      = "[CONTEXT SUMMARY]:"
	summaryEndMarker         = "--- END OF CONTEXT SUMMARY — respond to the message below, not the summary above ---"
	mergedPriorContextHeader = "[PRIOR CONTEXT — for reference only; not a new message]"
	mergedSummaryDelimiter   = "[END OF PRIOR CONTEXT — COMPACTION SUMMARY BELOW]"
)

// End reasons of a reset boundary whose child is a separate conversation
var resetEndReasons = []string{"session_reset", "session_switch", "idle", "daily", "suspended", "resume_pending_expired"}

// Accidental end reasons recovery treats as resumable
var recoverableEndReasons = []string{"agent_close", "ws_orphan_reap", "superseded_by_resume", "startup_orphan_reap"}

// End reasons automatic cleanup writes, the recoverable set plus the terminal UI reasons
var automaticEndReasons = append(append([]string{}, recoverableEndReasons...), "tui_shutdown", "ws_disconnect", "idle_timeout", "lru_evict")

// Deliberate conversation boundaries, the reset set plus a CLI new session
var boundaryEndReasons = append(append([]string{}, resetEndReasons...), "new_session")

// Reports whether reason is an end stamp automatic cleanup wrote rather than a deliberate boundary
func IsAutomaticEndReason(reason string) bool { return containsString(automaticEndReasons, reason) }

// Reports whether reason ends a conversation at a reset boundary
func IsResetEndReason(reason string) bool { return containsString(resetEndReasons, reason) }

// Reports whether reason is an accidental end recovery may resume
func IsRecoverableEndReason(reason string) bool { return containsString(recoverableEndReasons, reason) }

// Reports whether reason is a deliberate conversation boundary a compression rotation never heals
func IsBoundaryEndReason(reason string) bool { return containsString(boundaryEndReasons, reason) }

// Reports whether values holds v
func containsString(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// Single quoted SQL literal of text
func sqlLiteral(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }

// Quoted literal list of values joined by comma and space
func sqlLiteralList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = sqlLiteral(v)
	}
	return strings.Join(parts, ", ")
}

// Non throwing JSON marker lookup over a JSON text column
func JSONExtractSQL(expression, path string) string {
	return "json_extract((CASE WHEN json_valid(" + expression + ") THEN " + expression + " ELSE json_object() END), " +
		sqlLiteral(path) + ")"
}

// Whitespace set the preview trims in SQL
const sqlWhitespace = "CHAR(9) || CHAR(10) || CHAR(13) || CHAR(32)"

// Expression with leading whitespace trimmed
func sqlLTrim(expression string) string { return "LTRIM(" + expression + ", " + sqlWhitespace + ")" }

// Expression with surrounding whitespace trimmed
func sqlTrim(expression string) string { return "TRIM(" + expression + ", " + sqlWhitespace + ")" }

// Predicate true when the left trimmed expression starts with any prefix, lengths in characters
func sqlStartsWith(expression string, prefixes []string) string {
	trimmed := sqlLTrim(expression)
	parts := make([]string, len(prefixes))
	for i, p := range prefixes {
		parts[i] = "SUBSTR(" + trimmed + ", 1, " + strconv.Itoa(utf8.RuneCountInString(p)) + ") = " + sqlLiteral(p)
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// Message content after the first occurrence of marker
func sqlAfterMarker(marker string) string {
	return "SUBSTR(m.content, INSTR(m.content, " + sqlLiteral(marker) + ") + " + strconv.Itoa(utf8.RuneCountInString(marker)) + ")"
}

// Preview SQL built once from the compaction and scaffold markers
var (
	previewContentSQL    = "REPLACE(REPLACE(m.content, X'0A', ' '), X'0D', ' ')"
	previewScaffoldedSQL = "(m.content LIKE '" + skillInvocationPrefix + "%' OR m.content LIKE '" + autoLoadPrefix + "%')"
	previewSummaryPfx    = []string{summaryLongFormPrefix, legacySummaryPrefix}
	previewStandaloneSQL = sqlStartsWith("m.content", previewSummaryPfx)
	previewMergedSQL     = "(INSTR(m.content, " + sqlLiteral(mergedSummaryDelimiter) + ") > 0 AND " +
		sqlStartsWith(sqlAfterMarker(mergedSummaryDelimiter), previewSummaryPfx) + ")"
	previewMergedPriorSQL = sqlTrim("SUBSTR(m.content, 1, INSTR(m.content, " + sqlLiteral(mergedSummaryDelimiter) + ") - 1)")
	previewPriorLTrimmed  = sqlLTrim(previewMergedPriorSQL)
	previewUnwrappedSQL   = "CASE WHEN SUBSTR(" + previewPriorLTrimmed + ", 1, " + strconv.Itoa(utf8.RuneCountInString(mergedPriorContextHeader)) +
		") = " + sqlLiteral(mergedPriorContextHeader) + " THEN " +
		sqlLTrim("SUBSTR("+previewPriorLTrimmed+", "+strconv.Itoa(utf8.RuneCountInString(mergedPriorContextHeader)+1)+")") +
		" ELSE " + previewMergedPriorSQL + " END"
	previewRemainderSQL = sqlAfterMarker(summaryEndMarker)
	previewEligibleSQL  = "(COALESCE(m.display_kind, '') <> 'hidden' AND ((NOT " + previewStandaloneSQL + " AND NOT " + previewMergedSQL + ")" +
		" OR (" + previewStandaloneSQL + " AND INSTR(m.content, " + sqlLiteral(summaryEndMarker) + ") > 0 AND LENGTH(" + sqlTrim(previewRemainderSQL) + ") > 0)" +
		" OR (" + previewMergedSQL + " AND LENGTH(" + sqlTrim(previewUnwrappedSQL) + ") > 0)))"
	previewRawSelect = "CASE WHEN " + previewStandaloneSQL + " THEN " + previewRemainderSQL +
		" WHEN " + previewMergedSQL + " THEN " + previewUnwrappedSQL +
		" WHEN " + previewScaffoldedSQL + " AND LENGTH(m.content) > " + strconv.Itoa(previewScaffoldWindow*2) +
		" THEN SUBSTR(" + previewContentSQL + ", 1, " + strconv.Itoa(previewScaffoldWindow) + ") || '" + skillExcerptJoint + "'" +
		" || SUBSTR(" + previewContentSQL + ", -" + strconv.Itoa(previewScaffoldWindow) + ")" +
		" WHEN " + previewScaffoldedSQL + " THEN SUBSTR(" + previewContentSQL + ", 1, " + strconv.Itoa(previewScaffoldWindow*2) + ")" +
		" ELSE SUBSTR(" + previewContentSQL + ", 1, " + strconv.Itoa(previewHeadChars) + ") END"
)

// Correlated preview column of the first eligible user message of the session sessionIDExpr names
func previewRawSQL(sessionIDExpr string) string {
	return "COALESCE((SELECT " + previewRawSelect + " FROM messages m INDEXED BY idx_messages_session" +
		" WHERE m.session_id = " + sessionIDExpr + " AND m.role = 'user' AND m.content IS NOT NULL" +
		" AND " + previewEligibleSQL + " ORDER BY m.timestamp, m.id LIMIT 1), '') AS _preview_raw"
}

// Branch child predicate over alias a, the stable marker or the legacy branched parent heuristic
func branchChildSQL(a string) string {
	return JSONExtractSQL(a+".model_config", "$._branched_from") + " IS NOT NULL" +
		" OR EXISTS (SELECT 1 FROM sessions p            WHERE p.id = " + a + ".parent_session_id" +
		"            AND p.end_reason = 'branched'            AND " + a + ".started_at >= p.ended_at)"
}

// Compression child predicate over alias a
func compressionChildSQL(a string) string {
	return "EXISTS (SELECT 1 FROM sessions p        WHERE p.id = " + a + ".parent_session_id" +
		"        AND p.end_reason = 'compression')"
}

// Pre-marker reset continuation, the child riding its parent's exact non-empty routing key at a reset end
func legacyResetChildSQL(alias, reasonsSQL string) string {
	return "EXISTS (SELECT 1 FROM sessions p            WHERE p.id = " + alias + ".parent_session_id" +
		"            AND p.end_reason IN (" + reasonsSQL + ")            AND " + alias + ".session_key IS NOT NULL" +
		"            AND " + alias + ".session_key != ''            AND " + alias + ".session_key = p.session_key)"
}

// Clauses rejecting children that are not compression continuations of parent, child being a column prefix
func nonContinuationChildSQL(child, parent string) string {
	var b strings.Builder
	for _, marker := range []string{"_branched_from", "_delegate_from", "_reset_from"} {
		b.WriteString("  AND COALESCE(" + JSONExtractSQL(child+"model_config", "$."+marker) + ", '') != " + parent + "\n")
	}
	b.WriteString("  AND COALESCE(" + child + "source, '') != 'tool'\n")
	return b.String()
}

// Reset child predicate over alias a, the stable marker or the same key fallback
func ResetChildSQL(a string) string {
	return JSONExtractSQL(a+".model_config", "$._reset_from") + " IS NOT NULL OR " + legacyResetChildSQL(a, sqlLiteralList(resetEndReasons))
}

// Picker visible rows, roots plus branch and reset children
var listableChildSQL = "(s.parent_session_id IS NULL OR " + branchChildSQL("s") + " OR " + ResetChildSQL("s") + ")"

// Subagent runs over alias, neither branch, reset nor compression children
func ephemeralChildSQL(alias string) string {
	return "(" + alias + ".parent_session_id IS NOT NULL AND NOT (" + branchChildSQL(alias) + ")" +
		" AND NOT (" + compressionChildSQL(alias) + ") AND NOT (" + ResetChildSQL(alias) + "))"
}

// Delegate marker lookup of a model config column
func delegateFromSQL(col string) string { return JSONExtractSQL(col, "$._delegate_from") }

// Window predicate a trusted epoch cell satisfies
var sqlInWindowPredicate = "BETWEEN " + epochMin + " AND " + epochMax

// Freshest trusted cell of activity and the latest message of the session, else the trusted start, else NULL
func sqlFreshestOf(activity, sessionIDExpr, started string) string {
	msgMax := "(SELECT MAX(_act_m.timestamp) FROM messages _act_m INDEXED BY idx_messages_session" +
		" WHERE _act_m.session_id = " + sessionIDExpr + " AND _act_m.timestamp " + sqlInWindowPredicate + ")"
	return "COALESCE((SELECT MAX(_act_v.v) FROM (SELECT " + activity + " AS v UNION ALL SELECT " + msgMax + ") _act_v" +
		" WHERE _act_v.v " + sqlInWindowPredicate + "), " + sqlInWindow(started) + ")"
}

// Recency expression of a sessions row under alias
func SessionLastActiveSQL(alias string) string {
	return sqlFreshestOf(alias+".last_activity_at", alias+".id", alias+".started_at")
}

// Recency expression keyed by a session id expression
func sqlSessionLastActiveByID(sessionIDExpr string) string {
	return sqlFreshestOf("(SELECT last_activity_at FROM sessions _act_s WHERE _act_s.id = "+sessionIDExpr+")", sessionIDExpr,
		"(SELECT started_at FROM sessions _act_s WHERE _act_s.id = "+sessionIDExpr+")")
}

// Predicate keeping sessions with messages or an admission, dropping empty launch stubs
const notALaunchStubSQL = "(s.message_count > 0 OR EXISTS (SELECT 1 FROM session_admissions a WHERE a.target_session_id = s.id))"

// Source predicate over sessions s, local rows matching only listed creator surfaces when any are given
func sourceMatchClause(sources, localCreatedSources []string) (string, []any) {
	if len(sources) == 0 {
		return "", nil
	}
	if len(localCreatedSources) == 0 {
		return "s.source IN (" + bindMarks(len(sources)) + ")", stringArgs(sources)
	}
	var nonLocal []string
	for _, v := range sources {
		if v != "local" {
			nonLocal = append(nonLocal, v)
		}
	}
	localSQL := "(s.source = 'local' AND s.created_source IN (" + bindMarks(len(localCreatedSources)) + "))"
	if len(nonLocal) == 0 {
		return localSQL, stringArgs(localCreatedSources)
	}
	return "(s.source IN (" + bindMarks(len(nonLocal)) + ") OR " + localSQL + ")", append(stringArgs(nonLocal), stringArgs(localCreatedSources)...)
}

// Bound arguments of strings
func stringArgs(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// Short preview a listing shows from the raw preview column
func shapePreview(raw string) string {
	text := strings.TrimFunc(raw, previewSpace)
	if text == "" {
		return ""
	}
	text = strings.NewReplacer("\n", " ", "\r", " ").Replace(text)
	if described, ok := describeSkillInvocation(text); ok {
		text = described
	} else {
		text, _, _ = strings.Cut(text, skillExcerptJoint)
	}
	if utf8.RuneCountInString(text) > previewMaxChars {
		return string([]rune(text)[:previewMaxChars]) + "..."
	}
	return text
}

// Whitespace preview shaping trims and splits on, the ASCII information separators included
func previewSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x1c, 0x1d, 0x1e, 0x1f, 0x85, 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

// Words of text split on preview whitespace
func previewFields(text string) []string { return strings.FieldsFunc(text, previewSpace) }

// Markers inside a skill scaffold that bound the typed instruction
const (
	singleSkillMarker      = "The full skill content is loaded below.]"
	singleSkillInstruction = "The user has provided the following instruction alongside the skill invocation: "
	skillRuntimeNote       = "\n\n[Runtime note:"
	skillBundleMarker      = " skill bundle,"
	bundleUserInstruction  = "\nUser instruction: "
	bundleFirstSkillBlock  = "\n\n[Loaded as part of the "
	skillDirNoteEnd        = "then run them with the terminal tool using the absolute path."
	supportingFilesEnd     = "/scripts/foo.js`)."
)

// Blocks an auto-load payload may append after its footer before the typed request
var trailingBlockEnds = [][2]string{{"[Skill config (", "\n]"}, {"[Skill setup note: ", "]\n\n"}, {"[This skill has supporting files", supportingFilesEnd}}

// Turn a skill scaffold back into what the user typed, false when content is no scaffold
func describeSkillInvocation(content string) (string, bool) {
	if strings.HasPrefix(content, autoLoadPrefix) {
		return describeAutoLoadedSkill(content)
	}
	if !strings.HasPrefix(content, skillInvocationPrefix) {
		return "", false
	}
	name := ""
	if rest, ok := strings.CutPrefix(content, skillInvocationPrefix+`"`); ok {
		if end := strings.IndexByte(rest, '"'); end >= 0 {
			name = strings.TrimFunc(rest[:end], previewSpace)
		}
	}
	label := name
	if !strings.HasPrefix(name, "/") {
		label = "/" + name
	}
	if instruction, ok := skillInstruction(content); ok {
		before, _, _ := strings.Cut(instruction, skillExcerptJoint)
		if joined := strings.Join(previewFields(before), " "); joined != "" {
			if name != "" {
				return label + " — " + joined, true
			}
			return joined, true
		}
	}
	if name != "" {
		return label, true
	}
	return "", false
}

// Instruction a skill scaffold carried, false for a bare invocation
func skillInstruction(content string) (string, bool) {
	if strings.Contains(content, skillBundleMarker) {
		return cutAfter(content, bundleUserInstruction, bundleFirstSkillBlock, strings.Index(content, bundleUserInstruction))
	}
	if strings.Contains(content, singleSkillMarker) {
		return cutAfter(content, singleSkillInstruction, skillRuntimeNote, strings.LastIndex(content, singleSkillInstruction))
	}
	return "", false
}

// Trimmed text between a marker found at index and the stop marker, false when absent or empty
func cutAfter(message, marker, stop string, index int) (string, bool) {
	if index < 0 {
		return "", false
	}
	text, _, _ := strings.Cut(message[index+len(marker):], stop)
	text = strings.TrimFunc(text, previewSpace)
	return text, text != ""
}

// Typed request after the last payload footer of an auto-load scaffold, the skill label when none follows
func describeAutoLoadedSkill(content string) (string, bool) {
	name := ""
	if end := strings.IndexByte(content[len(autoLoadPrefix):], '"'); end >= 0 {
		name = content[len(autoLoadPrefix) : len(autoLoadPrefix)+end]
	}
	bare := func() (string, bool) {
		if name != "" {
			return "/" + name, true
		}
		return "", false
	}
	footer := strings.LastIndex(content, skillDirNoteEnd)
	if footer < 0 {
		return bare()
	}
	tail := strings.TrimLeftFunc(content[footer+len(skillDirNoteEnd):], previewSpace)
	for {
		blockEnd := ""
		for _, b := range trailingBlockEnds {
			if strings.HasPrefix(tail, b[0]) {
				blockEnd = b[1]
				break
			}
		}
		if blockEnd == "" {
			break
		}
		if cut := strings.Index(tail, blockEnd); cut >= 0 {
			tail = strings.TrimLeftFunc(tail[cut+len(blockEnd):], previewSpace)
		} else {
			tail = ""
		}
	}
	if strings.TrimFunc(tail, previewSpace) == "" {
		return bare()
	}
	joined := strings.Join(previewFields(tail), " ")
	return joined, joined != ""
}
