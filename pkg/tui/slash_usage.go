package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session totals panel, model, token counts, calls, the context line and compressions
func usageTotalsSections(r *tuigateway.Usage) []panelSection {
	sections := []panelSection{{Rows: [][2]string{
		{tr("slashCmd.session.usage.rowModel"), r.Model}, {tr("slashCmd.session.usage.rowInputTokens"), groupDigits(float64(r.Input))},
		{tr("slashCmd.session.usage.rowOutputTokens"), groupDigits(float64(r.Output))}, {tr("slashCmd.session.usage.rowTotalTokens"), groupDigits(float64(r.Total))},
		{tr("slashCmd.session.usage.rowApiCalls"), groupDigits(float64(r.Calls))},
	}}}
	if val(r.ContextMax) > 0 {
		mark := ""
		if val(r.ContextEstimated) {
			mark = "~"
		}
		text := tr("slashCmd.session.usage.context", mark+groupDigits(float64(val(r.ContextUsed))), groupDigits(float64(val(r.ContextMax))), mark+strconv.FormatInt(val(r.ContextPercent), 10))
		sections = append(sections, panelSection{Text: &text})
	}
	if n := val(r.Compressions); n > 0 {
		text := tr("slashCmd.session.usage.compressions", strconv.FormatInt(n, 10))
		sections = append(sections, panelSection{Text: &text})
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
			r.sys(tr("slashCmd.session.usage.noCalls"))
			return nil
		}
		a.panel(tr("slashCmd.session.usage.usageTitle"), usageTotalsSections(res))
		return nil
	}))
}
