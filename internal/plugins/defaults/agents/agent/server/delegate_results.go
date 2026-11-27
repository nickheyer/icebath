package server

import (
	"cmp"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	delegationconfig "github.com/nickheyer/icebath/internal/delegation/config"
	"github.com/nickheyer/icebath/internal/tools"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/redact"
	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Statuses of a subagent that did not finish its task
var failedStatuses = map[string]bool{"failed": true, "error": true, "timeout": true}

// What one subagent came to, as the agent that delegated reads it
type entry struct {
	TaskIndex int `json:"task_index"`
	// completed, failed, error, timeout or interrupted
	Status string `json:"status"`
	// The subagent's own final report, null when it gave none
	Summary  *string `json:"summary"`
	Error    string  `json:"error,omitempty"`
	APICalls int     `json:"api_calls"`
	Duration float64 `json:"duration_seconds"`
	Model    string  `json:"model,omitempty"`
	// completed, max_iterations, interrupted, error or timeout
	ExitReason string `json:"exit_reason,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Tokens     *usage `json:"tokens,omitempty"`
	ToolTrace  []step `json:"tool_trace,omitempty"`
	// Dollars its model calls cost, and how sure that figure is
	Cost       *float64 `json:"cost_usd,omitempty"`
	CostStatus string   `json:"cost_status,omitempty"`
	Route      *routed  `json:"route,omitempty"`
	// What the failure was caused by, as the provider's refusal said
	FailureReason string `json:"failure_reason,omitempty"`
	// Whether the final answer satisfied the output schema of its task, after how many correction turns, and what failed
	SchemaValid   *bool    `json:"schema_valid,omitempty"`
	SchemaRetries int      `json:"schema_retries,omitempty"`
	SchemaErrors  []string `json:"schema_errors,omitempty"`
	SchemaNote    string   `json:"schema_note,omitempty"`
	// Guidance sent to it that never reached it
	MissedSteer string `json:"missed_steer,omitempty"`
	// How a subagent that stopped making progress timed out
	TimeoutSeconds *float64 `json:"timeout_seconds,omitempty"`
	TimedOutAfter  *float64 `json:"timed_out_after_seconds,omitempty"`
	TimeoutPhase   string   `json:"timeout_phase,omitempty"`
	LastEventAge   *float64 `json:"last_event_age,omitempty"`
	DiagnosticPath string   `json:"diagnostic_path,omitempty"`
	// Its git worktree and what it left in it
	Worktree *worktreeReport `json:"worktree,omitempty"`
	// Background processes it handed to the parent, left running and killed with it, and finished unread
	HandedOff []handed `json:"handed_off_processes,omitempty"`
	Orphaned  []orphan `json:"orphaned_processes,omitempty"`
	Unread    []unread `json:"unread_completions,omitempty"`
	Stale     []string `json:"stale_paths,omitempty"`
	// The summary was cut to fit the parent's context, the whole of it saved to the file
	SummaryTruncated bool   `json:"summary_truncated,omitempty"`
	SummaryFullPath  string `json:"summary_full_path,omitempty"`
	LiveTranscript   string `json:"live_transcript,omitempty"`
	// Role it ran as and its cost, for the parent's own rollup, never shown
	role string
	cost float64
	// Summary as the files and memory of the profile keep it, its secrets always masked
	forced string
}

type usage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
}

// One tool call of a subagent as its trace shows it: the tool, the size and targets of its arguments and how it went
type step struct {
	Tool      string  `json:"tool"`
	ArgsBytes int     `json:"args_bytes"`
	Input     summary `json:"input_summary"`
	Result    *int    `json:"result_bytes,omitempty"`
	Status    string  `json:"status,omitempty"`
}

// Argument names of a call and the paths and addresses it acted on, without the rest of its arguments
type summary struct {
	Keys    []string       `json:"argument_keys"`
	Targets map[string]any `json:"targets"`
}

// What a backend routed subagent ran on
type routed struct {
	Backend          string `json:"backend"`
	Label            string `json:"label,omitempty"`
	Refused          bool   `json:"refused,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	ServiceTier      string `json:"service_tier,omitempty"`
	FallbackActivate bool   `json:"fallback_activated,omitempty"`
}

type handed struct {
	Process string `json:"session_id"`
	Command string `json:"command"`
	Note    string `json:"note"`
}

type orphan struct {
	Process string `json:"session_id"`
	Command string `json:"command"`
	Runtime int    `json:"runtime_seconds"`
}

type unread struct {
	Process string `json:"session_id"`
	Command string `json:"command"`
	Code    *int   `json:"exit_code"`
	Tail    string `json:"output_tail"`
}

type worktreeReport struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Commits int    `json:"commits"`
	Dirty   bool   `json:"dirty"`
	Pruned  bool   `json:"pruned"`
	Archive string `json:"archive,omitempty"`
	Failed  bool   `json:"inspection_failed,omitempty"`
	Note    string `json:"note,omitempty"`
}

