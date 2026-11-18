package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
)

// Session totals panel, model, token counts, calls, the context line and compressions
func usageTotalsSections(r map[string]any) []panelSection {
	number := func(key string) float64 {
		n, _ := r[key].(float64)
		return n
	}
	model, _ := r["model"].(string)
	sections := []panelSection{{Rows: [][2]string{
		{tr("slashCmd.session.usage.rowModel"), model}, {tr("slashCmd.session.usage.rowInputTokens"), groupDigits(number("input"))},
		{tr("slashCmd.session.usage.rowOutputTokens"), groupDigits(number("output"))}, {tr("slashCmd.session.usage.rowTotalTokens"), groupDigits(number("total"))},
		{tr("slashCmd.session.usage.rowApiCalls"), groupDigits(number("calls"))},
	}}}
	if number("context_max") > 0 {
		mark := ""
		if truthyJS(r["context_estimated"]) {
			mark = "~"
		}
		text := tr("slashCmd.session.usage.context", mark+groupDigits(number("context_used")), groupDigits(number("context_max")), mark+jsString(number("context_percent")))
		sections = append(sections, panelSection{Text: &text})
	}
	if n := number("compressions"); n > 0 {
		text := tr("slashCmd.session.usage.compressions", jsString(n))
		sections = append(sections, panelSection{Text: &text})
	}
	return sections
}

// Session totals and the Nous credit lines from session.usage, the credit lines before the totals
func (a *app) sessionUsage(r *slashRun) tea.Cmd {
	return tea.Batch(r.ran(), r.rpc("session.usage", map[string]any{"session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
		number := func(key string) float64 {
			n, _ := res[key].(float64)
			return n
		}
		a.store.ui.Usage = map[string]any{"calls": number("calls"), "input": number("input"), "output": number("output"), "total": number("total")}
		shown := false
		switch lines := rpcStrings(res["credits_lines"]); {
		case len(lines) > 0 && truthyJS(res["credits_usage_window"]):
			for _, line := range lines {
				if line = strings.TrimRightFunc(stripAnsi(line), isJSSpace); line != "" {
					r.sys(line)
					shown = true
				}
			}
		case len(lines) > 0:
			text := strings.Join(lines, "\n")
			a.panel(tr("slashCmd.session.usage.nousBalanceTitle"), []panelSection{{Text: &text}})
			shown = true
		}
		if number("calls") == 0 {
			if !shown {
				r.sys(tr("slashCmd.session.usage.noCalls"))
			}
			r.sys(tr("slashCmd.session.usage.cta"))
			return nil
		}
		a.panel(tr("slashCmd.session.usage.usageTitle"), usageTotalsSections(res))
		r.sys(tr("slashCmd.session.usage.cta"))
		return nil
	}))
}
