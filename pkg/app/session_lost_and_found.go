package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/state"
)

var lfMessageRoles = map[string]bool{"user": true, "assistant": true, "tool": true, "system": true}

var lfKnownSources = map[string]bool{"cli": true, "telegram": true, "discord": true, "slack": true, "whatsapp": true, "signal": true,
	"matrix": true, "irc": true, "email": true, "x": true, "twitter": true, "api": true, "gateway": true, "web": true, "dashboard": true,
	"tool": true, "subagent": true, "cron": true, "recovered": true, "imported": true, "acp": true}

const (
	lfSessionsLegacyMinimalNfield = 14
	lfSessionModelUsageNfield     = 18
	lfEpochLow                    = 1_000_000_000.0
	lfEpochHigh                   = 4_000_000_000.0
	lfSyntheticTitleLabel         = "best-effort recovered"
	lfSyntheticTitlePrefix        = "[" + lfSyntheticTitleLabel
	lfSQLiteHeaderLength          = 100
	lfSampleCap                   = 512
)

// Sentinel columns per salvaged table kind
var lfLayoutSentinels = map[string][]string{
	"sessions":            {"id", "source", "started_at"},
	"messages":            {"session_id", "role", "timestamp"},
	"session_model_usage": {"session_id", "model"},
}

const lfSQLite3CLIGuidance = "A last-resort page-level salvage is available when a `.recover`-capable `sqlite3` command-line shell is " +
	"installed: its `.recover` command can rebuild rows into lost_and_found tables even when the table schemas are " +
	"unreadable (this is a CLI-only feature, not part of Python's sqlite3 module, and some distro builds lack it — " +
	"the shell must include the sqlite_dbpage extension, as the official builds from sqlite.org do). Install such a " +
	"sqlite3 CLI (e.g. `brew install sqlite` or the precompiled sqlite-tools from sqlite.org) so it is on PATH, then " +
	"re-run with --allow-partial."

const lfWALResetVulnerableGuidance = "salvage against an Icebath database with the WAL-reset bug " +
	"(https://sqlite.org/wal.html#walresetbug, fixed in 3.51.3+ / backports " +
	"3.50.7 / 3.44.6; the vulnerable fresh-opener can unlink a live WAL/SHM " +
	"pair and split the database into two generations, losing acknowledged " +
	"writes — #100368). Install a fixed sqlite3 CLI (3.51.3+, e.g. `brew " +
	"install sqlite` or the precompiled sqlite-tools from sqlite.org)"

// Failure of the CLI recover pass to produce a usable database
type lostAndFoundError struct{ msg string }

// Message of the recover failure
func (e *lostAndFoundError) Error() string { return e.msg }

var lfVersionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// Version of the sqlite3 CLI from its version output, false when it cannot run or be parsed
func lfParseSQLite3CLIVersion(binary string) (state.VersionTuple, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--version")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return state.VersionTuple{}, false
	}
	match := lfVersionRe.FindSubmatch(stdout.Bytes())
	if match == nil {
		return state.VersionTuple{}, false
	}
	var version state.VersionTuple
	for i := 0; i < 3; i++ {
		version[i], _ = new(big.Int).SetString(string(match[i+1]), 10)
	}
	return version, true
}

var (
	lfLastRefusalMu sync.Mutex
	lfLastRefusal   = pyjson.NewObject()
)

// Why the last sqlite3 CLI search refused, empty when it succeeded
func findSQLite3CLIRefusal() *pyjson.Object {
	lfLastRefusalMu.Lock()
	defer lfLastRefusalMu.Unlock()
	return lfLastRefusal.Copy()
}

// Records the refusal of the current sqlite3 CLI search
func lfSetRefusal(refusal *pyjson.Object) {
	lfLastRefusalMu.Lock()
	lfLastRefusal = refusal
	lfLastRefusalMu.Unlock()
}

// Salvage safe recover capable sqlite3 CLI on PATH, false with the refusal recorded otherwise
func findSQLite3CLI() (string, bool) {
	lfSetRefusal(pyjson.NewObject())
	binary, ok := pyshutil.Which("sqlite3", nil)
	if !ok {
		lfSetRefusal(pyjson.ObjectOf("reason", "missing"))
		return "", false
	}
	if !lfCLISupportsRecover(binary) {
		lfSetRefusal(pyjson.ObjectOf("reason", "no_dbpage", "binary", binary))
		return "", false
	}
	if version, ok := lfParseSQLite3CLIVersion(binary); ok && state.IsSQLiteWALResetVulnerable(version) {
		text := version[0].String() + "." + version[1].String() + "." + version[2].String()
		cliLogger.Warn(fmt.Sprintf("sqlite3 CLI %s reports version %s, which still carries the "+
			"WAL-reset opener bug; refusing to use it for salvage", binary, text))
		lfSetRefusal(pyjson.ObjectOf("reason", "wal_reset_vulnerable", "binary", binary, "version", text,
			"detail", "reports version "+text+", which has "+lfWALResetVulnerableGuidance))
		return "", false
	}
	return binary, true
}

// True when the binary runs recover on a scratch database without missing sqlite_dbpage
func lfCLISupportsRecover(binary string) bool {
	tmp, err := pypath.Gettempdir()
	if err != nil {
		return false
	}
	scratchDir, err := os.MkdirTemp(tmp, "icebath-recover-probe-")
	if err != nil {
		return false
	}
	defer os.RemoveAll(scratchDir)
	scratch := filepath.Join(scratchDir, "probe.db")
	conn, err := recoveryConnectDefault(scratch)
	if err != nil {
		return false
	}
	_, err = recoveryExec(conn, "CREATE TABLE t (x)")
	if err == nil {
		_, err = recoveryExec(conn, "INSERT INTO t VALUES (1)")
	}
	if cerr := conn.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-readonly", scratch, ".recover")
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false
	}
	return !bytes.Contains(stderr.Bytes(), []byte("sqlite_dbpage"))
}

