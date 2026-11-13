package app

import (
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

// Dim indented billing line with an optional icon, a blank line first when lead is set
func (c *CLI) billingDim(msg, icon string, lead bool) {
	if lead {
		infoPrint("")
	}
	cprint("  " + icon + dimTTY(msg))
}

// Shared dollar usage model of the Nous account, false on any failure
func (c *CLI) tryUsageModel() (usage provider.UsageModel, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			usage, ok = provider.UsageModel{}, false
		}
	}()
	return provider.BuildUsageModel(c.ctx, 10*time.Second), true
}

// Text padded with spaces to width code points then cut to width, as str.ljust then a slice
func ljustCut(s string, width int) string {
	r := []rune(s)
	for len(r) < width {
		r = append(r, ' ')
	}
	return string(r[:width])
}

// Plan and top up bar lines of the usage model, filled cells showing what remains
func (c *CLI) usageBarLines(usage *provider.UsageModel, planName string) []string {
	var lines []string
	if usage != nil && usage.PlanBar != nil && usage.PlanBar.TotalUSD > 0 {
		pb := usage.PlanBar
		filled := max(0, min(10, int(math.RoundToEven(pb.FillFraction()*10))))
		bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
		pctText := ""
		if pct, ok := pb.PctUsed(); ok {
			pctText = c.t("cli.billing.bar_pct_used", map[string]any{"pct": pct})
		}
		label := planName
		if label == "" {
			label = c.t("cli.billing.bar_plan_label", nil)
		}
		lines = append(lines, "  "+c.t("cli.billing.bar_plan", map[string]any{"label": ljustCut(label, 8), "bar": bar,
			"remaining": pyFmt("{v:,.2f}", map[string]any{"v": pb.RemainingUSD}), "total": pyFmt("{v:,.2f}", map[string]any{"v": pb.TotalUSD}), "pct": pctText}))
	}
	if usage != nil && usage.TopupBar != nil && usage.TopupBar.RemainingUSD > 0 {
		lines = append(lines, "  "+c.t("cli.billing.bar_topup", map[string]any{"label": ljustCut(c.t("cli.billing.bar_topup_label", nil), 8),
			"bar": strings.Repeat("█", 10), "remaining": pyFmt("{v:,.2f}", map[string]any{"v": usage.TopupBar.RemainingUSD})}))
	}
	return lines
}

// Prints the total spendable line when the account holds a top up balance
func (c *CLI) printTotalSpendable(usage *provider.UsageModel, printFn func(string)) {
	if usage != nil && usage.HasTopup() && usage.TotalSpendableUSD != nil {
		printFn("  " + c.t("cli.billing.total_spendable", map[string]any{"amount": pyFmt("{v:,.2f}", map[string]any{"v": *usage.TotalSpendableUSD})}))
	}
}

// Prints the Nous balance block of /usage, else the legacy credits lines, true when anything printed
func (c *CLI) printNousCreditsBlock() bool {
	if usage, ok := c.tryUsageModel(); ok && usage.Available {
		plan := usage.PlanName
		if plan == "" && usage.Status == "free" {
			plan = c.t("cli.billing.plan_free", nil)
		}
		renewsDisplay := usage.RenewsDisplay
		if renewsDisplay == "" {
			renewsDisplay, _ = provider.FormatRenews(usage.RenewsAt)
		}
		renews := ""
		if renewsDisplay != "" {
			renews = c.t("cli.billing.renews_suffix", map[string]any{"date": renewsDisplay})
		}
		var head []string
		if plan != "" {
			head = append(head, "  "+boldTTY(c.t("cli.billing.plan_line", map[string]any{"plan": plan, "renews": renews})))
		}
		head = append(head, c.usageBarLines(&usage, usage.PlanName)...)
		var tail []string
		switch usage.Status {
		case "free":
			tail = append(tail, "  "+dimTTY(c.t("cli.billing.free_models_only_hint", nil)))
		case "low":
			amount := c.t("cli.billing.under_five", nil)
			if usage.TotalSpendableUSD != nil {
				amount = "$" + pyFmt("{v:,.2f}", map[string]any{"v": *usage.TotalSpendableUSD})
			}
			tail = append(tail, "  "+c.t("cli.billing.low_balance_usage", map[string]any{"amount": amount}))
		}
		if plan != "" {
			infoPrint("")
		}
		for _, line := range head {
			cprint(line)
		}
		c.printTotalSpendable(&usage, cprint)
		for _, line := range tail {
			cprint(line)
		}
		if len(head) > 0 || len(tail) > 0 {
			return true
		}
	}
	var state *provider.CreditsState
	if c.agentRef() != nil {
		state = c.agentRef().CreditsCapture().Credits()
	}
	lines := provider.NousCreditsLines(c.ctx, false, 10*time.Second, state)
	if len(lines) == 0 {
		return false
	}
	infoPrint("")
	for _, line := range lines {
		infoPrint("  " + line)
	}
	return true
}

// Prints the /usage call to action of a Nous account
func (c *CLI) printUsageCTA() {
	c.billingDim(c.t("cli.billing.usage_cta", nil), "", false)
}
