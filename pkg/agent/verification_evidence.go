package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	maxVerifyOutputSummaryChars      = 2000
	maxVerifyEvidenceAgeDays         = 30
	maxVerifyEventsPerSessionRoot    = 100
	maxVerifyTotalUnreferencedEvents = 10_000
	verifySchemaVersion              = 1
	maxUnattributedRuns              = 256
	verifyLedgerFile                 = "verification_evidence.db"
	adHocCanonicalCommand            = "ad-hoc verification script"
	verifyRunCanonicalCommand        = "icebath verify"
)

var (
	adHocScriptNamePrefixes = []string{"icebath-verify-", "icebath-ad-hoc-"}
	windowsExeSuffixRe      = regexp.MustCompile(`(?i)\.(?:exe|bat|cmd)(\n?)$`)
	interpreterNameRe       = regexp.MustCompile(`^(?:python3|python|node|bash|ruby|perl|py|sh)(?:[0-9]+(?:\.[0-9]+)*)?\n?$`)
	interpreterVersionRe    = regexp.MustCompile(`[0-9.]+(\n?)$`)
	interpreterFlagRe       = regexp.MustCompile(`^-(?:[IBuEsSOd]+|3(?:\.\p{Nd}+)?|[WX].*)\n?$`)
	timeoutDurationRe       = regexp.MustCompile(`^\p{Nd}+(?:\.\p{Nd}+)?[smhd]?\n?$`)
	leadingDotSlashRe       = regexp.MustCompile(`^(?:\./)+`)
	verifyTargetExtensions  = []string{".py", ".js", ".jsx", ".ts", ".tsx", ".rs", ".go", ".java"}
	verifyTargetPrefixes    = []string{"test_", "tests", "spec", "__tests__"}
	verifyKindKeywords      = []struct {
		words []string
		kind  string
	}{
		{[]string{"lint", "eslint", "ruff"}, "lint"},
		{[]string{"typecheck", "tsc", "mypy", "pyright", "ty"}, "typecheck"},
		{[]string{"build"}, "build"},
		{[]string{"fmt", "format"}, "format"},
	}
	pytestSpellings = [][]string{
		{"python", "-m", "pytest"}, {"python3", "-m", "pytest"},
		{"uv", "run", "pytest"}, {"poetry", "run", "pytest"}, {"pipenv", "run", "pytest"},
	}
	wrapperOptions = map[string]struct{ flags, valued []string }{
		"timeout": {[]string{"-p", "--preserve-status", "-f", "--foreground", "-v", "--verbose"},
			[]string{"-s", "--signal", "-k", "--kill-after"}},
		"nice":   {nil, []string{"-n", "--adjustment"}},
		"nohup":  {nil, nil},
		"stdbuf": {nil, []string{"-i", "-o", "-e", "--input", "--output", "--error"}},
	}
	verifySchemaDDL = []string{
		`
        CREATE TABLE IF NOT EXISTS meta (
            key TEXT PRIMARY KEY,
            value TEXT NOT NULL
        )
        `,
		`
        CREATE TABLE IF NOT EXISTS verification_events (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            created_at TEXT NOT NULL,
            session_id TEXT NOT NULL,
            cwd TEXT NOT NULL,
            root TEXT NOT NULL,
            command TEXT NOT NULL,
            canonical_command TEXT NOT NULL,
            kind TEXT NOT NULL,
            scope TEXT NOT NULL,
            status TEXT NOT NULL,
            exit_code INTEGER NOT NULL,
            output_summary TEXT NOT NULL
        )
        `,
		`
        CREATE TABLE IF NOT EXISTS verification_state (
            session_id TEXT NOT NULL,
            root TEXT NOT NULL,
            last_event_id INTEGER,
            last_edit_at TEXT,
            changed_paths_json TEXT NOT NULL DEFAULT '[]',
            PRIMARY KEY (session_id, root)
        )
        `,
		`
        CREATE INDEX IF NOT EXISTS idx_verification_events_session_root
        ON verification_events(session_id, root, id DESC)
        `,
	}
)

// Serializes every ledger transaction in the process
var verifyLedgerMu sync.Mutex

// Raw connector of the ledger database, swapped by descriptor leak tests
var verifyLedgerOpener state.SQLiteOpener = state.ConnectSQLite

// Current instant, swapped by tests that age ledger rows
var verifyLedgerNow = func() time.Time { return time.Now().UTC() }

// Latest verify command run per session and root whose own exit status the shell hid, bounded
var unattributedRuns = struct {
	sync.Mutex
	runs map[[2]string]unattributedRun
	seq  uint64
}{runs: map[[2]string]unattributedRun{}}

type unattributedRun struct {
	command, at string
	seq         uint64
}

// One top level shell command with the control operator after it, empty for none
type shellSegment struct {
	tokens            []string
	followingOperator string
}

// VerificationEvidence is a classified command result worth recording
type VerificationEvidence struct {
	Command          string
	CanonicalCommand string
	Kind             string
	Scope            string
	Status           string
	ExitCode         int
	Cwd              string
	Root             string
	SessionID        string
	OutputSummary    string
}

