package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

var (
	bareDays = regexp.MustCompile(`^\d+(?:\.\d+)?$`)
	span     = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*(s|sec|secs|second|seconds|m|min|mins|minute|minutes|h|hr|hrs|hour|hours|d|day|days|w|wk|wks|week|weeks)$`)
	units    = map[byte]float64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 604800}
	isoForms = []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"}
)

// Filter flags prune, archive and export share
type filters struct {
	olderThan, newerThan, before, after                                  string
	source, title, endReason, cwd, model, provider, user, chat, chatType string
	branch                                                               string
	minMessages, maxMessages, minTokens, maxTokens                       int64
	minCost, maxCost                                                     float64
	minToolCalls, maxToolCalls                                           int64
	dryRun, yes                                                          bool
	cmd                                                                  *cobra.Command
}

func (f *filters) bind(cmd *cobra.Command, olderHelp string) {
	f.cmd = cmd
	flags := cmd.Flags()
	flags.StringVar(&f.olderThan, "older-than", "", olderHelp)
	flags.StringVar(&f.newerThan, "newer-than", "", "Only match sessions active within the last AGE (e.g. '5h', '2d') or after an ISO timestamp")
	flags.StringVar(&f.before, "before", "", "Only match sessions started before TIME (duration ago like '5h', or ISO timestamp like '2026-07-05 14:30')")
	flags.StringVar(&f.after, "after", "", "Only match sessions started at/after TIME (duration ago like '5h', or ISO timestamp)")
	flags.StringVar(&f.source, "source", "", "Only match sessions from this source")
	flags.StringVar(&f.title, "title", "", "Only match sessions whose title contains this substring")
	flags.StringVar(&f.endReason, "end-reason", "", "Only match sessions with this end reason")
	flags.StringVar(&f.cwd, "cwd", "", "Only match sessions whose working directory is under this path")
	flags.Int64Var(&f.minMessages, "min-messages", 0, "Only match sessions with >= N messages")
	flags.Int64Var(&f.maxMessages, "max-messages", 0, "Only match sessions with <= N messages")
	flags.StringVar(&f.model, "model", "", "Only match sessions whose model name contains this substring (e.g. 'sonnet', 'gpt-5', 'icebath')")
	flags.StringVar(&f.provider, "provider", "", "Only match sessions billed through this provider (e.g. openrouter, anthropic, openai-codex)")
	flags.StringVar(&f.user, "user", "", "Only match sessions from this user ID")
	flags.StringVar(&f.chat, "chat-id", "", "Only match sessions from this chat/channel ID")
	flags.StringVar(&f.chatType, "chat-type", "", "Only match sessions with this chat type (e.g. dm, group)")
	flags.StringVar(&f.branch, "branch", "", "Only match sessions whose git branch contains this substring")
	flags.Int64Var(&f.minTokens, "min-tokens", 0, "Only match sessions with >= N total tokens (input+output)")
	flags.Int64Var(&f.maxTokens, "max-tokens", 0, "Only match sessions with <= N total tokens (input+output)")
	flags.Float64Var(&f.minCost, "min-cost", 0, "Only match sessions costing >= N USD (actual or estimated)")
	flags.Float64Var(&f.maxCost, "max-cost", 0, "Only match sessions costing <= N USD (actual or estimated)")
	flags.Int64Var(&f.minToolCalls, "min-tool-calls", 0, "Only match sessions with >= N tool calls")
	flags.Int64Var(&f.maxToolCalls, "max-tool-calls", 0, "Only match sessions with <= N tool calls")
	flags.BoolVar(&f.dryRun, "dry-run", false, "List matching sessions without changing anything")
	flags.BoolVarP(&f.yes, "yes", "y", false, "Skip confirmation")
}

// Names of the flags that filter
var filterFlags = []string{"older-than", "newer-than", "before", "after", "source", "title", "end-reason", "cwd", "min-messages", "max-messages", "model",
	"provider", "user", "chat-id", "chat-type", "branch", "min-tokens", "max-tokens", "min-cost", "max-cost", "min-tool-calls", "max-tool-calls"}

// Whether any filter flag was given
func (f *filters) any() bool {
	for _, name := range filterFlags {
		if f.cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func (f *filters) set(name string) bool {
	return f.cmd.Flags().Changed(name)
}

// Point in time a value names: a bare number of days ago, a duration ago, or an ISO time read as local time unless it says otherwise
func pointInTime(flag, raw string, now time.Time) (time.Time, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if bareDays.MatchString(value) {
		days, _ := strconv.ParseFloat(value, 64)
		return now.Add(-time.Duration(days * 86400 * float64(time.Second))), nil
	}
	if match := span.FindStringSubmatch(value); match != nil {
		n, _ := strconv.ParseFloat(match[1], 64)
		return now.Add(-time.Duration(n * units[match[2][0]] * float64(time.Second))), nil
	}
	for _, form := range isoForms {
		if t, err := time.ParseInLocation(form, strings.TrimSpace(raw), time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("Invalid value for --%s: '%s'. Use a duration like '5h', '30m', '2d', '1w', a bare number of days, or an ISO timestamp like "+
		"'2026-07-05' or '2026-07-05 14:30'.", flag, raw)
}

func shown(t *timestamppb.Timestamp) string {
	if t == nil {
		return "-"
	}
	return t.AsTime().Local().Format("2006-01-02 15:04")
}

// Filter the flags make, with the default age for a prune given no filter
func (f *filters) build(defaultAge string) (*sessionsv1.Filter, error) {
	now := time.Now()
	out := &sessionsv1.Filter{Source: f.source, Title: f.title, EndReason: f.endReason, Directory: f.cwd, Model: f.model, Provider: f.provider,
		UserId: f.user, ChatId: f.chat, ChatType: f.chatType, Branch: f.branch}
	older := f.olderThan
	if older == "" && !f.any() {
		older = defaultAge
	}
	for _, bound := range []struct {
		flag, raw string
		into      **timestamppb.Timestamp
	}{{"older-than", older, &out.ActiveBefore}, {"newer-than", f.newerThan, &out.ActiveAfter}, {"before", f.before, &out.StartedBefore},
		{"after", f.after, &out.StartedAfter}} {
		if bound.raw == "" {
			continue
		}
		t, err := pointInTime(bound.flag, bound.raw, now)
		if err != nil {
			return nil, err
		}
		*bound.into = timestamppb.New(t)
	}
	if out.StartedAfter != nil && out.StartedBefore != nil && !out.StartedAfter.AsTime().Before(out.StartedBefore.AsTime()) {
		return nil, fmt.Errorf("Empty start-time window: the --after bound (%s) is not earlier than the --before bound (%s).", shown(out.StartedAfter),
			shown(out.StartedBefore))
	}
	if out.ActiveAfter != nil && out.ActiveBefore != nil && !out.ActiveAfter.AsTime().Before(out.ActiveBefore.AsTime()) {
		return nil, fmt.Errorf("Empty activity window: the --newer-than bound (%s) is not earlier than the --older-than bound (%s).", shown(out.ActiveAfter),
			shown(out.ActiveBefore))
	}
	whole := func(name string, value int64, into **int64) {
		if f.set(name) {
			*into = &value
		}
	}
	whole("min-messages", f.minMessages, &out.MinMessages)
	whole("max-messages", f.maxMessages, &out.MaxMessages)
	whole("min-tokens", f.minTokens, &out.MinTokens)
	whole("max-tokens", f.maxTokens, &out.MaxTokens)
	whole("min-tool-calls", f.minToolCalls, &out.MinToolCalls)
	whole("max-tool-calls", f.maxToolCalls, &out.MaxToolCalls)
	if f.set("min-cost") {
		out.MinCost = &f.minCost
	}
	if f.set("max-cost") {
		out.MaxCost = &f.maxCost
	}
	return out, nil
}

// What the filter matches, in words
func describe(f *sessionsv1.Filter) string {
	var parts []string
	add := func(on bool, text string) {
		if on {
			parts = append(parts, text)
		}
	}
	add(f.ActiveBefore != nil, "last active before "+shown(f.ActiveBefore))
	add(f.ActiveAfter != nil, "last active after "+shown(f.ActiveAfter))
	add(f.StartedBefore != nil, "started before "+shown(f.StartedBefore))
	add(f.StartedAfter != nil, "started after "+shown(f.StartedAfter))
	add(f.GetSource() != "", "source '"+f.GetSource()+"'")
	add(f.GetTitle() != "", "title contains '"+f.GetTitle()+"'")
	add(f.GetEndReason() != "", "end reason '"+f.GetEndReason()+"'")
	add(f.GetDirectory() != "", "cwd under '"+f.GetDirectory()+"'")
	add(f.MinMessages != nil, fmt.Sprintf(">= %d messages", f.GetMinMessages()))
	add(f.MaxMessages != nil, fmt.Sprintf("<= %d messages", f.GetMaxMessages()))
	add(f.GetModel() != "", "model contains '"+f.GetModel()+"'")
	add(f.GetProvider() != "", "provider '"+f.GetProvider()+"'")
	add(f.GetUserId() != "", "user '"+f.GetUserId()+"'")
	add(f.GetChatId() != "", "chat '"+f.GetChatId()+"'")
	add(f.GetChatType() != "", "chat type '"+f.GetChatType()+"'")
	add(f.GetBranch() != "", "git branch contains '"+f.GetBranch()+"'")
	add(f.MinTokens != nil, fmt.Sprintf(">= %d tokens", f.GetMinTokens()))
	add(f.MaxTokens != nil, fmt.Sprintf("<= %d tokens", f.GetMaxTokens()))
	add(f.MinCost != nil, fmt.Sprintf(">= $%g", f.GetMinCost()))
	add(f.MaxCost != nil, fmt.Sprintf("<= $%g", f.GetMaxCost()))
	add(f.MinToolCalls != nil, fmt.Sprintf(">= %d tool calls", f.GetMinToolCalls()))
	add(f.MaxToolCalls != nil, fmt.Sprintf("<= %d tool calls", f.GetMaxToolCalls()))
	if len(parts) == 0 {
		return "no filters (all ended sessions)"
	}
	return strings.Join(parts, ", ")
}