// Runs recover streamed into a fresh scratch database, zeroing a refused page one header of the snapshot and retrying
func runCLILostAndFoundRecover(source, lfPath, sqlite3Bin string, timeout time.Duration) (*pyjson.Object, error) {
	attempts, err := lfCLIRecoverAttempts(source, lfPath, sqlite3Bin, timeout)
	if err != nil {
		return nil, err
	}
	usable := func() bool { return pyTruthyValue(attempts[len(attempts)-1].GetOr("usable", nil)) }
	if usable() {
		return pyjson.ObjectOf("binary", sqlite3Bin, "attempts", lfAttemptList(attempts)), nil
	}
	notADatabase := false
	for _, a := range attempts {
		if strings.Contains(pyrepr.Str(a.GetOr("dump_stderr_tail", "")), "not a database") {
			notADatabase = true
		}
	}
	if notADatabase {
		handle, err := os.OpenFile(source, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		_, werr := handle.WriteAt(make([]byte, lfSQLiteHeaderLength), 0)
		if cerr := handle.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, werr
		}
		more, err := lfCLIRecoverAttempts(source, lfPath, sqlite3Bin, timeout)
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, more...)
		if usable() {
			return pyjson.ObjectOf("binary", sqlite3Bin, "attempts", lfAttemptList(attempts), "header_zeroed", true), nil
		}
	}
	var details []string
	for _, a := range attempts {
		tail := pyrepr.Str(a.GetOr("dump_stderr_tail", ""))
		if tail == "" {
			tail = pyrepr.Str(a.GetOr("load_stderr_tail", ""))
		}
		details = append(details, pystr.Strip(fmt.Sprintf("[%s] dump rc=%s load rc=%s %s", pyrepr.Str(a.GetOr("command", "")),
			pyrepr.Str(a.GetOr("dump_returncode", nil)), pyrepr.Str(a.GetOr("load_returncode", nil)), tail)))
	}
	return nil, &lostAndFoundError{"sqlite3 .recover did not produce a usable lost_and_found database: " + strings.Join(details, "; ")}
}

// Attempts as a JSON list
func lfAttemptList(attempts []*pyjson.Object) []any {
	out := make([]any, len(attempts))
	for i, a := range attempts {
		out[i] = a
	}
	return out
}

// Last n code points of UTF-8 decoded bytes with replacement
func lfTail(data []byte, n int) string {
	text := pystr.DecodeUTF8Replace(data)
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	runes := []rune(text)
	return string(runes[len(runes)-n:])
}

// Exit status of a finished command as subprocess reports it, negative for a signal
func lfReturncode(cmd *exec.Cmd) any {
	if cmd.ProcessState == nil {
		return nil
	}
	return int64(pysubprocess.Returncode(&exec.ExitError{ProcessState: cmd.ProcessState}))
}

// Recover with ignore freelist first and plain recover for older shells, stopping at the first usable output
func lfCLIRecoverAttempts(source, lfPath, sqlite3Bin string, timeout time.Duration) ([]*pyjson.Object, error) {
	var attempts []*pyjson.Object
	for _, command := range []string{".recover --ignore-freelist", ".recover"} {
		if _, err := os.Stat(lfPath); err == nil {
			if err := os.Remove(lfPath); err != nil {
				return nil, err
			}
		}
		tmp, err := pypath.Gettempdir()
		if err != nil {
			return nil, err
		}
		dumpStderr, err := os.CreateTemp(tmp, "icebath-recover-dump-stderr-")
		if err != nil {
			return nil, err
		}
		_ = os.Remove(dumpStderr.Name())
		attempt, err := func() (*pyjson.Object, error) {
			defer dumpStderr.Close()
			reader, writer, err := os.Pipe()
			if err != nil {
				return nil, err
			}
			dump := exec.Command(sqlite3Bin, "-readonly", source, command)
			dump.Stdout, dump.Stderr = writer, dumpStderr
			load := exec.Command(sqlite3Bin, lfPath)
			var loadErr bytes.Buffer
			load.Stdin, load.Stdout, load.Stderr = reader, nil, &loadErr
			if err := dump.Start(); err != nil {
				reader.Close()
				writer.Close()
				return nil, err
			}
			if err := load.Start(); err != nil {
				reader.Close()
				writer.Close()
				_ = dump.Process.Kill()
				_ = dump.Wait()
				return nil, err
			}
			writer.Close()
			reader.Close()
			loadDone := make(chan error, 1)
			go func() { loadDone <- load.Wait() }()
			dumpDone := make(chan error, 1)
			go func() { dumpDone <- dump.Wait() }()
			timedOut, loadReaped, dumpReaped := false, false, false
			select {
			case <-loadDone:
				loadReaped = true
				select {
				case <-dumpDone:
					dumpReaped = true
				case <-time.After(60 * time.Second):
					timedOut = true
				}
			case <-time.After(timeout):
				timedOut = true
			}
			if timedOut {
				_ = dump.Process.Kill()
				_ = load.Process.Kill()
				if !dumpReaped {
					<-dumpDone
				}
				if !loadReaped {
					<-loadDone
				}
				return nil, &lostAndFoundError{fmt.Sprintf("sqlite3 .recover timed out after %.0fs", timeout.Seconds())}
			}
			end, err := dumpStderr.Seek(0, io.SeekEnd)
			if err != nil {
				return nil, err
			}
			tailSize := min(end, 65536)
			buf := make([]byte, tailSize)
			if _, err := dumpStderr.ReadAt(buf, end-tailSize); err != nil && !errors.Is(err, io.EOF) {
				return nil, err
			}
			return pyjson.ObjectOf("command", command, "dump_returncode", lfReturncode(dump), "load_returncode", lfReturncode(load),
				"dump_stderr_tail", lfTail(buf, 2000), "load_stderr_tail", lfTail(loadErr.Bytes(), 2000),
				"usable", lfDBUsable(lfPath)), nil
		}()
		if err != nil {
			return nil, err
		}
		attempts = append(attempts, attempt)
		if pyTruthyValue(attempt.GetOr("usable", nil)) {
			break
		}
	}
	return attempts, nil
}

// True when the recover output exists, is not empty and holds a table
func lfDBUsable(lfPath string) bool {
	info, err := os.Stat(lfPath)
	if err != nil || info.Size() == 0 {
		return false
	}
	conn, err := recoveryConnectDefault(lfPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	_, found, err := recoveryQueryOne(conn, "SELECT 1 FROM sqlite_master WHERE type='table' LIMIT 1")
	return err == nil && found
}

// Substitutes by column index for NOT NULL columns, the schema default or an empty string or zero
func lfNotNullDefaults(conn state.SQLiteConn, table string) (map[int]any, error) {
	rows, err := recoveryQueryAll(conn, fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return nil, err
	}
	substitutes := map[int]any{}
	for index, row := range rows {
		if !pyTruthyValue(row[3]) {
			continue
		}
		if row[4] != nil {
			substitutes[index] = lfParseSQLDefault(pyrepr.Str(row[4]))
			continue
		}
		declared := ""
		if pyTruthyValue(row[2]) {
			declared = strings.ToUpper(pyrepr.Str(row[2]))
		}
		if strings.Contains(declared, "INT") || strings.Contains(declared, "REAL") {
			substitutes[index] = int64(0)
		} else {
			substitutes[index] = ""
		}
	}
	return substitutes, nil
}

// Quoted string, int, float or raw text of a table_info default literal
func lfParseSQLDefault(text string) any {
	if len(text) >= 2 && strings.HasPrefix(text, "'") && strings.HasSuffix(text, "'") {
		return text[1 : len(text)-1]
	}
	if text == "'" {
		return ""
	}
	if n, ok := lfPyInt(text); ok {
		return n
	}
	if f, ok := lfPyFloat(text); ok {
		return f
	}
	return text
}

// Python int() of text
func lfPyInt(text string) (any, bool) {
	t := pystr.Strip(text)
	if t == "" || strings.HasPrefix(t, "_") || strings.HasSuffix(t, "_") || strings.Contains(t, "__") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(t, "_", ""), 10)
	if !ok {
		return nil, false
	}
	if n.IsInt64() {
		return n.Int64(), true
	}
	return n, true
}