// Field mapping of the evidence keyed by the ledger column names
func (e VerificationEvidence) fields() map[string]any {
	return map[string]any{
		"command": e.Command, "canonical_command": e.CanonicalCommand, "kind": e.Kind, "scope": e.Scope,
		"status": e.Status, "exit_code": e.ExitCode, "cwd": e.Cwd, "root": e.Root,
		"session_id": e.SessionID, "output_summary": e.OutputSummary,
	}
}

// Current UTC time in Python isoformat
func verifyUTCNow() string { return pydatetime.ISOFormatUTC(verifyLedgerNow()) }

// Ledger database file under the active home
func verifyLedgerPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), verifyLedgerFile)
}

// Whether verify on stop is on, the only consumer the ledger records for
func verifyLedgerEnabled(ctx context.Context, env *tools.Env) bool {
	return verifyOnStopEnabled(ctx, env, nil)
}

// Opens the ledger database with its schema applied
func connectVerifyLedger(ctx context.Context) (state.SQLiteConn, error) {
	return state.OpenDB(ctx, verifyLedgerPath(ctx), verifyLedgerFile,
		state.WithInitialize(ensureVerifySchema), state.WithOpener(verifyLedgerOpener))
}

// Runs body in one ledger transaction that always closes its connection
func verifyLedgerTransaction(ctx context.Context, body func(conn state.SQLiteConn) error) error {
	conn, err := connectVerifyLedger(ctx)
	if err != nil {
		return err
	}
	return state.Transaction(ctx, conn, false, body)
}

