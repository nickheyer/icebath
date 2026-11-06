package state

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Resume and export guard default, read open retry window and read busy timeout
const (
	maxSafeMessages            = 20_000
	readOpenRetrySeconds       = 60.0
	readOnlyIOErrRetryAttempts = 3
	readBusyTimeoutS           = 5.0
)

// Billing buckets that are no routable provider identity
var bareBillingProviders = setOf("auto", "custom")

// Sessions limit from config.yaml, fallback when unset, negative or unreadable, zero disabling the guard
func configuredTranscriptLimit(ctx context.Context, key string, fallback int) int {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), os.LookupEnv)
	if err != nil {
		return fallback
	}
	var value any
	switch sessions := cfg["sessions"].(type) {
	case config.Tree:
		value = sessions[key]
	case map[string]any:
		value = sessions[key]
	}
	if value == nil {
		return fallback
	}
	n, err := pyint.Of(value)
	switch {
	case err != nil || n.Sign() < 0:
		return fallback
	case !n.IsInt64() || n.Int64() > math.MaxInt:
		return math.MaxInt
	}
	return int(n.Int64())
}

// Message cap a resume reloads, sessions.max_resume_messages
func ResolvedMaxResumeMessages(ctx context.Context) int {
	return configuredTranscriptLimit(ctx, "max_resume_messages", maxSafeMessages)
}

// Message cap an in memory export builds, sessions.max_export_messages
func ResolvedMaxExportMessages(ctx context.Context) int {
	return configuredTranscriptLimit(ctx, "max_export_messages", maxSafeMessages)
}

// A ValueError refusing to reload a transcript longer than the resume cap
type SessionResumeTooLargeError struct {
	MessageCount, Limit int
	Scope               string
}

// Resume refusal with the default limit and scope of the Python constructor
func NewSessionResumeTooLargeError(messageCount int) *SessionResumeTooLargeError {
	return &SessionResumeTooLargeError{MessageCount: messageCount, Limit: maxSafeMessages, Scope: "across its lineage"}
}

func (e *SessionResumeTooLargeError) Error() string {
	return fmt.Sprintf("This session is too long to reload safely (%d messages; limit %d). "+
		"Start a fresh chat and keep a copy with the dashboard Sessions page's Export action or "+
		"`icebath sessions export --format md --session-id <id>` (neither is capped), or raise the limit "+
		"with `icebath config set sessions.max_resume_messages 0`.", e.MessageCount, e.Limit)
}

func (e *SessionResumeTooLargeError) PyTypeName() string { return "ValueError" }

// A ValueError refusing an in memory JSON export past the export cap
type SessionExportTooLargeError struct {
	SessionID           string
	MessageCount, Limit int
}

func (e *SessionExportTooLargeError) Error() string {
	limit, _ := pyrepr.Format(int64(e.Limit), ",")
	return fmt.Sprintf("Session '%s' has more than %s exportable messages; the JSON/JSONL "+
		"backup is built in memory and capped per session. Use the dashboard Sessions page's streaming "+
		"Export action, or set sessions.max_export_messages: 0 in config.yaml to disable the guard.", e.SessionID, limit)
}

func (e *SessionExportTooLargeError) PyTypeName() string { return "ValueError" }

// Pid existence probe of lease holders, swapped by tests as hermes swaps its psutil module
var holderPIDExists = psutilPIDExists

// Lock holder pid token of a compression lease
var compressionHolderPIDRe = mustPyRe(`(?:^|:)pid=(\d+)(?::|$)`)

// Reports a local pid lock holder provably gone, foreign namespaces and probe doubt keeping the lease
func compressionLockHolderProcessIsDead(holder string) bool {
	m, err := compressionHolderPIDRe.FindStringMatch(holder)
	if err != nil || m == nil {
		return false
	}
	n, ok := pyint.Parse(m.GroupByNumber(1).String())
	if !ok || n.Sign() <= 0 || (n.IsInt64() && n.Int64() == int64(os.Getpid())) || !HolderPidCheckable(holder) {
		return false
	}
	if !n.IsInt64() || n.Int64() > math.MaxInt32 {
		return false
	}
	return !holderPIDExists(int(n.Int64()))
}

// Default state database path resolved at call time, a repointed DefaultDBPath winning
var DefaultDBPath = ""

// State database path for the process home unless DefaultDBPath was repointed
func defaultDBPath(ctx context.Context) string {
	if DefaultDBPath != "" {
		return DefaultDBPath
	}
	return filepath.Join(home.Dir(ctx), "state.db")
}

// In process guard bypass and extra production roots the isolation guard denies
var (
	stateDBGuardBypass         = false
	stateDBGuardExtraDenyRoots []string
	stateDBGuardMu             sync.Mutex
)

