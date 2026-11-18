package app

import (
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const billingRule = "─────────────────────────────────────────"

const billingTimeout = 15 * time.Second

// Blank line, the icon and bold title, then the rule when asked
func (c *CLI) blockHeader(icon, title string, rule bool) {
	infoPrint("")
	cprint("  " + icon + " " + boldTTY(title))
	if rule {
		infoPrint("  " + billingRule)
	}
}

// Logged out or fetch failed block shared by /subscription and /topup
func (c *CLI) printLoggedOut(errText *string, loadFailed, cmd string) {
	if errText != nil && *errText != "" {
		c.billingDim(c.t("cli.billing.load_failed", map[string]any{"label": loadFailed, "error": *errText}), "\U0001f4b3 ", true)
		return
	}
	c.billingDim(c.t("cli.billing.not_logged_in", nil), "\U0001f4b3 ", true)
	infoPrint("  " + c.t("cli.billing.run_portal_then", map[string]any{"cmd": cmd}))
}

// Dim org line with the titled role, skipped without an org
func (c *CLI) printOrgLine(orgName, role any) {
	if !pyrepr.Truthy(orgName) {
		return
	}
	roleText := ""
	if pyrepr.Truthy(role) {
		roleText = pystr.Title(pyrepr.Str(role))
	}
	if roleText != "" {
		c.billingDim(c.t("cli.billing.org_line_role", map[string]any{"org": orgName, "role": roleText}), "", false)
	} else {
		c.billingDim(c.t("cli.billing.org_line", map[string]any{"org": orgName}), "", false)
	}
}

// Text of a value as Python str, empty for None
func optText(v any) string {
	if v == nil {
		return ""
	}
	return pyrepr.Str(v)
}

// Display date of an ISO value, empty when the value is falsy
func fmtRenews(v any) string {
	if !pyrepr.Truthy(v) {
		return ""
	}
	out, _ := provider.FormatRenews(pyrepr.Str(v))
	return out
}

// Money text of a decimal, the dash for none
func fmtMoney(v *provider.PyDecimal) string {
	out, err := provider.FormatMoney(v, false)
	if err != nil {
		panic(err)
	}
	return out
}

// Panics on a Python raised error so the slash dispatch surfaces it
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Handles /subscription, the plan view and change menu deep linking the portal, never charging
func (c *CLI) showSubscription() {
	state := must(provider.BuildSubscriptionState(c.ctx, billingTimeout))
	if !state.LoggedIn {
		c.printLoggedOut(state.Error, c.t("cli.subscription.load_failed_label", nil), "/subscription")
		return
	}
	if state.Context == "team" {
		c.blockHeader("☤", c.t("cli.subscription.team_header", nil), true)
		c.printOrgLine(state.OrgName, state.Role)
		org := state.OrgName
		if !pyrepr.Truthy(org) {
			org = c.t("cli.subscription.a_team_org", nil)
		}
		infoPrint("  " + c.t("cli.subscription.team_connected", map[string]any{"org": org}))
		c.billingDim(c.t("cli.subscription.personal_note", nil), "", false)
		return
	}
	manageURL, _ := provider.SubscriptionManageURL(state, "")
	c.subscriptionOverview(state, manageURL)
}

// Plan block then the portal hand off, the catalog for free plans or the change menu
func (c *CLI) subscriptionOverview(state *provider.SubscriptionState, manageURL string) {
	usage, haveUsage := c.tryUsageModel()
	var usageRef *provider.UsageModel
	if haveUsage {
		usageRef = &usage
	}
	cur := state.Current
	isFree := cur == nil || !pyrepr.Truthy(cur.TierID)
	canChange := must(state.CanChangePlan())
	var planName any
	if cur != nil {
		planName = pyrepr.Or(cur.TierName, cur.TierID)
	} else if usageRef != nil {
		planName = usageRef.PlanName
	}
	uStatus := ""
	var spend *float64
	renewsDisplay := ""
	if usageRef != nil {
		uStatus, spend, renewsDisplay = usageRef.Status, usageRef.TotalSpendableUSD, usageRef.RenewsDisplay
	}
	if renewsDisplay == "" && cur != nil && pyrepr.Truthy(cur.CycleEndsAt) {
		renewsDisplay = fmtRenews(cur.CycleEndsAt)
	}
	flip := ""
	var trans []any
	yourPlan := c.t("cli.subscription.your_plan", nil)
	if cur != nil && cur.CancelAtPeriodEnd {
		flip = c.t("cli.subscription.status_flip_cancels", nil)
		when := fmtRenews(cur.CancellationEffectiveAt)
		if when == "" {
			when = c.t("cli.subscription.end_of_billing_period", nil)
		}
		trans = []any{pyrepr.Or(cur.TierName, yourPlan), c.t("cli.subscription.cancels", nil), when}
	} else if cur != nil && pyrepr.Truthy(cur.PendingDowngradeTierName) {
		flip = c.t("cli.subscription.status_flip_to", map[string]any{"tier": cur.PendingDowngradeTierName})
		when := fmtRenews(cur.PendingDowngradeAt)
		if when == "" {
			when = c.t("cli.subscription.end_of_cycle", nil)
		}
		trans = []any{pyrepr.Or(cur.TierName, yourPlan), cur.PendingDowngradeTierName, when}
	}
	left := ""
	if spend != nil {
		left = c.t("cli.subscription.status_left", map[string]any{"amount": pyFmt("{v:,.2f}", map[string]any{"v": *spend})})
	}
	tail := ""
	switch {
	case uStatus == "low" && spend != nil:
	case !canChange:
		tail = c.t("cli.subscription.status_view_only", nil)
	case renewsDisplay != "":
		tail = c.t("cli.subscription.status_renews", map[string]any{"date": renewsDisplay})
	}
	status := c.t("cli.subscription.status_free", nil)
	if pyrepr.Truthy(planName) {
		status = c.t("cli.subscription.status_line", map[string]any{"plan": planName, "flip": flip, "left": left, "tail": tail})
	}
	cprint("")
	if trans != nil {
		cprint("  ⏳ " + boldTTY(c.t("cli.subscription.scheduled_change", nil)))
		cprint("  " + c.t("cli.subscription.scheduled_change_line", map[string]any{"from_plan": trans[0], "to_plan": trans[1],
			"when": dimTTY(c.t("cli.subscription.scheduled_when", map[string]any{"when": trans[2]}))}))
		c.billingDim(c.t("cli.subscription.keep_until_then", map[string]any{"plan": trans[0]}), "", false)
		cprint("")
	}
	cprint("  ☤ " + boldTTY(status))
	infoPrint("  " + billingRule)
	for _, line := range c.usageBarLines(usageRef, optText(planName)) {
		infoPrint(line)
	}
	c.printTotalSpendable(usageRef, infoPrint)
	if isFree {
		c.billingDim(c.t("cli.subscription.paid_models_need_subscription", nil), "", false)
	} else if uStatus == "low" {
		amt := c.t("cli.billing.under_five", nil)
		if spend != nil {
			amt = "$" + pyFmt("{v:,.2f}", map[string]any{"v": *spend})
		}
		cprint("  " + c.t("cli.subscription.low_balance", map[string]any{"amount": amt}))
	}
	c.printOrgLine(state.OrgName, state.Role)
	infoPrint("  " + billingRule)
	switch {
	case !canChange:
		c.billingDim(c.t("cli.subscription.plan_changes_need_admin", nil), "", true)
		if manageURL != "" {
			infoPrint("  " + c.t("cli.billing.manage_on_portal_url", map[string]any{"url": manageURL}))
		}
	default:
		infoPrint("")
		if manageURL != "" {
			infoPrint("  " + c.t("cli.subscription.manage_url_line", map[string]any{"url": manageURL}))
			infoPrint("  " + c.t("cli.subscription.open_then_rerun", nil))
		}
	}
}