// Python float() of text
func lfPyFloat(text string) (float64, bool) {
	t := pystr.Strip(text)
	lower := strings.ToLower(strings.TrimLeft(t, "+-"))
	if lower == "inf" || lower == "infinity" || lower == "nan" {
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	}
	if t == "" || strings.ContainsAny(t, "xXpP") || strings.HasPrefix(t, "_") || strings.HasSuffix(t, "_") || strings.Contains(t, "__") {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(t, "_", ""), 64)
	if err != nil {
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && numErr.Err == strconv.ErrRange {
			return f, true
		}
		return 0, false
	}
	return f, true
}

// True for a cell that can be a session id of any minted shape
func lfIsSessionID(value any) bool {
	s, ok := value.(string)
	return ok && state.IsKnownSessionID(s)
}

var lfSourceRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// True for a cell that looks like a sessions.source value
func lfLooksLikeSource(value any) bool {
	s, ok := value.(string)
	return ok && s != "" && (lfKnownSources[s] || lfSourceRe.MatchString(s))
}

// Index of the source cell of a sessions record, one or three after the runtime counters, false otherwise
func lfSessionSourceIndex(cells []any) (int, bool) {
	if len(cells) > 1 && lfLooksLikeSource(cells[1]) {
		return 1, true
	}
	if len(cells) > 3 && lfLooksLikeSource(cells[3]) {
		for _, v := range cells[1:3] {
			if _, isInt := v.(int64); v != nil && !isInt {
				return 0, false
			}
		}
		return 3, true
	}
	return 0, false
}

// Table kind of one lost_and_found record by field count and sentinel values, empty when unclassifiable
func classifyLostAndFoundRow(nfield int64, cells []any) string {
	if len(cells) >= 3 && cells[0] == nil {
		sessionID, okID := cells[1].(string)
		role, okRole := cells[2].(string)
		if okID && sessionID != "" && okRole && lfMessageRoles[role] {
			return "messages"
		}
		return ""
	}
	var first any
	if len(cells) > 0 {
		first = cells[0]
	}
	if !lfIsSessionID(first) {
		return ""
	}
	var second any
	if len(cells) > 1 {
		second = cells[1]
	}
	if nfield == lfSessionModelUsageNfield {
		if s, ok := second.(string); ok && s != "" {
			return "session_model_usage"
		}
		return ""
	}
	if nfield >= lfSessionsLegacyMinimalNfield {
		if _, ok := lfSessionSourceIndex(cells); ok {
			return "sessions"
		}
	}
	return ""
}

