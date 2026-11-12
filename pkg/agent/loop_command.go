package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	loopIntervalTokenRe = pyre.MustCompile(`(?i)^(?=\d)(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$`)
	loopTimesFlagRe     = pyre.MustCompile(`\s--times\s+(\S+)`)
	loopUntilFlagRe     = pyre.MustCompile(`(?s)\s--until\s+(.+)$`)
)

// LoopHelp is the usage text of the loop command
const LoopHelp = "Usage: /loop [interval] <prompt> [--times N] [--until <condition>]\n" +
	"  /loop 5m check the deploy status      — first run now, then every 5m\n" +
	"  /loop every 10m /recap                — loop a slash command\n" +
	"  /loop keep fixing tests until green   — self-paced (backs off while output is unchanged)\n" +
	"  /loop 2m poll CI --times 30           — stop after 30 runs\n" +
	"  /loop 5m watch the queue --until queue is empty\n" +
	"Controls: /loop status · /loop pause · /loop resume · /loop stop\n" +
	"The loop also stops itself when the agent replies with " + LoopCompleteMarker + "."

// Total seconds of a 30s, 5m, 2h or 1h30m token, false for a bare number, prose or zero
func ParseLoopIntervalToken(token string) (int64, bool) {
	if token == "" {
		return 0, false
	}
	m, err := loopIntervalTokenRe.FindStringMatch(pystr.Strip(token))
	if err != nil || m == nil {
		return 0, false
	}
	var parts [3]int64
	for i := range parts {
		if g := m.GroupByNumber(i + 1); g != nil && g.RuneLength > 0 {
			n, err := pyint.Of(g.String())
			if err != nil || !n.IsInt64() {
				return 0, false
			}
			parts[i] = n.Int64()
		}
	}
	total := parts[0]*3600 + parts[1]*60 + parts[2]
	if total <= 0 {
		return 0, false
	}
	return total, true
}

// LoopArgs is a parsed loop spec, IntervalSeconds nil meaning self paced and Error set for unusable input
type LoopArgs struct {
	IntervalSeconds *int64
	Prompt          string
	Times           int64
	Until           string
	Error           string
}

// Parses an interval, prompt, times flag and until flag out of loop command text
func ParseLoopArgs(text string) LoopArgs {
	raw := pystr.Strip(text)
	if raw == "" {
		return LoopArgs{Error: "empty"}
	}
	runes := []rune(raw)
	var times int64
	until := ""
	if m, _ := loopTimesFlagRe.FindStringMatch(raw); m != nil {
		group := m.GroupByNumber(1).String()
		n, err := pyint.Of(group)
		if err != nil || n.Sign() < 1 || !n.IsInt64() {
			return LoopArgs{Error: "--times expects a positive integer, got " + pyrepr.Repr(group)}
		}
		times = n.Int64()
		raw = pystr.Strip(string(runes[:m.RuneIndex]) + string(runes[m.RuneIndex+m.RuneLength:]))
		runes = []rune(raw)
	}
	if m, _ := loopUntilFlagRe.FindStringMatch(raw); m != nil {
		until = pystr.Strip(m.GroupByNumber(1).String())
		raw = pystr.Strip(string(runes[:m.RuneIndex]))
	}
	tokens := pystr.SplitWhitespace(raw, 1)
	if len(tokens) > 0 && pystr.Lower(tokens[0]) == "every" && len(tokens) > 1 {
		raw = tokens[1]
		tokens = pystr.SplitWhitespace(raw, 1)
	}
	var interval *int64
	if len(tokens) > 0 {
		if n, ok := ParseLoopIntervalToken(tokens[0]); ok {
			interval = &n
			raw = ""
			if len(tokens) > 1 {
				raw = pystr.Strip(tokens[1])
			}
		}
	}
	if raw == "" {
		return LoopArgs{Error: "missing prompt (usage: /loop [interval] <prompt>)"}
	}
	return LoopArgs{IntervalSeconds: interval, Prompt: raw, Times: times, Until: until}
}

// Output of a pause control
func loopPauseOutput(ctx context.Context, mgr LoopAutomation) (string, error) {
	state, err := mgr.Pause(ctx, loopDefaultPauseReason)
	if err != nil {
		return "", err
	}
	if state == nil {
		return "No loop set.", nil
	}
	return fmt.Sprintf("⏸ Loop paused: %s\nUse /loop resume to continue.", loopText(state.Prompt)), nil
}