func (e *entry) summaryText() string {
	if e.Summary == nil {
		return ""
	}
	return *e.Summary
}

func text(s string) *string { return &s }

// Entry of a task that never ran or whose subagent gave no result: why, with nothing done
func failedEntry(index int, status, reason string, duration float64) *entry {
	return &entry{TaskIndex: index, Status: status, Error: reason, Duration: duration}
}

// Child produced text as the parent reads it: its secrets masked unless the profile turned that off, or always when forced for what is
// kept on disk; the values the vault filled masked whatever the setting says
func childText(cfg *config.Config, text string, force bool) string {
	text = redact.Filled(cfg.Profile(), text)
	if force || cfg.GetBool(toolsconfig.RedactSecrets) {
		return redact.Text(text, false, redact.Ends)
	}
	return text
}

// Characters of a summary the parent gets whole, against the room left in its context; never under this
const (
	minSummaryChars = 2000
	// Share of the parent's free context the summaries of one batch may take together
	summaryShare = 0.5
)

// Cuts the summaries of a batch so together they fit the parent's context and the profile's ceiling, each one's whole text saved to a file
// the parent can page through; free is the parent's room in tokens, 0 when not known
func (a *Agent) budgetSummaries(cfg *config.Config, env *tools.Env, results []*entry, free int) {
	var long []*entry
	for _, e := range results {
		if e.summaryText() != "" {
			long = append(long, e)
		}
	}
	if len(long) == 0 {
		return
	}
	var limits []int
	if ceiling := max(cfg.GetInt(delegationconfig.MaxSummaryChars), 0); ceiling > 0 {
		limits = append(limits, ceiling)
	}
	if free != 0 {
		if free < 0 {
			limits = append(limits, minSummaryChars)
		} else {
			limits = append(limits, max(minSummaryChars, int(float64(free)*summaryShare)/len(long)*4))
		}
	}
	if len(limits) == 0 {
		return
	}
	limit := slices.Min(limits)
	for _, e := range long {
		summary := e.summaryText()
		length := utf8.RuneCountInString(summary)
		if length <= limit {
			continue
		}
		path := spillSummary(cfg, e.TaskIndex, cmp.Or(e.forced, childText(cfg, summary, true)))
		shown := path
		if path != "" && env != nil && env.Visible != nil {
			shown = env.Visible(path)
		}
		e.Summary = text(headTail(summary, limit, shown))
		e.forced = headTail(cmp.Or(e.forced, summary), limit, shown)
		e.SummaryTruncated, e.SummaryFullPath = true, shown
		slog.Debug(fmt.Sprintf("[subagent-%d] summary trimmed %d → ~%d chars (spill=%s)", e.TaskIndex, length, limit, cmp.Or(shown, "none")))
	}
}

// Saves a whole summary under the profile's delegation cache, the path or empty when it could not be written
func spillSummary(cfg *config.Config, index int, summary string) string {
	dir := filepath.Join(cfg.ProfileDir(cfg.Profile()), "cache", "delegation")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Debug("Failed to spill subagent summary to file: " + err.Error())
		return ""
	}
	now := time.Now()
	path := filepath.Join(dir, fmt.Sprintf("subagent-summary-%d-%s_%06d.txt", index, now.Format("20060102_150405"), now.Nanosecond()/1000))
	if err := fsx.Write(path, []byte(summary), 0o600); err != nil {
		slog.Debug("Failed to spill subagent summary to file: " + err.Error())
		return ""
	}
	return path
}

