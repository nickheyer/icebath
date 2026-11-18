package app

import (
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Handles /topup, the remote spending overview and its screens, never collecting card details
func (c *CLI) showBilling(command string) {
	state := must(provider.BuildBillingState(c.ctx, billingTimeout))
	if !state.LoggedIn {
		c.printLoggedOut(state.Error, c.t("cli.billing.load_failed_label", nil), "/topup")
		return
	}
	c.billingOverview(state)
}

// Portal deep link line with an optional reason first, silent without a portal URL
func (c *CLI) billingPortalHint(state *provider.BillingState, reason string) {
	if !pyrepr.Truthy(state.PortalURL) {
		return
	}
	if reason != "" {
		infoPrint("  " + reason)
	}
	infoPrint("  " + c.t("cli.billing.manage_on_portal_url", map[string]any{"url": state.PortalURL}))
}

// Admin and org kill switch gate, printing the portal funnel and returning false when blocked
func (c *CLI) billingRequireAdmin(state *provider.BillingState, icon, offReasonKey string) bool {
	canChange := must(state.CanChangePlan())
	if canChange && state.CLIBillingEnabled {
		return true
	}
	if icon != "" {
		infoPrint("")
	}
	if !canChange {
		c.billingDim(c.t("cli.billing.actions_require_admin", nil), icon, false)
		c.billingPortalHint(state, "")
	} else {
		c.billingDim(c.t("cli.billing.remote_spending_off_for_org", nil), icon, false)
		c.billingPortalHint(state, c.t(offReasonKey, nil))
	}
	return false
}

// Screen one, the balance, bars, auto reload and card lines then the action menu
func (c *CLI) billingOverview(state *provider.BillingState) {
	usage, haveUsage := c.tryUsageModel()
	var usageRef *provider.UsageModel
	planName := ""
	if haveUsage {
		usageRef = &usage
		planName = usage.PlanName
	}
	infoPrint("")
	cprint("  \U0001f4b3 " + boldTTY(c.t("cli.billing.topup_header", map[string]any{"balance": fmtMoney(state.BalanceUSD)})))
	c.printOrgLine(state.OrgName, state.Role)
	infoPrint("  " + billingRule)
	for _, line := range c.usageBarLines(usageRef, planName) {
		infoPrint(line)
	}
	ar := state.AutoReload
	if ar != nil {
		if ar.Enabled {
			infoPrint("  " + c.t("cli.billing.auto_reload_on_line", map[string]any{"threshold": fmtMoney(ar.ThresholdUSD), "reload_to": fmtMoney(ar.ReloadToUSD)}))
		} else {
			infoPrint("  " + c.t("cli.billing.auto_reload_off_line", nil))
		}
	}
	if state.KeepGoingOnCredits != nil {
		key := "cli.billing.keep_going_off_line"
		if *state.KeepGoingOnCredits {
			key = "cli.billing.keep_going_on_line"
		}
		infoPrint("  " + c.t(key, nil))
	}
	if must(state.CanChangePlan()) && state.CLIBillingEnabled {
		if state.Card != nil {
			infoPrint("  " + c.t("cli.billing.card_line", map[string]any{"card": state.Card.Display()}))
		} else {
			c.billingDim(c.t("cli.billing.no_saved_card_add_funds_hint", nil), "", false)
		}
	}
	infoPrint("  " + billingRule)
	if !c.billingRequireAdmin(state, "", "cli.billing.killswitch_reason_overview") {
		return
	}
	c.billingPortalHint(state, "")
}
