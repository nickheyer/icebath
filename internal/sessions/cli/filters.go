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
	"github.com/nickheyer/icebath/pkg/strs"
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
	flags.StringVar(&f.newerThan, "newer-than", "", "Only sessions used within this time, such as 5h or 2d, or since a date")
	flags.StringVar(&f.before, "before", "", "Only sessions started before a date, or more than a time such as 5h ago")
	flags.StringVar(&f.after, "after", "", "Only sessions started since a date, or within a time such as 5h")
	flags.StringVar(&f.source, "source", "", "Only sessions from this source, such as cli or telegram")
	flags.StringVar(&f.title, "title", "", "Only sessions whose title contains this text")
	flags.StringVar(&f.endReason, "end-reason", "", "Only sessions that ended for this reason")
	flags.StringVar(&f.cwd, "cwd", "", "Only sessions whose folder is inside this path")
	flags.Int64Var(&f.minMessages, "min-messages", 0, "Only sessions with at least this many messages")
	flags.Int64Var(&f.maxMessages, "max-messages", 0, "Only sessions with at most this many messages")
	flags.StringVar(&f.model, "model", "", "Only sessions whose model name contains this text, such as sonnet")
	flags.StringVar(&f.provider, "provider", "", "Only sessions billed through this provider, such as openrouter")
	flags.StringVar(&f.user, "user", "", "Only sessions from this user id")
	flags.StringVar(&f.chat, "chat-id", "", "Only sessions from this chat or channel id")
	flags.StringVar(&f.chatType, "chat-type", "", "Only sessions of this chat type, such as dm or group")
	flags.StringVar(&f.branch, "branch", "", "Only sessions whose git branch contains this text")
	flags.Int64Var(&f.minTokens, "min-tokens", 0, "Only sessions with at least this many tokens in and out")
	flags.Int64Var(&f.maxTokens, "max-tokens", 0, "Only sessions with at most this many tokens in and out")
	flags.Float64Var(&f.minCost, "min-cost", 0, "Only sessions that cost at least this many US dollars")
	flags.Float64Var(&f.maxCost, "max-cost", 0, "Only sessions that cost at most this many US dollars")
	flags.Int64Var(&f.minToolCalls, "min-tool-calls", 0, "Only sessions with at least this many tool calls")
	flags.Int64Var(&f.maxToolCalls, "max-tool-calls", 0, "Only sessions with at most this many tool calls")
	flags.BoolVar(&f.dryRun, "dry-run", false, "List the matching sessions without changing anything")
	flags.BoolVarP(&f.yes, "yes", "y", false, "Go ahead without asking first")
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
	return time.Time{}, fmt.Errorf("--%s takes a time, and “%s” is not one. %s", flag, raw, times)
}

func shown(t *timestamppb.Timestamp) string {
	if t == nil {
		return "Unknown"
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
		return nil, fmt.Errorf("No session can match, because --after, %s, is not earlier than --before, %s.", shown(out.StartedAfter),
			shown(out.StartedBefore))
	}
	if out.ActiveAfter != nil && out.ActiveBefore != nil && !out.ActiveAfter.AsTime().Before(out.ActiveBefore.AsTime()) {
		return nil, fmt.Errorf("No session can match, because --newer-than, %s, is not earlier than --older-than, %s.", shown(out.ActiveAfter),
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
	number := func(n int64) string { return strs.Number(float64(n)) }
	add(f.ActiveBefore != nil, "last used before "+shown(f.ActiveBefore))
	add(f.ActiveAfter != nil, "last used after "+shown(f.ActiveAfter))
	add(f.StartedBefore != nil, "started before "+shown(f.StartedBefore))
	add(f.StartedAfter != nil, "started after "+shown(f.StartedAfter))
	add(f.GetSource() != "", "from “"+f.GetSource()+"”")
	add(f.GetTitle() != "", "with “"+f.GetTitle()+"” in the title")
	add(f.GetEndReason() != "", "ended for “"+f.GetEndReason()+"”")
	add(f.GetDirectory() != "", "in a folder under “"+f.GetDirectory()+"”")
	add(f.MinMessages != nil, "with at least "+number(f.GetMinMessages())+" messages")
	add(f.MaxMessages != nil, "with at most "+number(f.GetMaxMessages())+" messages")
	add(f.GetModel() != "", "with “"+f.GetModel()+"” in the model")
	add(f.GetProvider() != "", "from the provider “"+f.GetProvider()+"”")
	add(f.GetUserId() != "", "from the user “"+f.GetUserId()+"”")
	add(f.GetChatId() != "", "in the chat “"+f.GetChatId()+"”")
	add(f.GetChatType() != "", "of the chat type “"+f.GetChatType()+"”")
	add(f.GetBranch() != "", "on a git branch with “"+f.GetBranch()+"” in its name")
	add(f.MinTokens != nil, "with at least "+number(f.GetMinTokens())+" tokens")
	add(f.MaxTokens != nil, "with at most "+number(f.GetMaxTokens())+" tokens")
	add(f.MinCost != nil, "costing at least $"+strs.Number(f.GetMinCost()))
	add(f.MaxCost != nil, "costing at most $"+strs.Number(f.GetMaxCost()))
	add(f.MinToolCalls != nil, "with at least "+number(f.GetMinToolCalls())+" tool calls")
	add(f.MaxToolCalls != nil, "with at most "+number(f.GetMaxToolCalls())+" tool calls")
	if len(parts) == 0 {
		return "no filters, so every ended session"
	}
	return strings.Join(parts, ", ")
}