// Creates the ledger tables and index and stamps the schema version
func ensureVerifySchema(ctx context.Context, conn state.SQLiteConn) error {
	for _, ddl := range verifySchemaDDL {
		if _, err := conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	_, err := conn.ExecContext(ctx, "INSERT OR REPLACE INTO meta(key, value) VALUES ('schema_version', ?)",
		fmt.Sprint(verifySchemaVersion))
	return err
}

// Top level shell commands tokenized with their control operators, empty for anything unparseable
func splitShellSegments(command string, posix bool) []shellSegment {
	type rawSegment struct{ raw, operator string }
	var raws []rawSegment
	start, index := 0, 0
	var quote byte
	escaped := false
	for index < len(command) {
		char := command[index]
		if escaped || (char == '\\' && quote != '\'') {
			escaped = !escaped
			index++
			continue
		}
		if quote != 0 || char == '\'' || char == '"' {
			switch {
			case char == quote:
				quote = 0
			case quote == 0:
				quote = char
			}
			index++
			continue
		}
		operator := ""
		rest := command[index:]
		switch {
		case strings.HasPrefix(rest, "&&") || strings.HasPrefix(rest, "||") || strings.HasPrefix(rest, "|&"):
			operator = rest[:2]
		case char == '\n':
			operator = ";"
		case char == ';' || char == '|' ||
			(char == '&' && (index == 0 || (command[index-1] != '<' && command[index-1] != '>')) && !strings.HasPrefix(rest, "&>")):
			operator = string(char)
		}
		if operator == "" {
			index++
			continue
		}
		raw := pystr.Strip(command[start:index])
		if raw == "" {
			return nil
		}
		raws = append(raws, rawSegment{raw, operator})
		if char == '\n' {
			index++
		} else {
			index += len(operator)
		}
		start = index
	}
	if quote != 0 || escaped {
		return nil
	}
	if trailing := pystr.Strip(command[start:]); trailing != "" {
		raws = append(raws, rawSegment{trailing, ""})
	} else if len(raws) > 0 && raws[len(raws)-1].operator != ";" {
		return nil
	}
	segments := make([]shellSegment, 0, len(raws))
	for _, r := range raws {
		var tokens []string
		var err error
		if posix {
			tokens, err = pyshlex.SplitPOSIX(r.raw, false)
		} else {
			tokens, err = pyshlex.SplitNonPOSIX(r.raw)
		}
		if err != nil {
			return nil
		}
		segments = append(segments, shellSegment{tokens, r.operator})
	}
	for _, s := range segments {
		if len(s.tokens) == 0 {
			return nil
		}
	}
	return segments
}

// Own exit status of the matched segment when the shell proves it, false otherwise
func segmentStatus(segments []shellSegment, matchIndex, exitCode int, pipelineStatus []int) (int, bool) {
	if len(segments) == 0 || matchIndex < 0 || matchIndex >= len(segments) {
		return 0, false
	}
	for _, s := range segments {
		if s.followingOperator == "&" {
			return 0, false
		}
	}
	sequenceStart := 0
	for i, s := range segments[:len(segments)-1] {
		if s.followingOperator == ";" {
			sequenceStart = i + 1
		}
	}
	if matchIndex < sequenceStart {
		return 0, false
	}
	sequence := segments[sequenceStart : len(segments)-1]
	operators := map[string]bool{}
	for _, s := range sequence {
		operators[s.followingOperator] = true
	}
	if operators["||"] {
		return 0, false
	}
	if operators["|"] || operators["|&"] {
		pipelineStart := sequenceStart
		for index, s := range sequence {
			if s.followingOperator != "|" && s.followingOperator != "|&" {
				pipelineStart = sequenceStart + index + 1
			}
		}
		chainRan := !operators["&&"] || exitCode == 0
		if chainRan && len(pipelineStatus) >= 2 && matchIndex == pipelineStart {
			return pipelineStatus[0], true
		}
		return 0, false
	}
	if len(operators) == 0 {
		return exitCode, true
	}
	if len(operators) == 1 && operators["&&"] && exitCode == 0 {
		return exitCode, true
	}
	return 0, false
}

// Tokens of a canonical command with leading dot slashes removed, empty when unparseable
func canonicalTokens(canonical string) []string {
	split, err := pyshlex.SplitPOSIX(canonical, false)
	if err != nil {
		return nil
	}
	out := []string{}
	for _, t := range split {
		if t != "" {
			out = append(out, leadingDotSlashRe.ReplaceAllString(pystr.Strip(t), ""))
		}
	}
	return out
}

// Tokens one wrapper option spans, two for a separate value, zero when unknown
func wrapperOptionWidth(token string, flags, valued []string) int {
	for _, f := range flags {
		if token == f {
			return 1
		}
	}
	for _, v := range valued {
		if token == v {
			return 2
		}
	}
	for _, v := range valued {
		if (strings.HasPrefix(v, "--") && strings.HasPrefix(token, v+"=")) || (!strings.HasPrefix(v, "--") && strings.HasPrefix(token, v)) {
			return 1
		}
	}
	return 0
}

// Index of the child command after a status preserving wrapper, false unless the wrapper runs it
func skipWrapper(tokens []string, i int) (int, bool) {
	opts := wrapperOptions[tokens[i]]
	timeout := tokens[i] == "timeout"
	i++
	for i < len(tokens) && strings.HasPrefix(tokens[i], "-") && tokens[i] != "--" {
		width := wrapperOptionWidth(tokens[i], opts.flags, opts.valued)
		if width == 0 {
			return 0, false
		}
		i += width
	}
	if i < len(tokens) && tokens[i] == "--" {
		i++
	}
	if timeout {
		if i >= len(tokens) || !timeoutDurationRe.MatchString(tokens[i]) {
			return 0, false
		}
		i++
	}
	return i, true
}

// Tokens without env assignments, command time noglob prefixes and child running wrappers
func stripCommandPrefix(tokens []string) []string {
	i := 0
	if len(tokens) > 0 && tokens[0] == "env" {
		i = 1
	}
	for i < len(tokens) && strings.Contains(tokens[i], "=") && !strings.HasPrefix(tokens[i], "-") {
		i++
	}
	for i < len(tokens) {
		if tokens[i] == "command" || tokens[i] == "time" || tokens[i] == "noglob" {
			i++
			continue
		}
		if _, ok := wrapperOptions[tokens[i]]; ok {
			if child, ok := skipWrapper(tokens, i); ok {
				i = child
				continue
			}
		}
		break
	}
	if i >= len(tokens) {
		return []string{}
	}
	return append([]string{}, tokens[i:]...)
}

// Basename of a token with Windows separators read as slashes
func verifyTokenBasename(token string) string {
	name := strings.ReplaceAll(token, "\\", "/")
	return name[strings.LastIndex(name, "/")+1:]
}

// Interpreter family a token names wherever it lives, empty when it is not an interpreter
func interpreterFamilyOf(token string) string {
	name := leadingDotSlashRe.ReplaceAllString(verifyTokenBasename(token), "")
	name = windowsExeSuffixRe.ReplaceAllString(name, "$1")
	if !interpreterNameRe.MatchString(name) {
		return ""
	}
	family := interpreterVersionRe.ReplaceAllString(name, "$1")
	if family == "py" {
		return "python"
	}
	return family
}

// Whether a token is a Python interpreter flag that still runs the module after it
func isHarmlessInterpreterFlag(token string) bool { return interpreterFlagRe.MatchString(token) }

// Comparison form with leading dot slashes gone, interpreters collapsed and harmless Python flags dropped
func normalizeVerifyTokens(tokens []string) []string {
	normalized := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); {
		family := interpreterFamilyOf(tokens[i])
		if family != "" {
			normalized = append(normalized, family)
		} else {
			normalized = append(normalized, leadingDotSlashRe.ReplaceAllString(tokens[i], ""))
		}
		i++
		if family == "python" {
			for i < len(tokens) && isHarmlessInterpreterFlag(tokens[i]) {
				if tokens[i] == "-W" || tokens[i] == "-X" {
					i += 2
				} else {
					i++
				}
			}
		}
	}
	return normalized
}

// Command spellings equivalent to a detected canonical command
func equivalentNeedles(needle []string) [][]string {
	candidates := [][]string{needle}
	if len(needle) >= 3 && needle[1] == "run" && (needle[0] == "npm" || needle[0] == "pnpm" || needle[0] == "yarn" || needle[0] == "bun") {
		candidates = append(candidates, []string{needle[0], needle[2]})
	}
	if len(needle) == 1 && strings.Contains(needle[0], "/") {
		candidates = append(candidates, []string{"bash", needle[0]}, []string{"sh", needle[0]})
	}
	if len(needle) == 1 && needle[0] == "pytest" {
		candidates = append(candidates, pytestSpellings...)
	}
	return candidates
}

// First detected command a segment runs, with its trailing args and own status when attributed
type canonicalMatch struct {
	canonical  string
	trailing   []string
	status     int
	attributed bool
}