// Three quarters of the room for the start of a summary and the rest for its end, each cut at a line where one is near, and a footer
// saying where the whole of it is
func headTail(summary string, limit int, path string) string {
	runes := []rune(summary)
	headRoom := limit * 3 / 4
	tailRoom := limit - headRoom
	head := runes[:min(headRoom, len(runes))]
	tail := runes
	if tailRoom > 0 && tailRoom < len(runes) {
		tail = runes[len(runes)-tailRoom:]
	}
	if nl := lastIndex(head, '\n'); float64(nl) > float64(headRoom)*0.5 {
		head = head[:nl]
	}
	if nl := slices.Index(tail, '\n'); nl >= 0 && float64(nl) < float64(tailRoom)*0.5 {
		tail = tail[nl+1:]
	}
	rule := strings.Repeat("─", 8)
	footer := []string{"", rule + " [SUMMARY TRUNCATED] " + rule,
		fmt.Sprintf("Showing %s chars (head) + %s chars (tail) of %s total — trimmed to protect the parent's context window.",
			strs.Thousands(int64(len(head))), strs.Thousands(int64(len(tail))), strs.Thousands(int64(len(runes))))}
	if path != "" {
		footer = append(footer, "Full subagent output saved to: "+path,
			fmt.Sprintf("To read the omitted middle: read_file path=\"%s\" offset=%d limit=200  (the file is the complete summary; raise/lower offset to "+
				"page through it).", path, strings.Count(string(head), "\n")+2))
	} else {
		footer = append(footer, "Full output could not be stored to disk; the head+tail above is all that was preserved.")
	}
	footer = append(footer, strings.Repeat("─", 37))
	return string(head) + "\n\n[... middle omitted — see footer ...]\n\n" + string(tail) + strings.Join(footer, "\n")
}

func lastIndex(runes []rune, r rune) int {
	for i := len(runes) - 1; i >= 0; i-- {
		if runes[i] == r {
			return i
		}
	}
	return -1
}

// Lines telling the parent what became of the background processes of one subagent: those handed to it, those killed with it, and those
// that finished without the subagent reading them
func processLines(e *entry) []string {
	var lines []string
	for _, h := range e.HandedOff {
		lines = append(lines, fmt.Sprintf("Handed off to you: %s (%s) — %s. You own it now; its completion notice will arrive here.", h.Process,
			strs.Head(h.Command, 120), h.Note))
	}
	if len(e.Orphaned) > 0 {
		var parts []string
		for _, o := range e.Orphaned {
			parts = append(parts, fmt.Sprintf("%s `%s` (%ds)", o.Process, strs.Head(o.Command, 100), o.Runtime))
		}
		lines = append(lines, fmt.Sprintf("Child left %d background process(es) running that were TERMINATED with it (subagent process notices never "+
			"reach you): %s. Re-launch in this session anything you still need.", len(e.Orphaned), strings.Join(parts, "; ")))
	}
	for _, u := range e.Unread {
		code := "None"
		if u.Code != nil {
			code = fmt.Sprint(*u.Code)
		}
		lines = append(lines, fmt.Sprintf("Child's process %s `%s` finished (exit code %s) but the child never read its result; output tail:\n%s", u.Process,
			strs.Head(u.Command, 100), code, u.Tail))
	}
	return lines
}

// Errors a provider gives for a model id it doesn't know
var modelNotFound = []string{"is not a valid model", "model not found", "model_not_found"}

// Lines warning that the subagent model of the settings was refused as an unknown model, nil when it wasn't
func (a *Agent) modelRejected(cfg *config.Config, results []*entry) []string {
	model := strings.ToLower(strings.TrimSpace(cfg.GetString(delegationconfig.Model)))
	if model == "" {
		return nil
	}
	rejected := slices.ContainsFunc(results, func(e *entry) bool {
		text := strings.ToLower(e.Error + " " + e.summaryText())
		return strings.Contains(text, model) && slices.ContainsFunc(modelNotFound, func(p string) bool { return strings.Contains(text, p) })
	})
	if !rejected {
		return nil
	}
	lines := []string{"", fmt.Sprintf("⚠ SUBAGENT MODEL REJECTED: the configured Subagent Model %q was rejected by provider %q (HTTP 400: not a valid model ID).",
		cfg.GetString(delegationconfig.Model), cmp.Or(strings.TrimSpace(cfg.GetString(delegationconfig.Provider)), "configured provider")),
		"Every task in this batch failed for this reason before doing any work.",
		"Check it with `icebath config get delegation.model`."}
	if len(cfg.GetStringSlice(delegationconfig.Fallbacks)) == 0 {
		lines = append(lines, "No fallback chain is configured, so no failover was attempted.")
	}
	return lines
}

// Said in place of a summary cut short by the step limit
const truncatedNote = "[TRUNCATED — subagent hit its iteration cap; the summary below may be incomplete. Verify before relying on it, or re-dispatch " +
	"the unfinished part.]"

func done(status string) bool { return status == "completed" || status == "success" }

// Seconds as a figure with up to two decimals, as the results give durations
func seconds(d time.Duration) float64 {
	return float64(d.Round(10*time.Millisecond).Milliseconds()) / 1000
}
