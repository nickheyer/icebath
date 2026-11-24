package state

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
)

// Read open retry window and read busy timeout
const (
	readOpenRetrySeconds       = 60.0
	readOnlyIOErrRetryAttempts = 3
	readBusyTimeoutS           = 5.0
)

// Billing buckets that are no routable provider identity
var bareBillingProviders = setOf("auto", "custom")

// Message cap of a setting, zero disabling the guard and a negative value reading as the default
func transcriptLimit(ctx context.Context, e *settings.Int) int {
	if n := e.Get(ctx); n >= 0 {
		return n
	}
	return e.Default
}

// Message cap a resume reloads, sessions.max_resume_messages
func ResolvedMaxResumeMessages(ctx context.Context) int {
	return transcriptLimit(ctx, settings.SessionsMaxResumeMessages)
}

// Message cap an in memory export builds, sessions.max_export_messages
func ResolvedMaxExportMessages(ctx context.Context) int {
	return transcriptLimit(ctx, settings.SessionsMaxExportMessages)
}

// A ValueError refusing to reload a transcript longer than the resume cap
type SessionResumeTooLargeError struct {
	MessageCount, Limit int
	Scope               string
}

func (e *SessionResumeTooLargeError) Error() string {
	return fmt.Sprintf("This session is too long to reload safely (%d messages; limit %d). "+
		"Start a fresh chat and keep a copy with "+
		"`icebath sessions export --format md --session-id <id>` (not capped), or raise the limit "+
		"with `icebath config set sessions.max_resume_messages 0`.", e.MessageCount, e.Limit)
}

func (e *SessionResumeTooLargeError) ErrorKind() string { return "ValueError" }

// A ValueError refusing an in memory JSON export past the export cap
type SessionExportTooLargeError struct {
	SessionID           string
	MessageCount, Limit int
}

func (e *SessionExportTooLargeError) Error() string {
	limit, _ := templateutil.Format("{v:,}", map[string]any{"v": int64(e.Limit)})
	return fmt.Sprintf("Session '%s' has more than %s exportable messages; the JSON/JSONL "+
		"backup is built in memory and capped per session. Use the uncapped "+
		"`icebath sessions export --format md --session-id <id>`, or set sessions.max_export_messages: 0 in config.yaml to disable the guard.", e.SessionID, limit)
}

func (e *SessionExportTooLargeError) ErrorKind() string { return "ValueError" }

// Pid existence probe of lease holders, an error being probe doubt, swapped by tests
var holderPIDExists = func(pid int) (bool, error) { return psutilPIDExists(pid), nil }

// Lock holder pid token of a compression lease
var compressionHolderPIDRe = mustPyRe(`(?:^|:)pid=(\d+)(?::|$)`)

// Reports a local pid lock holder provably gone, foreign namespaces and probe doubt keeping the lease
func compressionLockHolderProcessIsDead(holder string) bool {
	m, err := compressionHolderPIDRe.FindStringMatch(holder)
	if err != nil || m == nil {
		return false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(m.GroupByNumber(1).String()), 10, 64)
	if err != nil || n <= 0 || (n == int64(os.Getpid())) || !HolderPidCheckable(holder) {
		return false
	}
	if n > math.MaxInt32 {
		return false
	}
	alive, err := holderPIDExists(int(n))
	return err == nil && !alive
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

// State database path the session layer opens when no path is pinned
func ResolveDefaultDBPath(ctx context.Context) string { return defaultDBPath(ctx) }

// In process guard bypass and extra production roots the isolation guard denies
var (
	stateDBGuardBypass         = false
	stateDBGuardExtraDenyRoots []string
	stateDBGuardMu             sync.Mutex
)

// Lead of the refusal a test gets for opening the real state database, the one failure never cached
const LiveSystemGuardLead = "live-system guard: "

// Refusal a test context process gets for opening a production state database
type LiveSystemGuardError struct {
	Path string
	Root string
}

func (e *LiveSystemGuardError) Error() string {
	return LiveSystemGuardLead + "test attempted to open production state.db at " + e.Path +
		" (under real Icebath root " + e.Root + "). Tests run against a temporary ICEBATH_HOME: " +
		"set ICEBATH_HOME to a temporary directory or pass an explicit temporary db path. " +
		"A test or spawned child process that genuinely needs the live database exports " +
		stateDBGuardBypassEnv + "=1 in its environment."
}

// Exception class the init error record names for the refusal
func (e *LiveSystemGuardError) ErrorKind() string { return "RuntimeError" }

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
	if root, ok := developerStateRoot(); ok {
		roots = append(roots, root)
	}
	for _, r := range extra {
		if p, err := resolvePythonPath(r); err == nil {
			roots = append(roots, p)
		}
	}
	for _, root := range roots {
		if isProductionStateDB(resolved, root) {
			return &LiveSystemGuardError{Path: resolved, Root: root}
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
	case []any, *jsonutil.Object, map[string]any:
		return false, &errorutil.Error{Kind: "TypeError", Msg: fmt.Sprintf("the message role is %s, expected text", errchain.Kind(r))}
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
	text = strings.TrimLeftFunc(text, unicode.IsSpace)
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
			if role, _ := mappingGet(msg, "role"); isMapping(msg) && anyutil.Equal(role, "assistant") {
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
	if !anyutil.Equal(role, "assistant") || !anyutil.IsTrue(calls) {
		return false
	}
	content, _ := mappingGet(msg, "content")
	text, ok := content.(string)
	return ok && staleToolCallMarkerRe.MatchString(strings.TrimSpace(text))
}

// Blanks stale marker assistant content in place, keeping tool call and result pairing
func stripStaleToolCallMarkers(messages []any) []any {
	repaired := 0
	for _, msg := range messages {
		if isStaleToolCallMarkerMessage(msg) {
			switch m := msg.(type) {
			case map[string]any:
				m["content"] = ""
			case *jsonutil.Object:
				m.Set("content", "")
			}
			repaired++
		}
	}
	if repaired > 0 {
		pkgLog().Info(fmt.Sprintf("Cleared %d stale tool-call marker message(s) while restoring session (#78148)", repaired))
	}
	return messages
}