// Refuses a test context process opening a production state database before any connection or probe
func ensureTestIsolation(dbPath string) error {
	stateDBGuardMu.Lock()
	bypass, extra := stateDBGuardBypass, slicesCloneStrings(stateDBGuardExtraDenyRoots)
	stateDBGuardMu.Unlock()
	if bypass || os.Getenv(stateDBGuardBypassEnv) != "" || !inTestContext() {
		return nil
	}
	resolved, err := resolvePythonPath(dbPath)
	if err != nil {
		return nil
	}
	var roots []string
	if root, ok := realPlatformStateRoot(); ok {
		roots = append(roots, root)
	}
	for _, r := range extra {
		if p, err := resolvePythonPath(r); err == nil {
			roots = append(roots, p)
		}
	}
	for _, root := range roots {
		if isProductionStateDB(resolved, root) {
			return &pyint.Error{Kind: "RuntimeError", Msg: "live-system guard: test attempted to open production " +
				fmt.Sprintf("state.db at %s (under real Icebath root %s). ", resolved, root) +
				"Tests must run against a temporary ICEBATH_HOME — pass an " +
				"explicit tmp db_path or let the hermetic conftest redirect " +
				"ICEBATH_HOME. If this test genuinely needs the live database, mark it with " +
				"@pytest.mark.live_system_guard_bypass — or, for a spawned " +
				fmt.Sprintf("child process, export %s=1 in ", stateDBGuardBypassEnv) +
				"its environment."}
		}
	}
	return nil
}

func slicesCloneStrings(s []string) []string { return append([]string(nil), s...) }

// Path(p).expanduser().resolve() in its non strict form
func resolvePythonPath(p string) (string, error) {
	return pyRealpath(expandUser(p))
}

// Prompts the forked curator persisted into real sessions in older builds
var reviewHarnessPrefixes = []string{
	"Review the conversation above and update the skill library",
	"Review the conversation above and consider saving to memory",
}

// Role membership in a set of strings with the TypeError Python raises for an unhashable role
func roleIn(role any, roles ...string) (bool, error) {
	switch r := role.(type) {
	case []any, *pyjson.Object, map[string]any:
		return false, &pyint.Error{Kind: "TypeError", Msg: fmt.Sprintf("unhashable type: '%s'", pyrepr.TypeName(r))}
	case string:
		for _, want := range roles {
			if r == want {
				return true, nil
			}
		}
	}
	return false, nil
}

// Reports a persisted background review harness prompt in a user or system message
func isBackgroundReviewHarnessMessage(msg any) (bool, error) {
	if !isMapping(msg) {
		return false, nil
	}
	role, _ := mappingGet(msg, "role")
	if in, err := roleIn(role, "user", "system"); err != nil || !in {
		return false, err
	}
	content, _ := mappingGet(msg, "content")
	text, ok := content.(string)
	if !ok {
		return false, nil
	}
	text = pystr.Lstrip(text)
	for _, prefix := range reviewHarnessPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true, nil
		}
	}
	return false, nil
}

// Drops harness prompts and the curator mode assistant reply right after each, consecutive prompts arming one skip
func stripBackgroundReviewHarness(messages []any) ([]any, error) {
	if len(messages) == 0 {
		return messages, nil
	}
	out := []any{}
	skipNextAssistant, previousWasHarness := false, false
	for _, msg := range messages {
		harness, err := isBackgroundReviewHarnessMessage(msg)
		if err != nil {
			return nil, err
		}
		if harness {
			skipNextAssistant = !previousWasHarness
			previousWasHarness = true
			continue
		}
		if skipNextAssistant {
			skipNextAssistant = false
			if role, _ := mappingGet(msg, "role"); isMapping(msg) && pyrepr.Equal(role, "assistant") {
				previousWasHarness = false
				continue
			}
		}
		previousWasHarness = false
		out = append(out, msg)
	}
	return out, nil
}

// Bare protocol or tool name marker such as [memory]
var staleToolCallMarkerRe = regexp.MustCompile(`^\[[A-Za-z_][A-Za-z0-9_.-]*\]$`)

// Reports an assistant tool call turn whose content is a bare marker
func isStaleToolCallMarkerMessage(msg any) bool {
	if !isMapping(msg) {
		return false
	}
	role, _ := mappingGet(msg, "role")
	calls, _ := mappingGet(msg, "tool_calls")
	if !pyrepr.Equal(role, "assistant") || !pyrepr.Truthy(calls) {
		return false
	}
	content, _ := mappingGet(msg, "content")
	text, ok := content.(string)
	return ok && staleToolCallMarkerRe.MatchString(pystr.Strip(text))
}

// Blanks stale marker assistant content in place, keeping tool call and result pairing
func stripStaleToolCallMarkers(messages []any) []any {
	repaired := 0
	for _, msg := range messages {
		if isStaleToolCallMarkerMessage(msg) {
			switch m := msg.(type) {
			case map[string]any:
				m["content"] = ""
			case *pyjson.Object:
				m.Set("content", "")
			}
			repaired++
		}
	}
	if repaired > 0 {
		slog.Info(fmt.Sprintf("Cleared %d stale tool-call marker message(s) while restoring session (#78148)", repaired))
	}
	return messages
}