// Output of a resume control
func loopResumeOutput(ctx context.Context, mgr LoopAutomation) string {
	state := mgr.Resume(ctx)
	if state == nil {
		return "No loop to resume."
	}
	return fmt.Sprintf("▶ Loop resumed (%s): %s", state.CadenceLabel(), loopText(state.Prompt))
}

// Output of a control word, false when the argument is a new loop spec
func loopControlOutput(ctx context.Context, mgr LoopAutomation, word string) (string, bool, error) {
	switch word {
	case "", "status":
		return mgr.StatusLine(), true, nil
	case "pause":
		out, err := loopPauseOutput(ctx, mgr)
		return out, true, err
	case "resume":
		return loopResumeOutput(ctx, mgr), true, nil
	case "stop", "clear", "cancel":
		if mgr.Clear(ctx) {
			return "✓ Loop stopped.", true, nil
		}
		return "No active loop.", true, nil
	case "help", "--help", "-h":
		return LoopHelp, true, nil
	}
	return "", false, nil
}

// LoopCommandResult is the printed output of a loop command and whether it created a loop
type LoopCommandResult struct {
	Output  string
	Created bool
}

// Handles the loop command arguments for every surface, storing route on a new loop
func DispatchLoopCommand(ctx context.Context, mgr LoopAutomation, args string, route *pyjson.Object) (LoopCommandResult, error) {
	arg := pystr.Strip(args)
	out, isControl, err := loopControlOutput(ctx, mgr, pystr.Lower(arg))
	if err != nil {
		return LoopCommandResult{}, err
	}
	if isControl {
		return LoopCommandResult{Output: out}, nil
	}
	parsed := ParseLoopArgs(arg)
	if parsed.Error != "" {
		if parsed.Error == "empty" {
			return LoopCommandResult{Output: "Usage: /loop [interval] <prompt> — see /loop help."}, nil
		}
		return LoopCommandResult{Output: "/loop: " + parsed.Error}, nil
	}
	replacing := mgr.HasLoop()
	state, err := mgr.Set(ctx, parsed.Prompt, parsed.IntervalSeconds, parsed.Times, parsed.Until, route)
	if err != nil {
		var pyErr *pyint.Error
		if errors.As(err, &pyErr) && pyErr.Kind == "ValueError" {
			return LoopCommandResult{Output: "/loop: " + pyErr.Msg}, nil
		}
		return LoopCommandResult{}, err
	}
	lines := []string{fmt.Sprintf("↻ Loop set (%s): %s", state.CadenceLabel(), loopText(state.Prompt))}
	if replacing {
		lines = append(lines, "(replaced the previous loop for this session)")
	}
	if parsed.IntervalSeconds != nil && float64(*parsed.IntervalSeconds) < state.IntervalSeconds {
		lines = append(lines, fmt.Sprintf("(interval raised to the %s minimum — loops.min_interval_seconds)",
			FormatLoopInterval(state.IntervalSeconds)))
	}
	if state.Mode == "self_paced" {
		lines = append(lines, fmt.Sprintf("Self-paced: first check in %s; backs off up to %s while nothing changes.",
			FormatLoopInterval(state.CurrentDelay), FormatLoopInterval(float64(loopSelfPacedCeilingSeconds(ctx)))))
	}
	if state.Times != 0 {
		plural := "s"
		if state.Times == 1 {
			plural = ""
		}
		lines = append(lines, fmt.Sprintf("Runs %d time%s, then stops.", state.Times, plural))
	}
	if state.Until != "" {
		lines = append(lines, "Stops when: "+state.Until)
	}
	if state.Times == 0 && state.MaxTicks != 0 {
		lines = append(lines, fmt.Sprintf("Backstop budget: %d ticks (loops.max_ticks; 0 = unlimited).", state.MaxTicks))
	}
	first := state.RemainingLabel()
	if state.Status == "active" {
		first = "fires now, then on the cadence above"
	}
	lines = append(lines, fmt.Sprintf("First wakeup %s. Controls: /loop status · pause · resume · stop.", first))
	return LoopCommandResult{Output: strings.Join(lines, "\n"), Created: true}, nil
}