// Number of a numeric cell, false for other types
func lfNumber(value any) (float64, bool) {
	switch x := value.(type) {
	case int64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// First cell inside the plausible epoch window, zero when none is
func lfHeuristicStartedAt(cells []any) float64 {
	for _, value := range cells {
		if f, ok := lfNumber(value); ok && lfEpochLow <= f && f <= lfEpochHigh {
			return f
		}
	}
	return 0.0
}

// Inserts values onto the destination column prefix, NULLs taking the NOT NULL substitutes
func lfInsertPrefixRow(dest state.SQLiteConn, table string, destColumns []string, values []any, substitutes map[int]any) (bool, error) {
	if len(substitutes) > 0 {
		replaced := make([]any, len(values))
		for i, v := range values {
			if sub, ok := substitutes[i]; ok && v == nil {
				replaced[i] = sub
			} else {
				replaced[i] = v
			}
		}
		values = replaced
	}
	return lfExecuteInsert(dest, table, destColumns[:len(values)], values)
}

// Column order and substitutes of a sessions record no history layout claimed, runtime counters placed by the source cell
func lfSessionsPrefixColumns(columns []string, defaults map[int]any, cells []any) ([]string, map[int]any) {
	runtimeColumns := []string{"runtime_revision", "runtime_generation"}
	var base []string
	for _, c := range columns {
		if !recoveryContains(runtimeColumns, c) {
			base = append(base, c)
		}
	}
	var ordered []string
	if idx, ok := lfSessionSourceIndex(cells); ok && idx == 3 {
		ordered = append(append([]string{base[0]}, runtimeColumns...), base[1:]...)
	} else {
		ordered = append(append([]string{}, base...), runtimeColumns...)
	}
	named := map[string]any{}
	for i, v := range defaults {
		named[columns[i]] = v
	}
	out := map[int]any{}
	for i, c := range ordered {
		if v, ok := named[c]; ok {
			out[i] = v
		}
	}
	return ordered, out
}

// Declared type of each destination column
func lfDeclaredTypes(conn state.SQLiteConn, table string) (map[string]string, error) {
	rows, err := recoveryQueryAll(conn, fmt.Sprintf(`PRAGMA table_info("%s")`, table))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, row := range rows {
		declared := ""
		if pyTruthyValue(row[2]) {
			declared = pyrepr.Str(row[2])
		}
		out[pyrepr.Str(row[1])] = declared
	}
	return out, nil
}

// True when a salvaged cell cannot have come from a column of the declared type
func lfTypeConflicts(value any, declared string) bool {
	if value == nil {
		return false
	}
	affinity := strings.ToUpper(declared)
	switch {
	case strings.Contains(affinity, "CHAR") || strings.Contains(affinity, "CLOB") || strings.Contains(affinity, "TEXT"):
		_, ok := value.(string)
		return !ok
	case strings.Contains(affinity, "REAL") || strings.Contains(affinity, "FLOA") || strings.Contains(affinity, "DOUB"):
		_, ok := value.(float64)
		return !ok
	case strings.Contains(affinity, "INT"):
		_, ok := value.(int64)
		return !ok
	}
	return false
}

var (
	lfSessionKeyRe     = regexp.MustCompile(`^agent:[^:]+:`)
	lfTokenRe          = regexp.MustCompile(`^[a-z][a-z0-9_.:-]*\n?$`)
	lfPricingVersionRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	lfHashRe           = regexp.MustCompile(`^[0-9a-f]{64}$`)
	lfHandoffStates    = map[string]bool{"pending": true, "running": true, "completed": true, "failed": true}
	lfTitleSources     = map[string]bool{"derived": true, "llm": true, "user": true}
)

// True for a number inside the plausible epoch window
func lfIsEpoch(value any) bool {
	f, ok := lfNumber(value)
	return ok && lfEpochLow <= f && f <= lfEpochHigh
}

// True for a non empty string
func lfIsNonemptyStr(value any) bool {
	s, ok := value.(string)
	return ok && s != ""
}

// Whether a sentinel column's cell looks like what the column names, true for non sentinel columns
func lfSentinelHolds(name string, value any) bool {
	switch name {
	case "id":
		return lfIsSessionID(value)
	case "source":
		return lfLooksLikeSource(value)
	case "started_at", "timestamp":
		return lfIsEpoch(value)
	case "session_id", "model":
		return lfIsNonemptyStr(value)
	case "role":
		s, ok := value.(string)
		return ok && lfMessageRoles[s]
	}
	return true
}

// True for a machine token
func lfIsToken(value string) bool { return lfTokenRe.MatchString(value) }

// True for text starting a JSON object or list, or empty text
func lfIsJSONStart(value string) bool {
	return value == "" || strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[")
}

// True for a path shaped text, or empty text
func lfIsPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "~") {
		return true
	}
	runes := []rune(value)
	return len(runes) > 1 && runes[1] == ':'
}

// True for text naming a URL
func lfIsURL(value string) bool { return strings.Contains(value, "://") }

// Rule accepting empty text or what rule accepts
func lfBlankOr(rule func(string) bool) func(string) bool {
	return func(value string) bool { return value == "" || rule(value) }
}

// Shape rule of a text column, nil when the column has none
func lfTextShapeRule(kind, name string) func(string) bool {
	switch kind {
	case "sessions":
		switch name {
		case "session_key":
			return func(v string) bool { return lfSessionKeyRe.MatchString(v) }
		case "chat_type", "end_reason", "cost_status", "cost_source", "billing_mode", "last_activity_provenance", "handoff_platform":
			return lfIsToken
		case "pricing_version":
			return func(v string) bool { return lfPricingVersionRe.MatchString(v) }
		case "title_source":
			return func(v string) bool { return lfTitleSources[v] }
		case "handoff_state":
			return func(v string) bool { return lfHandoffStates[v] }
		case "parent_session_id":
			return func(v string) bool { return lfIsSessionID(v) }
		case "system_prompt_hash":
			return func(v string) bool { return lfHashRe.MatchString(v) }
		case "model_config", "origin_json":
			return func(v string) bool { return strings.HasPrefix(v, "{") || strings.HasPrefix(v, "[") || v == "" }
		case "cwd", "git_repo_root":
			return lfIsPath
		case "billing_base_url":
			return lfIsURL
		}
	case "messages":
		switch name {
		case "effect_disposition", "finish_reason", "display_kind":
			return lfIsToken
		case "tool_calls", "reasoning_details", "codex_reasoning_items", "codex_message_items", "api_content", "display_metadata":
			return lfBlankOr(lfIsJSONStart)
		}
	case "session_model_usage":
		switch name {
		case "billing_base_url":
			return lfBlankOr(lfIsURL)
		case "billing_mode", "cost_status", "cost_source":
			return lfBlankOr(lfIsToken)
		}
	}
	return nil
}

// Whether a cell fits a column by sentinel, declared type and text shape
func lfCellFits(kind, name string, value any, destTypes map[string]string) bool {
	if value == nil {
		return true
	}
	if recoveryContains(lfLayoutSentinels[kind], name) && !lfSentinelHolds(name, value) {
		return false
	}
	declared, ok := destTypes[name]
	if !ok {
		return true
	}
	if lfTypeConflicts(value, declared) {
		return false
	}
	s, isStr := value.(string)
	if !isStr {
		return true
	}
	rule := lfTextShapeRule(kind, name)
	return rule == nil || rule(s)
}

// Cross column invariant that a handoff error is only written with a handoff state
func lfRowInvariantsHold(kind string, layout []string, rows [][]any) bool {
	if kind != "sessions" {
		return true
	}
	errorAt, stateAt := -1, -1
	for i, name := range layout {
		if name == "handoff_error" {
			errorAt = i
		}
		if name == "handoff_state" {
			stateAt = i
		}
	}
	if errorAt < 0 || stateAt < 0 {
		return true
	}
	for _, row := range rows {
		if row[errorAt] != nil && row[stateAt] == nil {
			return false
		}
	}
	return true
}

// Hash key of a cell under Python equality, equal ints and integral floats sharing one key
func lfValueKey(value any) string {
	switch x := value.(type) {
	case int64:
		return "n:" + strconv.FormatInt(x, 10)
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			bf := new(big.Float).SetFloat64(x)
			n, _ := bf.Int(nil)
			return "n:" + n.String()
		}
		return "f:" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "s:" + x
	case []byte:
		return "b:" + string(x)
	}
	return "o:" + pyrepr.Repr(value)
}

// Evidence layout inference needs from a population, gathered in one streaming pass
type lfLayoutEvidence struct {
	kind        string
	widths      map[int]bool
	byPosition  []lfValueSet
	rowsByWidth map[int][][]any
}

// Insertion ordered set of distinct non NULL cells under Python equality
type lfValueSet struct {
	keys   map[string]bool
	values []any
}

// Evidence collector for one table kind
func newLFLayoutEvidence(kind string) *lfLayoutEvidence {
	return &lfLayoutEvidence{kind: kind, widths: map[int]bool{}, rowsByWidth: map[int][][]any{}}
}

// Adds one record's width, sampled distinct cells and for sessions the record itself
func (e *lfLayoutEvidence) add(cells []any) {
	e.widths[len(cells)] = true
	for len(e.byPosition) < len(cells) {
		e.byPosition = append(e.byPosition, lfValueSet{keys: map[string]bool{}})
	}
	for i, value := range cells {
		set := &e.byPosition[i]
		if value == nil || len(set.values) >= lfSampleCap {
			continue
		}
		key := lfValueKey(value)
		if !set.keys[key] {
			set.keys[key] = true
			set.values = append(set.values, value)
		}
	}
	if e.kind == "sessions" {
		e.rowsByWidth[len(cells)] = append(e.rowsByWidth[len(cells)], cells)
	}
}

