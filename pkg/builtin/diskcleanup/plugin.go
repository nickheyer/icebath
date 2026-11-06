package diskcleanup

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshlex"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
)

const preCallTTL = 3600 * time.Second

var drivePath = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func init() { plugins.RegisterBundled(Plugin{}) }

// Compiled-in disk-cleanup plugin
type Plugin struct{}

// Manifest of the disk-cleanup plugin from its plugin.yaml
func (Plugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name:          "disk-cleanup",
		Version:       "2.0.0",
		Description:   "Auto-track and clean up ephemeral files (test scripts, temp outputs, cron logs) created during icebath sessions. Runs via plugin hooks \u2014 no agent action required.",
		Author:        "@LVT382009 (original), NousResearch (plugin port)",
		ProvidesHooks: []string{"pre_tool_call", "post_tool_call", "on_session_end"},
	}
}

// Registers the tool call tracking hooks, the session end cleanup and the slash command
func (Plugin) Register(ctx *plugins.Context) error {
	h := shared
	ctx.RegisterHook("pre_tool_call", h.onPreToolCall, false)
	ctx.RegisterHook("post_tool_call", h.onPostToolCall, false)
	ctx.RegisterHook("on_session_end", h.onSessionEnd, false)
	ctx.RegisterCommand("disk-cleanup", h.handleSlash, "Track and clean up ephemeral icebath session files.", "", "")
	return nil
}

// Snapshot of the argument paths a call saw absent before it ran
type preCall struct {
	taken  time.Time
	absent map[string]bool
}

// Process wide hook state, test files tracked per owner and pre call snapshots per owner and call id
type hooks struct {
	mu               sync.Mutex
	recentTestTracks map[string]map[string]bool
	preCalls         map[[2]string]preCall
}

var shared = newHooks()

// Empty hook state
func newHooks() *hooks {
	return &hooks{recentTestTracks: map[string]map[string]bool{}, preCalls: map[[2]string]preCall{}}
}

// Single path argument of write_file and patch
func extractPathArg(args map[string]any) []string {
	if p, ok := args["path"].(string); ok && p != "" {
		return []string{p}
	}
	return nil
}

// Absolute, home relative and drive letter tokens named in a terminal command
func extractPathsFromTerminal(args map[string]any) []string {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return nil
	}
	var toks []string
	var err error
	if runtime.GOOS == "windows" {
		toks, err = pyshlex.SplitNonPOSIX(cmd)
	} else {
		toks, err = pyshlex.SplitPOSIX(cmd, false)
	}
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var paths []string
	for _, tok := range toks {
		if len(tok) >= 2 && tok[0] == tok[len(tok)-1] && (tok[0] == '\'' || tok[0] == '"') {
			tok = tok[1 : len(tok)-1]
		}
		if (strings.HasPrefix(tok, "/") || strings.HasPrefix(tok, "~") || drivePath.MatchString(tok)) && !seen[tok] {
			seen[tok] = true
			paths = append(paths, tok)
		}
	}
	return paths
}

var pathExtractors = map[string]func(map[string]any) []string{
	"write_file": extractPathArg,
	"patch":      extractPathArg,
	"terminal":   extractPathsFromTerminal,
}

// Owner of a call, the task id, then the session id, then default
func owner(taskID, sessionID string) string {
	if taskID != "" {
		return taskID
	}
	if sessionID != "" {
		return sessionID
	}
	return "default"
}

// String keyword argument, empty when absent or not a string
func kwString(kwargs map[string]any, key string) string {
	s, _ := kwargs[key].(string)
	return s
}

