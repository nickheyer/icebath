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

// Two bar dollar view of a usage model, the plan allowance and the top-up balance, nothing while unavailable
func usageBarsText(model map[string]any) []string {
	if model == nil || !truthyJS(model["available"]) {
		return nil
	}
	plan := "plan"
	if truthyJS(model["plan_name"]) {
		plan = jsValueString(model["plan_name"])
	}
	plan = sliceUnits(plan+strings.Repeat(" ", max(0, 8-unitLen(plan))), 8)
	var lines []string
	if bar, isDict := model["plan_bar"].(map[string]any); isDict {
		pct := ""
		if bar["pct_used"] != nil {
			pct = " · " + jsTemplate(bar, "pct_used") + "% used"
		}
		lines = append(lines, plan+"["+usageBar(jsNumberOf(bar["fill_fraction"]))+"]  "+jsTemplate(bar, "remaining_display")+" left of "+jsTemplate(bar, "total_display")+pct)
	}
	if bar, isDict := model["topup_bar"].(map[string]any); isDict {
		lines = append(lines, "top-up  ["+usageBar(1)+"]  "+jsTemplate(bar, "remaining_display")+" · never expires")
	}
	if truthyJS(model["total_spendable_display"]) && truthyJS(model["has_topup"]) {
		lines = append(lines, "Total spendable: "+jsValueString(model["total_spendable_display"]))
	}
	return lines
}

// Balance panel of the shared dollar usage model, the plan, its bars and the free or low balance note
func (a *app) usageBalancePanel(model map[string]any, bars []string) {
	var sections []panelSection
	text := func(s string) { sections = append(sections, panelSection{Text: &s}) }
	status, _ := model["status"].(string)
	plan := ""
	switch {
	case model["plan_name"] != nil:
		plan = jsValueString(model["plan_name"])
	case status == "free":
		plan = tr("slashCmd.session.usage.freePlan")
	}
	if plan != "" {
		if truthyJS(model["renews_display"]) {
			text(tr("slashCmd.session.usage.planRenews", plan, jsValueString(model["renews_display"])))
		} else {
			text(tr("slashCmd.session.usage.plan", plan))
		}
	}
	if len(bars) > 0 {
		text(strings.Join(bars, "\n"))
	}
	switch status {
	case "free":
		text(tr("slashCmd.session.usage.freeNote"))
	case "low":
		left := tr("slashCmd.session.usage.lowBalanceFallback")
		if model["total_spendable_display"] != nil {
			left = jsValueString(model["total_spendable_display"])
		}
		text(tr("slashCmd.session.usage.lowNote", left))
	}
	a.panel(tr("slashCmd.session.usage.balanceTitle"), sections)
}

// Session totals and balance from session.usage, the dollar usage model first and credit lines without it
func (a *app) sessionUsage(r *slashRun) tea.Cmd {
	return tea.Batch(r.ran(), r.rpc("session.usage", map[string]any{"session_id": nilIfEmpty(r.sid)}, func(res map[string]any) tea.Cmd {
		number := func(key string) float64 {
			n, _ := res[key].(float64)
			return n
		}
		a.store.ui.Usage = map[string]any{"calls": number("calls"), "input": number("input"), "output": number("output"), "total": number("total")}
		shown := false
		model, _ := res["usage"].(map[string]any)
		bars := usageBarsText(model)
		switch lines := rpcStrings(res["credits_lines"]); {
		case model != nil && truthyJS(model["available"]) && (len(bars) > 0 || model["status"] == "free"):
			a.usageBalancePanel(model, bars)
			shown = true
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