// Column name each record position resolves to per width, nil where surviving layouts disagree
func inferPhysicalLayouts(evidence *lfLayoutEvidence, destTypes map[string]string) (map[int][]*string, error) {
	kind := evidence.kind
	if _, ok := state.SchemaHistory[kind]; !ok || len(evidence.widths) == 0 {
		return map[int][]*string{}, nil
	}
	verdicts := map[string]bool{}
	fits := func(name string, value any) bool {
		key := name + "\x00" + lfValueKey(value)
		verdict, ok := verdicts[key]
		if !ok {
			verdict = lfCellFits(kind, name, value, destTypes)
			verdicts[key] = verdict
		}
		return verdict
	}
	invariantVerdicts := map[[3]int]bool{}
	invariantsHold := func(layout []string) bool {
		sameWidth := evidence.rowsByWidth[len(layout)]
		if len(sameWidth) == 0 {
			return true
		}
		errorAt, stateAt := -1, -1
		for i, name := range layout {
			if name == "handoff_error" {
				errorAt = i
			}
			if name == "handoff_state" {
				stateAt = i
			}
		}
		key := [3]int{len(layout), errorAt, stateAt}
		verdict, ok := invariantVerdicts[key]
		if !ok {
			verdict = lfRowInvariantsHold(kind, layout, sameWidth)
			invariantVerdicts[key] = verdict
		}
		return verdict
	}
	accept := func(layout []string, firstNew int) bool {
		for i := firstNew; i < min(len(layout), len(evidence.byPosition)); i++ {
			for _, value := range evidence.byPosition[i].values {
				if !fits(layout[i], value) {
					return false
				}
			}
		}
		return invariantsHold(layout)
	}
	survivors := map[int][][]string{}
	for w := range evidence.widths {
		survivors[w] = nil
	}
	layouts, err := state.ReachablePhysicalLayouts(kind, accept)
	if err != nil {
		return nil, err
	}
	for layout := range layouts {
		bucket, ok := survivors[len(layout)]
		if !ok {
			continue
		}
		dup := false
		for _, existing := range bucket {
			if strings.Join(existing, "\x00") == strings.Join(layout, "\x00") {
				dup = true
				break
			}
		}
		if !dup {
			survivors[len(layout)] = append(bucket, append([]string{}, layout...))
		}
	}
	result := map[int][]*string{}
	for width, list := range survivors {
		if len(list) == 0 {
			continue
		}
		consensus := make([]*string, width)
		for i := 0; i < width; i++ {
			name := list[0][i]
			agree := true
			for _, layout := range list[1:] {
				if layout[i] != name {
					agree = false
					break
				}
			}
			if agree {
				n := name
				consensus[i] = &n
			}
		}
		result[width] = consensus
	}
	return result, nil
}

// Inserts salvaged cells by source column name, unplaced NOT NULL columns taking their substitutes
func lfInsertNamedRow(dest state.SQLiteConn, table string, layout []*string, cells []any, destColumns []string, substitutes map[int]any, overrides *pyjson.Object) (bool, error) {
	destIndex := map[string]int{}
	for i, name := range destColumns {
		destIndex[name] = i
	}
	mapped := pyjson.NewObject()
	for i := 0; i < min(len(layout), len(cells)); i++ {
		if layout[i] == nil {
			continue
		}
		index, ok := destIndex[*layout[i]]
		if !ok {
			continue
		}
		value := cells[i]
		if sub, has := substitutes[index]; has && value == nil {
			value = sub
		}
		mapped.Set(*layout[i], value)
	}
	indexes := make([]int, 0, len(substitutes))
	for index := range substitutes {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		if _, ok := mapped.Get(destColumns[index]); !ok {
			mapped.Set(destColumns[index], substitutes[index])
		}
	}
	if overrides != nil {
		mapped.Update(overrides)
	}
	values := make([]any, len(mapped.Keys))
	for i, key := range mapped.Keys {
		values[i], _ = mapped.Get(key)
	}
	return lfExecuteInsert(dest, table, mapped.Keys, values)
}

