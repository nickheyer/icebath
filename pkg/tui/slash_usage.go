package tui

import (
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Session totals panel, model, token counts, calls, the context line and compressions
func usageTotalsSections(r *tuigateway.Usage) []panelSection {
	sections := []panelSection{{Rows: [][2]string{
		{tr(txt.SlashCmdSessionUsageRowModel), r.Model}, {tr(txt.SlashCmdSessionUsageRowInputTokens), groupDigits(float64(r.Input))},
		{tr(txt.SlashCmdSessionUsageRowOutputTokens), groupDigits(float64(r.Output))}, {tr(txt.SlashCmdSessionUsageRowTotalTokens), groupDigits(float64(r.Total))},
		{tr(txt.SlashCmdSessionUsageRowApiCalls), groupDigits(float64(r.Calls))},
	}}}
	if val(r.ContextMax) > 0 {
		mark := ""
		if val(r.ContextEstimated) {
			mark = "~"
		}
		text := tr(txt.SlashCmdSessionUsageContext, mark+groupDigits(float64(val(r.ContextUsed))), groupDigits(float64(val(r.ContextMax))), mark+strconv.FormatInt(val(r.ContextPercent), 10))
		sections = append(sections, panelSection{Text: &text})
	}
	if n := val(r.Compressions); n > 0 {
		text := tr(txt.SlashCmdSessionUsageCompressions, strconv.FormatInt(n, 10))
		sections = append(sections, panelSection{Text: &text})
	}
	return sections
}

// Windows of a provider's rate limits by their wire names, each with the key of its label
var rateLimitWindows = [4]struct {
	wire  string
	label txt.Key
}{
	{"requests_min", txt.SlashCmdSessionUsageRateRequestsMin}, {"requests_hour", txt.SlashCmdSessionUsageRateRequestsHour},
	{"tokens_min", txt.SlashCmdSessionUsageRateTokensMin}, {"tokens_hour", txt.SlashCmdSessionUsageRateTokensHour},
}

// Share of a rate limit window that counts as nearly spent
const rateLimitWarnPercent = 80

// Count the way the usage panel shortens it, 33599 as 33.6K and 7999856 as 8.0M
func compactCount(n float64) string {
	switch {
	case n >= 1e6:
		return strconv.FormatFloat(n/1e6, 'f', 1, 64) + "M"
	case n >= 1e3:
		return strconv.FormatFloat(n/1e3, 'f', 1, 64) + "K"
	}
	return strconv.FormatFloat(n, 'f', 0, 64)
}

// Span of seconds as 58s, 2m 14s or 1h 2m
func compactSeconds(seconds float64) string {
	s := max(int(seconds), 0)
	switch {
	case s < 60:
		return strconv.Itoa(s) + "s"
	case s < 3600 && s%60 == 0:
		return strconv.Itoa(s/60) + "m"
	case s < 3600:
		return strconv.Itoa(s/60) + "m " + strconv.Itoa(s%60) + "s"
	case s%3600/60 == 0:
		return strconv.Itoa(s/3600) + "h"
	}
	return strconv.Itoa(s/3600) + "h " + strconv.Itoa(s%3600/60) + "m"
}

// Rate limit sections of the usage panel from the state the provider's last answer carried: one row for each window that has a limit, then a warning
// for each window nearly spent; none before any answer carried limits
func rateLimitSections(state map[string]any, now time.Time) []panelSection {
	number := func(m map[string]any, key string) float64 {
		n, _ := m[key].(float64)
		return n
	}
	captured := number(state, "captured_at")
	if captured == 0 {
		return nil
	}
	sinceCapture := func(at float64) float64 {
		if at == 0 {
			at = captured
		}
		return max(float64(now.UnixNano())/1e9-at, 0)
	}
	var rows [][2]string
	var warnings []string
	for _, window := range rateLimitWindows {
		bucket, _ := state[window.wire].(map[string]any)
		limit, remaining := number(bucket, "limit"), number(bucket, "remaining")
		if limit <= 0 {
			continue
		}
		used := max(limit-remaining, 0)
		percent := used / limit * 100
		resets := compactSeconds(max(number(bucket, "reset_seconds")-sinceCapture(number(bucket, "captured_at")), 0))
		label := tr(window.label)
		rows = append(rows, [2]string{label, tr(txt.SlashCmdSessionUsageRateRow, strconv.FormatFloat(percent, 'f', 1, 64), compactCount(used), compactCount(limit), compactCount(remaining), resets)})
		if percent >= rateLimitWarnPercent {
			warnings = append(warnings, tr(txt.SlashCmdSessionUsageRateWarning, label, strconv.FormatFloat(percent, 'f', 0, 64), resets))
		}
	}
	if len(rows) == 0 {
		return nil
	}
	freshness := tr(txt.SlashCmdSessionUsageRateJustNow)
	if age := sinceCapture(captured); age >= 5 {
		freshness = tr(txt.SlashCmdSessionUsageRateAgo, compactSeconds(age))
	}
	provider, _ := state["provider"].(string)
	if provider == "" {
		provider = tr(txt.SlashCmdSessionUsageRateProvider)
	} else if first, size := utf8.DecodeRuneInString(provider); size > 0 {
		provider = string(unicode.ToUpper(first)) + provider[size:]
	}
	title := tr(txt.SlashCmdSessionUsageRateLimitsTitle, provider, freshness)
	sections := []panelSection{{Title: &title, Rows: rows}}
	if len(warnings) > 0 {
		sections = append(sections, panelSection{Items: warnings})
	}
	return sections
}

// Session totals from session.usage, the no calls line before the first call
func (a *app) sessionUsage(r *slashRun) tea.Cmd {
	return tea.Batch(r.ran(), ask(r.rpc, r.a.sys, tuigateway.SessionUsage, tuigateway.SessionUsageParams{SessionID: r.sid}, func(res *tuigateway.Usage) tea.Cmd {
		if res == nil {
			return nil
		}
		a.store.ui.Usage = &tuigateway.Usage{Calls: res.Calls, Input: res.Input, Output: res.Output, Total: res.Total}
		if res.Calls == 0 {
			r.sys(tr(txt.SlashCmdSessionUsageNoCalls))
			return nil
		}
		a.panel(tr(txt.SlashCmdSessionUsageUsageTitle), append(usageTotalsSections(res), rateLimitSections(res.RateLimit, a.store.now())...))
		return nil
	}))
}