// Detected command the shell command runs, also matching hidden status runs unless attributed is set
func findCanonicalMatch(command string, canonicalCommands []string, exitCode int, pipelineStatus []int, attributed bool) *canonicalMatch {
	segments := splitShellSegments(command, true)
	for _, canonical := range canonicalCommands {
		needle := canonicalTokens(canonical)
		if len(needle) == 0 {
			continue
		}
		for index, segment := range segments {
			candidateTokens := normalizeVerifyTokens(stripCommandPrefix(segment.tokens))
			status, known := segmentStatus(segments, index, exitCode, pipelineStatus)
			if !known && attributed {
				continue
			}
			for _, candidate := range equivalentNeedles(needle) {
				candidate = normalizeVerifyTokens(candidate)
				if len(candidateTokens) >= len(candidate) && equalStrings(candidateTokens[:len(candidate)], candidate) {
					return &canonicalMatch{canonical, candidateTokens[len(candidate):], status, known}
				}
			}
		}
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Evidence kind of a canonical command, lint typecheck build format check or test
func kindForCommand(canonical string) string {
	lowered := pystr.Lower(canonical)
	for _, group := range verifyKindKeywords {
		for _, w := range group.words {
			if strings.Contains(lowered, w) {
				return group.kind
			}
		}
	}
	if strings.Contains(lowered, "check") && !strings.Contains(lowered, "test") {
		return "check"
	}
	return "test"
}

// Whether a trailing argument names a test target rather than an option
func looksLikeTarget(arg string) bool {
	if arg == "" || strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	if strings.Contains(arg, "/") || strings.Contains(arg, "\\") || strings.Contains(arg, "::") {
		return true
	}
	for _, ext := range verifyTargetExtensions {
		if strings.HasSuffix(arg, ext) {
			return true
		}
	}
	for _, prefix := range verifyTargetPrefixes {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

// Whether absolute path token resolves to base or beneath it, false on any failure
func isUnderResolved(token, base string) bool {
	if base == "" {
		return false
	}
	p, err := pypath.PathExpanduser(token)
	if err != nil || !pypath.IsAbs(p) {
		return false
	}
	resolved, err := pypath.Resolve(p)
	if err != nil {
		return false
	}
	b, err := pypath.PathExpanduser(base)
	if err != nil {
		return false
	}
	basePath, err := pypath.Resolve(b)
	if err != nil {
		return false
	}
	if resolved == basePath {
		return true
	}
	for _, parent := range pypath.PathParents(resolved) {
		if parent == basePath {
			return true
		}
	}
	return false
}

// Whether a token is an ad hoc verify script by prefix, under the temp dir and outside the repo
var isTempScriptPath = func(token, root string) (bool, error) {
	p, err := pypath.PathExpanduser(token)
	if err != nil {
		return false, nil
	}
	name := pypath.PathName(p)
	prefixed := false
	for _, prefix := range adHocScriptNamePrefixes {
		prefixed = prefixed || strings.HasPrefix(name, prefix)
	}
	if !prefixed {
		return false, nil
	}
	tempDir, err := pypath.Gettempdir()
	if err != nil {
		return false, err
	}
	return isUnderResolved(token, tempDir) && !isUnderResolved(token, root), nil
}

// Whether a command word names a shell interpreter at any path or version
func isInterpreterToken(token string) bool {
	return interpreterNameRe.MatchString(windowsExeSuffixRe.ReplaceAllString(verifyTokenBasename(token), "$1"))
}

// Arguments after an ad hoc temp script the segment runs, false when it runs none
func adHocScriptArgs(tokens []string, root string) ([]string, bool, error) {
	candidateTokens := stripCommandPrefix(tokens)
	if len(candidateTokens) == 0 {
		return nil, false, nil
	}
	command := candidateTokens[0]
	if ok, err := isTempScriptPath(command, root); err != nil || ok {
		return candidateTokens[1:], ok, err
	}
	if pypath.PathName(command) == "env" && len(candidateTokens) > 1 && isInterpreterToken(candidateTokens[1]) {
		candidateTokens, command = candidateTokens[1:], candidateTokens[1]
	}
	if isInterpreterToken(command) {
		for idx := 1; idx < len(candidateTokens); idx++ {
			token := candidateTokens[idx]
			if ok, err := isTempScriptPath(token, root); err != nil || ok {
				return candidateTokens[idx+1:], ok, err
			}
			if token != "--" && !strings.HasPrefix(token, "-") {
				return nil, false, nil
			}
		}
	}
	return nil, false, nil
}

// Trailing args of an ad hoc script run whose status the shell proves, retried without POSIX splitting
func findAdHocMatch(command, root string, exitCode int) ([]string, bool, error) {
	for _, posix := range []bool{true, false} {
		segments := splitShellSegments(command, posix)
		for index, segment := range segments {
			trailing, ok, err := adHocScriptArgs(segment.tokens, root)
			if err != nil {
				return nil, false, err
			}
			if _, known := segmentStatus(segments, index, exitCode, nil); ok && known {
				return trailing, true, nil
			}
		}
	}
	return nil, false, nil
}

// Stripped output capped to head and tail with the omitted count between
func summarizeVerifyOutput(output string) string {
	runes := pystr.Runes(pystr.Strip(output))
	if len(runes) <= maxVerifyOutputSummaryChars {
		return pystr.FromRunes(runes)
	}
	head := maxVerifyOutputSummaryChars / 3
	return fmt.Sprintf("%s\n... [%d chars omitted] ...\n%s", pystr.FromRunes(runes[:head]),
		len(runes)-maxVerifyOutputSummaryChars, pystr.FromRunes(runes[len(runes)-(maxVerifyOutputSummaryChars-head):]))
}

// Bounds ledger growth per session root, by age and in total without dropping referenced events
func pruneOldVerifyEvents(ctx context.Context, conn state.SQLiteConn, sessionID, root string) error {
	cutoff := pydatetime.ISOFormatUTC(verifyLedgerNow().Add(-maxVerifyEvidenceAgeDays * 24 * time.Hour))
	statements := []struct {
		query string
		args  []any
	}{
		{"DELETE FROM verification_events WHERE session_id = ? AND root = ? AND id NOT IN (" +
			" SELECT id FROM verification_events WHERE session_id = ? AND root = ?" +
			" ORDER BY id DESC LIMIT ?)", []any{sessionID, root, sessionID, root, maxVerifyEventsPerSessionRoot}},
		{"DELETE FROM verification_state" +
			" WHERE (last_edit_at IS NOT NULL AND last_edit_at < ?)" +
			" OR (last_edit_at IS NULL AND last_event_id IN (" +
			" SELECT id FROM verification_events WHERE created_at < ?))", []any{cutoff, cutoff}},
		{"DELETE FROM verification_events WHERE created_at < ? AND id NOT IN (" +
			" SELECT last_event_id FROM verification_state WHERE last_event_id IS NOT NULL)", []any{cutoff}},
		{"DELETE FROM verification_events WHERE id NOT IN (" +
			" SELECT id FROM verification_events ORDER BY id DESC LIMIT ?)" +
			" AND id NOT IN (" +
			" SELECT last_event_id FROM verification_state WHERE last_event_id IS NOT NULL)", []any{maxVerifyTotalUnreferencedEvents}},
	}
	for _, s := range statements {
		if _, err := conn.ExecContext(ctx, s.query, s.args...); err != nil {
			return err
		}
	}
	return nil
}

// Workspace facts of cwd, nil when detection finds nothing
func verifyProjectFacts(cwd string) map[string]any {
	facts := tools.ProjectFactsFor(cwd)
	if len(facts) == 0 {
		return nil
	}
	return facts
}

// Workspace root from the facts, else the resolved cwd
func verifyRootFor(facts map[string]any, cwd string) (string, error) {
	if root := facts["root"]; pyTruthy(root) {
		return pyrepr.Str(root), nil
	}
	return resolveVerifyCwd(cwd)
}

// Resolved cwd, the current directory when empty
func resolveVerifyCwd(cwd string) (string, error) {
	if cwd == "" {
		cwd = "."
	}
	return pypath.Resolve(cwd)
}

// Decoded changed paths JSON, an empty list when it does not decode
func loadChangedPaths(raw any) any {
	text, ok := raw.(string)
	if raw == nil || (ok && text == "") {
		text, ok = "[]", true
	}
	if !ok {
		return []any{}
	}
	v, err := pyjson.Loads(text)
	if err != nil {
		return []any{}
	}
	return v
}

// Session key of a ledger record, default when absent
func verifySessionKey(sessionID string) string {
	if sessionID == "" {
		return "default"
	}
	return sessionID
}

// Detected verify command of the shell command in the workspace of cwd as evidence, nil when it is none
func classifyVerificationCommand(command, cwd, sessionID string, exitCode int, output string, pipelineStatus []int) (*VerificationEvidence, error) {
	if command == "" {
		return nil, nil
	}
	facts := verifyProjectFacts(cwd)
	if facts == nil {
		return nil, nil
	}
	return classifyInWorkspace(command, facts, cwd, sessionID, exitCode, output, pipelineStatus)
}

// Evidence of a detected command or of an ad hoc script when the project detects no command
func classifyInWorkspace(command string, facts map[string]any, cwd, sessionID string, exitCode int, output string, pipelineStatus []int) (*VerificationEvidence, error) {
	verifyCommands, _ := facts["verifyCommands"].([]string)
	var canonical, kind, scope string
	var status int
	if match := findCanonicalMatch(command, verifyCommands, exitCode, pipelineStatus, true); match != nil && match.attributed {
		canonical, status = match.canonical, match.status
		kind, scope = kindForCommand(canonical), "full"
		for _, arg := range match.trailing {
			if looksLikeTarget(arg) {
				scope = "targeted"
				break
			}
		}
	} else {
		if len(verifyCommands) > 0 {
			return nil, nil
		}
		root, _ := facts["root"].(string)
		_, ok, err := findAdHocMatch(command, root, exitCode)
		if err != nil || !ok {
			return nil, err
		}
		canonical, kind, scope, status = adHocCanonicalCommand, "ad_hoc", "targeted", exitCode
	}
	resolvedCwd, err := resolveVerifyCwd(cwd)
	if err != nil {
		return nil, err
	}
	root, err := verifyRootFor(facts, cwd)
	if err != nil {
		return nil, err
	}
	result := "failed"
	if status == 0 {
		result = "passed"
	}
	return &VerificationEvidence{Command: command, CanonicalCommand: canonical, Kind: kind, Scope: scope,
		Status: result, ExitCode: exitCode, Cwd: resolvedCwd, Root: root, SessionID: verifySessionKey(sessionID),
		OutputSummary: summarizeVerifyOutput(output)}, nil
}

// Records a foreground terminal result that is verification evidence, remembering hidden status verify runs
func recordTerminalResult(ctx context.Context, env *tools.Env, command, cwd, sessionID string, exitCode int, output string, pipelineStatus []int) (map[string]any, error) {
	if !verifyLedgerEnabled(ctx, env) || command == "" {
		return nil, nil
	}
	facts := verifyProjectFacts(cwd)
	if facts == nil {
		return nil, nil
	}
	root, err := verifyRootFor(facts, cwd)
	if err != nil {
		return nil, err
	}
	key := [2]string{verifySessionKey(sessionID), root}
	evidence, err := classifyInWorkspace(command, facts, cwd, sessionID, exitCode, output, pipelineStatus)
	if err != nil {
		return nil, err
	}
	if evidence != nil {
		return insertVerifyEvidence(ctx, *evidence)
	}
	verifyCommands, _ := facts["verifyCommands"].([]string)
	if findCanonicalMatch(command, verifyCommands, exitCode, nil, false) != nil {
		rememberUnattributedRun(key, command, verifyUTCNow())
	}
	return nil, nil
}

// Stores the newest unattributed run of a key, evicting the oldest past the bound
func rememberUnattributedRun(key [2]string, command, at string) {
	unattributedRuns.Lock()
	defer unattributedRuns.Unlock()
	unattributedRuns.seq++
	unattributedRuns.runs[key] = unattributedRun{command, at, unattributedRuns.seq}
	for len(unattributedRuns.runs) > maxUnattributedRuns {
		var oldest [2]string
		var oldestSeq uint64
		first := true
		for k, run := range unattributedRuns.runs {
			if first || run.seq < oldestSeq {
				oldest, oldestSeq, first = k, run.seq, false
			}
		}
		delete(unattributedRuns.runs, oldest)
	}
}

// Latest unattributed verify run in root unless evidence recorded at since came after it, empty for none
func lastUnattributedRun(sessionID, root, since any) string {
	sid := "default"
	if pyTruthy(sessionID) {
		sid = pyrepr.Str(sessionID)
	}
	unattributedRuns.Lock()
	run, ok := unattributedRuns.runs[[2]string{sid, pyrepr.Str(root)}]
	unattributedRuns.Unlock()
	if !ok || (pyTruthy(since) && pyrepr.Str(since) >= run.at) {
		return ""
	}
	return run.command
}

// Records a completed icebath verify run as evidence for the workspace of root
func recordVerifyRun(ctx context.Context, env *tools.Env, root, sessionID string, ok bool, command, scope, output string) (map[string]any, error) {
	if !verifyLedgerEnabled(ctx, env) {
		return nil, nil
	}
	resolved, err := pypath.Resolve(root)
	if err != nil {
		return nil, err
	}
	workspace := resolved
	if r := verifyProjectFacts(root)["root"]; pyTruthy(r) {
		workspace = pyrepr.Str(r)
	}
	if scope != "full" && scope != "targeted" {
		scope = "full"
	}
	status, exitCode := "failed", 1
	if ok {
		status, exitCode = "passed", 0
	}
	return insertVerifyEvidence(ctx, VerificationEvidence{Command: command, CanonicalCommand: verifyRunCanonicalCommand,
		Kind: "verify", Scope: scope, Status: status, ExitCode: exitCode, Cwd: resolved, Root: workspace,
		SessionID: verifySessionKey(sessionID), OutputSummary: summarizeVerifyOutput(output)})
}

// Inserts a classified evidence row and repoints the workspace state at it
func insertVerifyEvidence(ctx context.Context, e VerificationEvidence) (map[string]any, error) {
	createdAt := verifyUTCNow()
	unattributedRuns.Lock()
	delete(unattributedRuns.runs, [2]string{e.SessionID, e.Root})
	unattributedRuns.Unlock()
	var eventID int64
	verifyLedgerMu.Lock()
	defer verifyLedgerMu.Unlock()
	err := verifyLedgerTransaction(ctx, func(conn state.SQLiteConn) error {
		res, err := conn.ExecContext(ctx, "INSERT INTO verification_events("+
			" created_at, session_id, cwd, root, command, canonical_command,"+
			" kind, scope, status, exit_code, output_summary"+
			") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			createdAt, e.SessionID, e.Cwd, e.Root, e.Command, e.CanonicalCommand, e.Kind, e.Scope,
			e.Status, e.ExitCode, e.OutputSummary)
		if err != nil {
			return err
		}
		if eventID, err = res.LastInsertId(); err != nil {
			return errors.New("verification event insert did not return an id")
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO verification_state("+
			" session_id, root, last_event_id, last_edit_at, changed_paths_json"+
			") VALUES (?, ?, ?, NULL, '[]')"+
			" ON CONFLICT(session_id, root) DO UPDATE SET"+
			" last_event_id = excluded.last_event_id,"+
			" last_edit_at = NULL,"+
			" changed_paths_json = '[]'", e.SessionID, e.Root, eventID); err != nil {
			return err
		}
		if e.Kind == "verify" && e.Status == "passed" && e.Scope == "full" {
			if _, err := conn.ExecContext(ctx, "UPDATE verification_state"+
				" SET last_event_id = ?, last_edit_at = NULL, changed_paths_json = '[]'"+
				" WHERE root = ? AND session_id != ?", eventID, e.Root, e.SessionID); err != nil {
				return err
			}
		}
		return pruneOldVerifyEvents(ctx, conn, e.SessionID, e.Root)
	})
	if err != nil {
		return nil, err
	}
	out := e.fields()
	out["id"] = int(eventID)
	out["created_at"] = createdAt
	return out, nil
}

// Marks verification evidence of the workspace stale after a successful file edit
func markWorkspaceEdited(ctx context.Context, env *tools.Env, sessionID, cwd string, paths []string) (map[string]any, error) {
	if !verifyLedgerEnabled(ctx, env) {
		return nil, nil
	}
	facts := verifyProjectFacts(cwd)
	if facts == nil {
		return nil, nil
	}
	sid := verifySessionKey(sessionID)
	root, err := verifyRootFor(facts, cwd)
	if err != nil {
		return nil, err
	}
	unique := map[string]bool{}
	for _, p := range paths {
		if p != "" {
			unique[p] = true
		}
	}
	changedPaths := make([]string, 0, len(unique))
	for p := range unique {
		changedPaths = append(changedPaths, p)
	}
	sort.Strings(changedPaths)
	editedAt := verifyUTCNow()
	verifyLedgerMu.Lock()
	defer verifyLedgerMu.Unlock()
	err = verifyLedgerTransaction(ctx, func(conn state.SQLiteConn) error {
		var raw any
		existing := any([]any{})
		switch err := conn.QueryRowContext(ctx, "SELECT changed_paths_json FROM verification_state WHERE session_id = ? AND root = ?",
			sid, root).Scan(&raw); {
		case err == nil:
			existing = loadChangedPaths(raw)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		merged, err := mergeChangedPaths(existing, changedPaths)
		if err != nil {
			return err
		}
		encoded, err := pyjson.Dumps(merged)
		if err != nil {
			return err
		}
		_, err = conn.ExecContext(ctx, "INSERT INTO verification_state("+
			" session_id, root, last_event_id, last_edit_at, changed_paths_json"+
			") VALUES (?, ?, NULL, ?, ?)"+
			" ON CONFLICT(session_id, root) DO UPDATE SET"+
			" last_edit_at = excluded.last_edit_at,"+
			" changed_paths_json = excluded.changed_paths_json", sid, root, editedAt, encoded)
		return err
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"session_id": sid, "root": root, "last_edit_at": editedAt, "changed_paths": changedPaths}, nil
}

// Sorted union of the recorded and new changed paths, the last two hundred, raising as Python sets and sorting do
func mergeChangedPaths(existing any, changed []string) ([]any, error) {
	var items []any
	switch v := existing.(type) {
	case []any:
		items = append(items, v...)
	case *pyjson.Object:
		for _, k := range v.Keys {
			items = append(items, k)
		}
	case string:
		for _, r := range pystr.Runes(v) {
			items = append(items, pystr.FromRunes([]rune{r}))
		}
	default:
		return nil, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("'%s' object is not iterable", pyrepr.TypeName(existing))}
	}
	for _, p := range changed {
		items = append(items, p)
	}
	var merged []any
	for _, item := range items {
		switch item.(type) {
		case []any, *pyjson.Object:
			return nil, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(item))}
		}
		seen := false
		for _, m := range merged {
			if pyrepr.Equal(m, item) {
				seen = true
				break
			}
		}
		if !seen {
			merged = append(merged, item)
		}
	}
	var sortErr error
	sort.SliceStable(merged, func(i, j int) bool {
		less, err := pyOrder(merged[i], merged[j], "<")
		if err != nil && sortErr == nil {
			sortErr = err
		}
		return less
	})
	if sortErr != nil {
		return nil, sortErr
	}
	if len(merged) > 200 {
		merged = merged[len(merged)-200:]
	}
	if merged == nil {
		merged = []any{}
	}
	return merged, nil
}

// Python a < b or a > b over JSON scalars, a TypeError for values Python cannot order
func pyOrder(a, b any, op string) (bool, error) {
	cmp, ok := 0, false
	as, aStr := a.(string)
	bs, bStr := b.(string)
	af, aNum := pyJSONNumber(a)
	bf, bNum := pyJSONNumber(b)
	switch {
	case aStr && bStr:
		cmp, ok = strings.Compare(as, bs), true
	case aNum && bNum:
		cmp, ok = af.Cmp(bf), true
	}
	if !ok {
		return false, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("'%s' not supported between instances of '%s' and '%s'",
			op, pyrepr.TypeName(a), pyrepr.TypeName(b))}
	}
	if op == ">" {
		return cmp > 0, nil
	}
	return cmp < 0, nil
}

// Exact value of a decoded JSON number or bool
func pyJSONNumber(v any) (*big.Float, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return big.NewFloat(1), true
		}
		return big.NewFloat(0), true
	case float64:
		return new(big.Float).SetFloat64(x), true
	}
	if i, ok := pyrepr.Int(v); ok {
		return new(big.Float).SetInt(i), true
	}
	return nil, false
}

// One ledger row as a column keyed map, nil when no row matches
func verifyLedgerRow(ctx context.Context, conn state.SQLiteConn, query string, args ...any) (map[string]any, error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	values := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	row := make(map[string]any, len(cols))
	for i, c := range cols {
		switch v := values[i].(type) {
		case int64:
			row[c] = int(v)
		case []byte:
			row[c] = string(v)
		default:
			row[c] = v
		}
	}
	return row, nil
}

// Latest full passing icebath verify event of root, not older than since when since is set
func newestWorkspaceVerify(ctx context.Context, conn state.SQLiteConn, root string, since any) (map[string]any, error) {
	if pyTruthy(since) {
		return verifyLedgerRow(ctx, conn, "SELECT * FROM verification_events"+
			" WHERE root = ? AND kind = 'verify' AND status = 'passed' AND scope = 'full'"+
			" AND created_at >= ?"+
			" ORDER BY id DESC LIMIT 1", root, since)
	}
	return verifyLedgerRow(ctx, conn, "SELECT * FROM verification_events"+
		" WHERE root = ? AND kind = 'verify' AND status = 'passed' AND scope = 'full'"+
		" ORDER BY id DESC LIMIT 1", root)
}

// Best known verification state of a session workspace, stale when evidence predates the latest edit
func verificationStatus(ctx context.Context, env *tools.Env, sessionID, cwd string) (map[string]any, error) {
	if !verifyLedgerEnabled(ctx, env) {
		return map[string]any{"status": "disabled", "evidence": nil}, nil
	}
	facts := verifyProjectFacts(cwd)
	if facts == nil {
		return map[string]any{"status": "not_applicable", "evidence": nil}, nil
	}
	sid := verifySessionKey(sessionID)
	root, err := verifyRootFor(facts, cwd)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	verifyLedgerMu.Lock()
	defer verifyLedgerMu.Unlock()
	err = verifyLedgerTransaction(ctx, func(conn state.SQLiteConn) error {
		st, err := verifyLedgerRow(ctx, conn, "SELECT last_event_id, last_edit_at, changed_paths_json"+
			" FROM verification_state WHERE session_id = ? AND root = ?", sid, root)
		if err != nil {
			return err
		}
		if st == nil {
			out = map[string]any{"status": "unverified", "evidence": nil, "root": root, "session_id": sid, "changed_paths": []any{}}
			return nil
		}
		var event map[string]any
		if st["last_event_id"] != nil {
			if event, err = verifyLedgerRow(ctx, conn, "SELECT * FROM verification_events WHERE id = ?", st["last_event_id"]); err != nil {
				return err
			}
		}
		lastEditAt := st["last_edit_at"]
		stale := false
		if event != nil && pyTruthy(lastEditAt) {
			newer, err := pyOrder(lastEditAt, event["created_at"], ">")
			if err != nil {
				return err
			}
			stale = newer
		}
		if event == nil || stale {
			shared, err := newestWorkspaceVerify(ctx, conn, root, lastEditAt)
			if err != nil {
				return err
			}
			if shared != nil {
				event, stale = shared, false
			}
		}
		out = map[string]any{"evidence": nil, "root": root, "session_id": sid, "changed_paths": loadChangedPaths(st["changed_paths_json"])}
		if event == nil {
			out["status"] = "unverified"
			return nil
		}
		out["status"] = event["status"]
		if stale {
			out["status"] = "stale"
		}
		out["evidence"] = event
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Context carrying the profile home the tool env is bound to
func verifyEnvContext(env *tools.Env) context.Context {
	ctx := context.Background()
	if env != nil && env.Home != "" {
		ctx = home.WithOverride(ctx, env.Home)
	}
	return ctx
}

// Logger of the tool env or the process default
func verifyEnvLog(env *tools.Env) *slog.Logger {
	if env != nil && env.Log != nil {
		return env.Log
	}
	return slog.Default()
}

func init() {
	tools.VerificationEvidenceRecorder = func(env *tools.Env, command, cwd, sessionID string, exitCode int, output string, pipelineStatus []int) map[string]any {
		evidence, err := recordTerminalResult(verifyEnvContext(env), env, command, cwd, sessionID, exitCode, output, pipelineStatus)
		if err != nil {
			verifyEnvLog(env).Debug("verification evidence recording failed", "error", err)
			return nil
		}
		return evidence
	}
	tools.WorkspaceEditedMarker = func(env *tools.Env, sessionID, cwd string, paths []string) error {
		_, err := markWorkspaceEdited(verifyEnvContext(env), env, sessionID, cwd, paths)
		return err
	}
	verify.EvidenceRecorder = func(ctx context.Context, root, sessionID string, ok bool, command, scope, output string) (map[string]any, error) {
		return recordVerifyRun(ctx, nil, root, sessionID, ok, command, scope, output)
	}
}