// INSERT OR IGNORE of one row, true when it landed
func lfExecuteInsert(dest state.SQLiteConn, table string, columns []string, values []any) (bool, error) {
	quoted, marks := recoveryQuotedColumns(columns)
	n, err := recoveryExec(dest, fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (%s) VALUES (%s)`, table, quoted, marks), values...)
	return n == 1, err
}

// Copies rows recover attributed to real canonical tables, creating a lazy gateway table first
func lfCopyDirectTables(ctx context.Context, lfConn, dest state.SQLiteConn) (*pyjson.Object, error) {
	copied := pyjson.NewObject()
	for _, table := range append(append([]string{}, recoveryCanonicalTables...), recoveryAuxiliaryTables...) {
		sourceColumns, err := recoveryTableColumns(lfConn, table)
		if err != nil {
			return nil, err
		}
		if len(sourceColumns) == 0 {
			continue
		}
		destColumns, err := recoveryTableColumns(dest, table)
		if err != nil {
			return nil, err
		}
		if init, ok := recoveryAuxiliarySchemas[table]; ok && len(destColumns) == 0 {
			if err := init(ctx, dest); err != nil {
				return nil, err
			}
			if destColumns, err = recoveryTableColumns(dest, table); err != nil {
				return nil, err
			}
		}
		var columns []string
		for _, c := range destColumns {
			if recoveryContains(sourceColumns, c) {
				columns = append(columns, c)
			}
		}
		if len(columns) == 0 {
			continue
		}
		quoted, marks := recoveryQuotedColumns(columns)
		rows, err := recoveryQueryAll(lfConn, fmt.Sprintf(`SELECT %s FROM "%s"`, quoted, table))
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			copied.Set(table, int64(0))
			continue
		}
		before, err := recoveryCountRows(dest, table)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if _, err := recoveryExec(dest, fmt.Sprintf(`INSERT OR IGNORE INTO "%s" (%s) VALUES (%s)`, table, quoted, marks), row...); err != nil {
				return nil, err
			}
		}
		after, err := recoveryCountRows(dest, table)
		if err != nil {
			return nil, err
		}
		copied.Set(table, after-before)
	}
	return copied, nil
}

// One classified lost_and_found record
type lfRecord struct {
	kind    string
	lfRowid any
	nfield  int64
	cells   []any
}

// Streams every record of the lost_and_found tables with its classification
func lfRecords(lfConn state.SQLiteConn, lfTables []string, yield func(lfRecord) error) error {
	for _, lfTable := range lfTables {
		columns, err := recoveryTableColumns(lfConn, lfTable)
		if err != nil {
			return err
		}
		if len(columns) < 3 || columns[0] != "rootpgno" || columns[1] != "pgno" || columns[2] != "nfield" {
			continue
		}
		err = func() error {
			rows, err := lfConn.QueryContext(context.Background(), fmt.Sprintf(`SELECT * FROM "%s"`, lfTable))
			if err != nil {
				return err
			}
			defer rows.Close()
			for {
				row, ok, err := recoveryNextRow(rows)
				if err != nil {
					return err
				}
				if !ok {
					return nil
				}
				var nfield int64
				switch x := row[2].(type) {
				case nil:
				case int64:
					nfield = x
				case float64:
					nfield = int64(x)
				case string:
					n, ok := lfPyInt(x)
					if !ok {
						if err := yield(lfRecord{}); err != nil {
							return err
						}
						continue
					}
					nfield = lfClampInt(n)
				default:
					if err := yield(lfRecord{}); err != nil {
						return err
					}
					continue
				}
				end := 4 + max(nfield, 0)
				if end > int64(len(row)) {
					end = int64(len(row))
				}
				cells := append([]any{}, row[min(4, len(row)):end]...)
				if err := yield(lfRecord{kind: classifyLostAndFoundRow(nfield, cells), lfRowid: row[3], nfield: nfield, cells: cells}); err != nil {
					return err
				}
			}
		}()
		if err != nil {
			return err
		}
	}
	return nil
}

// Maps a recover output database into a fresh session store by inferred physical layout
func mapLostAndFoundRows(ctx context.Context, lfConn, dest state.SQLiteConn) (*pyjson.Object, error) {
	mapped := pyjson.ObjectOf("sessions", int64(0), "messages", int64(0), "session_model_usage", int64(0))
	widthsByKind := pyjson.NewObject()
	report := pyjson.ObjectOf("direct_table_rows", pyjson.NewObject(), "mapped", mapped, "legacy_minimal_sessions", int64(0),
		"mapped_by_layout", int64(0), "unrecognized_layout_rows", int64(0), "unrecognized_layout_widths", widthsByKind,
		"inferred_layouts", pyjson.NewObject(), "unmapped_rows", int64(0), "insert_conflicts", int64(0), "lost_and_found_tables", []any{})
	kinds := []string{"sessions", "messages", "session_model_usage"}
	protected := map[string][]string{"sessions": {"id", "source"}, "messages": {"session_id", "role"}, "session_model_usage": {"session_id", "model"}}
	err := recoveryImmediate(dest, func() error {
		direct, err := lfCopyDirectTables(ctx, lfConn, dest)
		if err != nil {
			return err
		}
		report.Set("direct_table_rows", direct)
		type target struct {
			columns  []string
			defaults map[int]any
		}
		targets := map[string]target{}
		destTypes := map[string]map[string]string{}
		for _, kind := range kinds {
			columns, err := recoveryTableColumns(dest, kind)
			if err != nil {
				return err
			}
			defaults, err := lfNotNullDefaults(dest, kind)
			if err != nil {
				return err
			}
			for _, name := range protected[kind] {
				for i, c := range columns {
					if c == name {
						delete(defaults, i)
						break
					}
				}
			}
			targets[kind] = target{columns, defaults}
			if destTypes[kind], err = lfDeclaredTypes(dest, kind); err != nil {
				return err
			}
		}
		tableRows, err := recoveryQueryAll(lfConn, "SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'lost_and_found%'")
		if err != nil {
			return err
		}
		var lfTables []string
		for _, row := range tableRows {
			lfTables = append(lfTables, pyrepr.Str(row[0]))
		}
		report.Set("lost_and_found_tables", recoveryStrings(lfTables))
		evidence := map[string]*lfLayoutEvidence{}
		for _, kind := range kinds {
			evidence[kind] = newLFLayoutEvidence(kind)
		}
		if err := lfRecords(lfConn, lfTables, func(r lfRecord) error {
			if r.kind == "" {
				recoveryBump(report, "unmapped_rows", 1)
			} else {
				evidence[r.kind].add(r.cells)
			}
			return nil
		}); err != nil {
			return err
		}
		layouts := map[string]map[int][]*string{}
		inferred := pyjson.NewObject()
		for _, kind := range kinds {
			if layouts[kind], err = inferPhysicalLayouts(evidence[kind], destTypes[kind]); err != nil {
				return err
			}
			if len(layouts[kind]) == 0 {
				continue
			}
			widths := make([]int, 0, len(layouts[kind]))
			for w := range layouts[kind] {
				widths = append(widths, w)
			}
			sort.Ints(widths)
			byWidth := pyjson.NewObject()
			for _, w := range widths {
				names := make([]any, len(layouts[kind][w]))
				for i, n := range layouts[kind][w] {
					if n != nil {
						names[i] = *n
					}
				}
				byWidth.Set(strconv.Itoa(w), names)
			}
			inferred.Set(kind, byWidth)
		}
		report.Set("inferred_layouts", inferred)
		return lfRecords(lfConn, lfTables, func(r lfRecord) error {
			if r.kind == "" {
				return nil
			}
			columns, defaults := targets[r.kind].columns, targets[r.kind].defaults
			layout, hasLayout := layouts[r.kind][len(r.cells)]
			legacyMinimal := r.kind == "sessions" && r.nfield == lfSessionsLegacyMinimalNfield
			if !hasLayout && !legacyMinimal {
				recoveryBump(report, "unrecognized_layout_rows", 1)
				widths, _ := widthsByKind.GetOr(r.kind, nil).([]any)
				if !recoveryContainsInt(widths, int64(len(r.cells))) {
					widths = append(widths, int64(len(r.cells)))
				}
				widthsByKind.Set(r.kind, widths)
			}
			var inserted bool
			var err error
			switch {
			case hasLayout:
				var overrides *pyjson.Object
				if r.kind == "messages" {
					overrides = pyjson.ObjectOf("id", r.lfRowid)
				}
				inserted, err = lfInsertNamedRow(dest, r.kind, layout, r.cells, columns, defaults, overrides)
				if err == nil && inserted {
					recoveryBump(report, "mapped_by_layout", 1)
				}
			case legacyMinimal:
				source := any("recovered")
				if lfLooksLikeSource(r.cells[1]) {
					source = r.cells[1]
				}
				var n int64
				n, err = recoveryExec(dest, "INSERT OR IGNORE INTO sessions (id, source, started_at, title) VALUES (?, ?, ?, ?)",
					r.cells[0], source, lfHeuristicStartedAt(r.cells), lfSyntheticTitlePrefix+"] legacy session row (layout unknown)")
				inserted = n == 1
				if err == nil && inserted {
					recoveryBump(report, "legacy_minimal_sessions", 1)
				}
			default:
				values := append([]any{}, r.cells[:min(len(columns), len(r.cells))]...)
				if r.kind == "messages" {
					values[0] = r.lfRowid
				} else if r.kind == "sessions" {
					columns, defaults = lfSessionsPrefixColumns(columns, defaults, r.cells)
				}
				inserted, err = lfInsertPrefixRow(dest, r.kind, columns, values, defaults)
			}
			if err != nil {
				if !isSQLiteDatabaseError(err) {
					return err
				}
				recoveryBump(report, "unmapped_rows", 1)
				return nil
			}
			if inserted {
				recoveryBump(mapped, r.kind, 1)
			} else {
				recoveryBump(report, "insert_conflicts", 1)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// Integer of a parsed Python int clamped to the int64 range
func lfClampInt(n any) int64 {
	switch x := n.(type) {
	case int64:
		return x
	case *big.Int:
		if x.Sign() > 0 {
			return math.MaxInt64
		}
		return math.MinInt64
	}
	return 0
}

// True when a JSON list holds the integer
func recoveryContainsInt(list []any, n int64) bool {
	for _, item := range list {
		if item == n {
			return true
		}
	}
	return false
}

// Fabricates marked synthetic parents for salvaged child rows and repairs dangling session references
func fabricateMissingParentSessions(dest state.SQLiteConn) (*pyjson.Object, error) {
	result := pyjson.ObjectOf("sessions_stubbed", int64(0), "messages_retained", int64(0), "usage_rows_retained", int64(0))
	err := recoveryImmediate(dest, func() error {
		type orphan struct {
			startedAt float64
			count     int64
		}
		orphans := map[string]orphan{}
		rows, err := recoveryQueryAll(dest, "SELECT m.session_id, MIN(m.timestamp), COUNT(*) FROM messages AS m WHERE m.session_id IS NOT NULL AND "+
			"NOT EXISTS (SELECT 1 FROM sessions WHERE sessions.id = m.session_id) GROUP BY m.session_id")
		if err != nil {
			return err
		}
		for _, row := range rows {
			startedAt := 0.0
			if row[1] != nil {
				startedAt = recoveryFloat(row[1])
			}
			orphans[pyrepr.Str(row[0])] = orphan{startedAt, recoveryInt(row[2])}
		}
		usage, err := recoveryQueryAll(dest, "SELECT DISTINCT u.session_id FROM session_model_usage AS u WHERE u.session_id IS NOT NULL AND NOT "+
			"EXISTS (SELECT 1 FROM sessions WHERE sessions.id = u.session_id)")
		if err != nil {
			return err
		}
		for _, row := range usage {
			if _, ok := orphans[pyrepr.Str(row[0])]; !ok {
				orphans[pyrepr.Str(row[0])] = orphan{}
			}
		}
		ids := make([]string, 0, len(orphans))
		for id := range orphans {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		titles := recoverySyntheticTitles(dest, lfSyntheticTitleLabel)
		for _, id := range ids {
			title, err := titles()
			if err != nil {
				return err
			}
			info := orphans[id]
			if _, err := recoveryExec(dest, "INSERT INTO sessions (id, source, started_at, title, message_count) "+
				"VALUES (?, 'recovered', ?, ?, ?)", id, info.startedAt, title, info.count); err != nil {
				return err
			}
			recoveryBump(result, "sessions_stubbed", 1)
			recoveryBump(result, "messages_retained", info.count)
		}
		count, err := recoveryCountRows(dest, "session_model_usage")
		if err != nil {
			return err
		}
		result.Set("usage_rows_retained", count)
		for _, stmt := range []string{
			"UPDATE sessions SET parent_session_id = NULL WHERE parent_session_id IS NOT NULL AND NOT EXISTS (SELECT " +
				"1 FROM sessions AS p WHERE p.id = sessions.parent_session_id)",
			"UPDATE sessions SET system_prompt_hash = NULL WHERE system_prompt_hash IS NOT NULL AND NOT EXISTS " +
				"(SELECT 1 FROM system_prompts WHERE system_prompts.hash = sessions.system_prompt_hash)",
			"UPDATE sessions SET tool_names = NULL WHERE " + recoveryDanglingToolPin,
		} {
			if _, err := recoveryExec(dest, stmt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Rebuilds the derived FTS indexes from the salvaged canonical rows
func rebuildFTSIndexes(dest state.SQLiteConn) (*pyjson.Object, error) {
	results := pyjson.NewObject()
	for _, table := range []string{"messages_fts", "messages_fts_trigram", "messages_fts_cjk"} {
		columns, err := recoveryTableColumns(dest, table)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 {
			continue
		}
		if _, err := recoveryExec(dest, fmt.Sprintf(`INSERT INTO "%s" ("%s") VALUES ('rebuild')`, table, table)); err != nil {
			if !isSQLiteDatabaseError(err) {
				return nil, err
			}
			results.Set(table, "rebuild failed: "+sessionsExcText(err))
			continue
		}
		results.Set(table, "rebuilt")
	}
	return results, nil
}

// Timestamp columns that are NULL or before the epoch floor in every salvaged row, the sign of a positional mis-mapping
func lostAndFoundPlausibilityErrors(conn state.SQLiteConn) ([]any, error) {
	var errs []any
	checks := []struct{ table, column, filter string }{
		{"sessions", "started_at", "WHERE COALESCE(title, '') NOT LIKE '" + lfSyntheticTitlePrefix + "%'"},
		{"messages", "timestamp", ""},
	}
	for _, check := range checks {
		row, _, err := recoveryQueryOne(conn, fmt.Sprintf("SELECT COUNT(*) FROM %s %s", check.table, check.filter))
		if err != nil {
			return nil, err
		}
		total := recoveryInt(row[0])
		if total == 0 {
			continue
		}
		joiner := "WHERE"
		if check.filter != "" {
			joiner = "AND"
		}
		row, _, err = recoveryQueryOne(conn, fmt.Sprintf("SELECT COUNT(*) FROM %s %s %s (%s IS NULL OR %s < ?)", check.table, check.filter, joiner,
			check.column, check.column), lfEpochLow)
		if err != nil {
			return nil, err
		}
		if recoveryInt(row[0]) == total {
			errs = append(errs, fmt.Sprintf("%s.%s is implausible in all %d salvaged row(s) "+
				"(NULL or before 2001-09): the source's physical column order "+
				"did not match the destination template, so cells were mapped "+
				"onto the wrong columns", check.table, check.column, total))
		}
	}
	return errs, nil
}

// Page level salvage through the sqlite3 CLI recover when table schemas are unreadable
func recoverViaLostAndFound(ctx context.Context, source, snapshotSource, snapshotDir, output string, inspection, diskSpace *pyjson.Object,
	missingRequired []string) (*pyjson.Object, error) {
	missing := strings.Join(missingRequired, ", ")
	sqlite3Bin, ok := findSQLite3CLI()
	if !ok {
		refusal := findSQLite3CLIRefusal()
		if refusal.GetOr("reason", nil) == "wal_reset_vulnerable" {
			return nil, recoverySourceError("Partial recovery requires a page-level salvage shell, but " +
				"the only sqlite3 CLI on PATH is not safe to use for it: it " + pyrepr.Str(refusal.GetOr("detail", "")) +
				". The readable table schemas for: " + missing + " are still required.")
		}
		return nil, recoverySourceError("Partial recovery still requires readable table schemas for: " + missing + ". " + lfSQLite3CLIGuidance)
	}
	lfPath := filepath.Join(snapshotDir, "lost_and_found.db")
	cliReport, err := runCLILostAndFoundRecover(snapshotSource, lfPath, sqlite3Bin, 3600*time.Second)
	var lfErr *lostAndFoundError
	if err != nil && (errors.As(err, &lfErr) || pypath.IsOSError(err)) {
		return nil, recoverySourceError("Partial recovery could not read the table schemas for: " + missing +
			", and page-level .recover salvage failed: " + sessionsExcText(err))
	}
	if err != nil {
		return nil, err
	}
	lfConn, err := recoveryConnectDefault(lfPath)
	if err != nil {
		return nil, err
	}
	destination, err := recoveryFreshDestination(ctx, output, false)
	if err != nil {
		lfConn.Close()
		return nil, err
	}
	var mapping, fabricated, fts, derived *pyjson.Object
	mapErr := func() error {
		var err error
		if mapping, err = mapLostAndFoundRows(ctx, lfConn, destination); err != nil {
			return err
		}
		if fabricated, err = fabricateMissingParentSessions(destination); err != nil {
			return err
		}
		if fts, err = rebuildFTSIndexes(destination); err != nil {
			return err
		}
		derived, err = recoveryFinalizeDerivedMetadata(destination)
		return err
	}()
	lfConn.Close()
	destination.Close()
	if mapErr != nil {
		return nil, mapErr
	}
	copyReport := pyjson.NewObject()
	direct, _ := mapping.GetOr("direct_table_rows", nil).(*pyjson.Object)
	mappedCounts, _ := mapping.GetOr("mapped", nil).(*pyjson.Object)
	for _, table := range []string{"sessions", "messages", "session_model_usage"} {
		copyReport.Set(table, pyjson.ObjectOf("mode", "lost_and_found_salvage", "status", "partial",
			"copied_rows", recoveryInt(direct.GetOr(table, nil))+recoveryInt(mappedCounts.GetOr(table, nil)),
			"error", "recovered via page-level lost_and_found salvage; row completeness cannot be verified against the source"))
	}
	orphanCleanup := pyjson.ObjectOf("sessions_reconstructed", fabricated.GetOr("sessions_stubbed", nil),
		"messages_retained", fabricated.GetOr("messages_retained", nil), "messages_removed", int64(0), "total_removed_or_relinked", int64(0))
	verification, err := recoveryVerifyRecoveredDatabase(ctx, output, map[string]*int64{"sessions": nil, "messages": nil}, copyReport, true, orphanCleanup)
	if err != nil {
		return nil, err
	}
	recoveryVerifyWarning(verification, "BEST-EFFORT page-level salvage: the source table schemas were unreadable, so rows were rebuilt from raw "+
		"pages and mapped heuristically. Review every count before trusting this output.")
	if pyTruthyValue(cliReport.GetOr("header_zeroed", nil)) {
		recoveryVerifyWarning(verification, "header salvage: SQLite refused the source outright (page-1 header damaged, 'file is not a "+
			"database'); the header of the private snapshot copy was zeroed so .recover could walk the "+
			"surviving pages. Rows written only to a -wal after the last checkpoint are not included.")
	}
	verification.Set("loss_detected", true)
	verification.Set("complete", false)
	plausibilityConn, err := recoveryConnectDefault(output)
	if err != nil {
		return nil, err
	}
	plausibility, err := lostAndFoundPlausibilityErrors(plausibilityConn)
	plausibilityConn.Close()
	if err != nil {
		return nil, err
	}
	if guessed := recoveryInt(mapping.GetOr("unrecognized_layout_rows", nil)); guessed != 0 {
		widthsByKind, _ := mapping.GetOr("unrecognized_layout_widths", nil).(*pyjson.Object)
		kinds := append([]string{}, widthsByKind.Keys...)
		sort.Strings(kinds)
		var parts []string
		for _, kind := range kinds {
			list, _ := widthsByKind.GetOr(kind, nil).([]any)
			var ws []int64
			for _, w := range list {
				ws = append(ws, recoveryInt(w))
			}
			sort.Slice(ws, func(i, j int) bool { return ws[i] < ws[j] })
			texts := make([]string, len(ws))
			for i, w := range ws {
				texts[i] = strconv.FormatInt(w, 10)
			}
			parts = append(parts, fmt.Sprintf("%s rows with %s fields", kind, strings.Join(texts, "/")))
		}
		plausibility = append(plausibility, fmt.Sprintf("%d salvaged row(s) matched no known physical column layout and were mapped "+
			"positionally (%s); their fields may be shifted. Inspect those sessions before trusting them.", guessed, strings.Join(parts, ", ")))
	}
	if len(plausibility) > 0 {
		errs, _ := verification.GetOr("errors", nil).([]any)
		verification.Set("errors", append(errs, plausibility...))
		verification.Set("healthy", false)
	}
	return recoveryReport(source, output, inspection, diskSpace, verification, "healthy",
		"allow_partial", true, "mode", "lost_and_found_salvage", "best_effort", true, "unreadable_schemas", recoveryStrings(missingRequired),
		"sqlite3_cli", cliReport, "lost_and_found", mapping, "session_stubs", fabricated, "fts_rebuild", fts, "copy", copyReport,
		"orphan_cleanup", orphanCleanup, "derived_metadata", derived), nil
}