// Records which argument paths are absent before the call and never fails the call
func (h *hooks) onPreToolCall(_ context.Context, kwargs map[string]any) (any, error) {
	extractor := pathExtractors[kwString(kwargs, "tool_name")]
	args, isMap := kwargs["args"].(map[string]any)
	callID := kwString(kwargs, "tool_call_id")
	if callID == "" || extractor == nil || !isMap {
		return nil, nil
	}
	now := time.Now()
	absent := map[string]bool{}
	for _, s := range extractor(args) {
		p, err := pypath.PathExpanduser(s)
		if err != nil {
			absent = map[string]bool{}
			break
		}
		if !exists(p) {
			absent[p] = true
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, snap := range h.preCalls {
		if now.Sub(snap.taken) > preCallTTL {
			delete(h.preCalls, key)
		}
	}
	h.preCalls[[2]string{owner(kwString(kwargs, "task_id"), kwString(kwargs, "session_id")), callID}] = preCall{taken: now, absent: absent}
	return nil, nil
}

// Tracks the ephemeral files this call created, argument paths its own snapshot saw absent that exist now
func (h *hooks) onPostToolCall(ctx context.Context, kwargs map[string]any) (any, error) {
	extractor := pathExtractors[kwString(kwargs, "tool_name")]
	args, isMap := kwargs["args"].(map[string]any)
	callID := kwString(kwargs, "tool_call_id")
	if !isMap || extractor == nil || callID == "" {
		return nil, nil
	}
	own := owner(kwString(kwargs, "task_id"), kwString(kwargs, "session_id"))
	h.mu.Lock()
	key := [2]string{own, callID}
	snap := h.preCalls[key]
	delete(h.preCalls, key)
	h.mu.Unlock()
	if len(snap.absent) == 0 {
		return nil, nil
	}
	for _, pathStr := range extractor(args) {
		p, err := pypath.PathExpanduser(pathStr)
		if err != nil {
			continue
		}
		if !snap.absent[p] || !exists(p) {
			continue
		}
		category, err := GuessCategory(ctx, p)
		if err != nil {
			return nil, err
		}
		if category == "" {
			continue
		}
		tracked, err := Track(ctx, p, category, true)
		if err != nil {
			return nil, err
		}
		if tracked && category == "test" {
			h.mu.Lock()
			if h.recentTestTracks[own] == nil {
				h.recentTestTracks[own] = map[string]bool{}
			}
			h.recentTestTracks[own][p] = true
			h.mu.Unlock()
		}
	}
	return nil, nil
}

// Runs quick cleanup when any test file was tracked during the turn, draining every owner bucket
func (h *hooks) onSessionEnd(ctx context.Context, kwargs map[string]any) (any, error) {
	sid := kwString(kwargs, "session_id")
	if sid == "" {
		sid = "default"
	}
	h.mu.Lock()
	bucket := h.recentTestTracks[sid]
	delete(h.recentTestTracks, sid)
	hadTracks := len(bucket) > 0 || len(h.recentTestTracks) > 0
	h.recentTestTracks = map[string]map[string]bool{}
	h.mu.Unlock()
	if !hadTracks {
		return nil, nil
	}
	summary, err := Quick(ctx)
	if err != nil {
		slog.Debug(fmt.Sprintf("disk-cleanup quick cleanup failed: %v", err))
		return nil, nil
	}
	if summary.Deleted > 0 || summary.EmptyDirs > 0 {
		Log(ctx, fmt.Sprintf("AUTO_QUICK (session_end): deleted=%d dirs=%d freed=%s", summary.Deleted, summary.EmptyDirs, FmtSize(summary.Freed)))
	}
	return nil, nil
}

const helpText = "/disk-cleanup \u2014 ephemeral-file cleanup\n" + `
Subcommands:
  status                     Per-category breakdown + top-10 largest
  dry-run                    Preview what quick/deep would delete
  quick                      Run safe cleanup now (no prompts)
  deep                       Run quick, then list items that need prompts
  track <path> <category>    Manually add a path to tracking
  forget <path>              Stop tracking a path (does not delete)

Categories: temp | test | research | download | chrome-profile | cron-output | other

All operations are scoped to ICEBATH_HOME and /tmp/icebath-*.  # no-tmp: ok ` + "\u2014" + ` legacy scratch scope this plugin cleans up
Test files are auto-tracked on write_file / terminal and auto-cleaned at session end.
`

// One line summary of a quick run with the error count when any removal failed
func fmtSummary(s Summary) string {
	base := fmt.Sprintf("[disk-cleanup] Cleaned %d files + %d empty dirs, freed %s.", s.Deleted, s.EmptyDirs, FmtSize(s.Freed))
	if len(s.Errors) > 0 {
		base += fmt.Sprintf("\n  %d error(s); see cleanup.log.", len(s.Errors))
	}
	return base
}

// Header carrying the item count and total size followed by one category and path line per item
func itemBlock(header string, items []Entry, indent string) ([]string, error) {
	total, err := sumSizes(items)
	if err != nil {
		return nil, err
	}
	header = strings.ReplaceAll(strings.ReplaceAll(header, "{n}", fmt.Sprint(len(items))), "{size}", FmtSize(total))
	lines := []string{header}
	for _, item := range items {
		cat, err := strField(item, "category")
		if err != nil {
			return nil, err
		}
		p, err := strField(item, "path")
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("%s[%s] %s", indent, cat, p))
	}
	return lines, nil
}

// Preview of what quick and deep would delete
func cmdDryRun(ctx context.Context, _ []string) (string, error) {
	auto, prompt, err := DryRun(ctx)
	if err != nil {
		return "", err
	}
	lines := []string{"Dry-run preview (nothing deleted):"}
	a, err := itemBlock("  Auto-delete : {n} files ({size})", auto, "    ")
	if err != nil {
		return "", err
	}
	p, err := itemBlock("  Needs prompt: {n} files ({size})", prompt, "    ")
	if err != nil {
		return "", err
	}
	lines = append(append(lines, a...), p...)
	total, err := sumSizes(append(append([]Entry{}, auto...), prompt...))
	if err != nil {
		return "", err
	}
	lines = append(lines, fmt.Sprintf("\n  Total potential: %s", FmtSize(total)))
	return strings.Join(lines, "\n"), nil
}

// Quick cleanup followed by the items that still need confirmation
func cmdDeep(ctx context.Context, _ []string) (string, error) {
	summary, err := Quick(ctx)
	if err != nil {
		return "", err
	}
	_, promptItems, err := DryRun(ctx)
	if err != nil {
		return "", err
	}
	lines := []string{fmtSummary(summary)}
	if len(promptItems) > 0 {
		block, err := itemBlock("\n{n} item(s) need confirmation ({size}):", promptItems, "  ")
		if err != nil {
			return "", err
		}
		lines = append(lines, block...)
		lines = append(lines, "\nRun `/disk-cleanup forget <path>` to skip, or delete manually via terminal.")
	}
	return strings.Join(lines, "\n"), nil
}

// Sorted allowed categories as a Python list repr
func allowedRepr() string {
	names := make([]string, 0, len(AllowedCategories))
	for k := range AllowedCategories {
		names = append(names, k)
	}
	sort.Strings(names)
	list := make([]any, len(names))
	for i, n := range names {
		list[i] = n
	}
	return pyrepr.Repr(list)
}

// Manually adds a path to tracking under a category
func cmdTrack(ctx context.Context, argv []string) (string, error) {
	if len(argv) < 3 {
		return "Usage: /disk-cleanup track <path> <category>", nil
	}
	pathArg, category := argv[1], argv[2]
	if !AllowedCategories[category] {
		return fmt.Sprintf("Unknown category '%s'. Allowed: %s", category, allowedRepr()), nil
	}
	ok, err := Track(ctx, pathArg, category, true)
	if err != nil {
		return "", err
	}
	if ok {
		return fmt.Sprintf("Tracked %s as '%s'.", pathArg, category), nil
	}
	return fmt.Sprintf("Not tracked (already present, missing, or outside ICEBATH_HOME): %s", pathArg), nil
}

// Stops tracking a path without deleting it
func cmdForget(ctx context.Context, argv []string) (string, error) {
	if len(argv) < 2 {
		return "Usage: /disk-cleanup forget <path>", nil
	}
	n, err := Forget(ctx, argv[1])
	if err != nil {
		return "", err
	}
	if n == 0 {
		return fmt.Sprintf("Not found in tracking: %s", argv[1]), nil
	}
	suffix := "ies"
	if n == 1 {
		suffix = "y"
	}
	return fmt.Sprintf("Removed %d tracking entr%s for %s.", n, suffix, argv[1]), nil
}

var subcommands = map[string]func(context.Context, []string) (string, error){
	"status": func(ctx context.Context, _ []string) (string, error) {
		s, err := GetStatus(ctx)
		if err != nil {
			return "", err
		}
		return FormatStatus(s), nil
	},
	"dry-run": cmdDryRun,
	"quick": func(ctx context.Context, _ []string) (string, error) {
		s, err := Quick(ctx)
		if err != nil {
			return "", err
		}
		return fmtSummary(s), nil
	},
	"deep":   cmdDeep,
	"track":  cmdTrack,
	"forget": cmdForget,
}

// Dispatches a /disk-cleanup invocation to its subcommand, help for none or an unknown one
func (h *hooks) handleSlash(ctx context.Context, rawArgs string) (any, error) {
	argv := pystr.SplitWhitespace(pystr.Strip(rawArgs), -1)
	if len(argv) == 0 || argv[0] == "help" || argv[0] == "-h" || argv[0] == "--help" {
		return helpText, nil
	}
	handler, ok := subcommands[argv[0]]
	if !ok {
		return fmt.Sprintf("Unknown subcommand: %s\n\n%s", argv[0], helpText), nil
	}
	return handler(ctx, argv)
}
