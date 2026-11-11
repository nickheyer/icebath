package app

import (
	"errors"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const billingRule = "─────────────────────────────────────────"

const billingTimeout = 15 * time.Second

var chargeFailedKeys = map[string]string{
	"authentication_required": "cli.billing.charge_failed_authentication_required",
	"payment_method_expired":  "cli.billing.charge_failed_payment_method_expired",
	"card_declined":           "cli.billing.charge_failed_card_declined"}

var chargeErrorKeys = map[string]string{
	"no_payment_method":        "cli.billing.charge_error_no_payment_method",
	"cli_billing_disabled":     "cli.billing.charge_error_remote_spending_disabled",
	"remote_spending_disabled": "cli.billing.charge_error_remote_spending_disabled",
	"role_required":            "cli.billing.charge_error_role_required",
	"idempotency_conflict":     "cli.billing.charge_error_idempotency_conflict"}

var upgradeStatusKeys = map[string]string{
	"requires_action": "cli.subscription.upgrade_requires_action",
	"payment_failed":  "cli.subscription.upgrade_payment_failed"}

var upgradeOKKeys = map[string]string{
	"already_on_tier": "cli.subscription.upgrade_already_on_tier",
	"upgraded":        "cli.subscription.upgrade_upgraded"}

var pendingOKKeys = map[string]string{
	"schedule": "cli.subscription.pending_scheduled",
	"cancel":   "cli.subscription.pending_cancel_scheduled",
	"resume":   "cli.subscription.pending_resumed"}

// Choice row from catalog keys
func (c *CLI) billingRow(value, labelKey, descKey string) slashConfirmChoice {
	return slashConfirmChoice{Value: value, Label: c.t(labelKey, nil), Desc: c.t(descKey, nil)}
}

// Cancel style row with the default do nothing copy when keys are empty
func (c *CLI) cancelRow(value, labelKey, descKey string) slashConfirmChoice {
	if value == "" {
		value = "cancel"
	}
	if labelKey == "" {
		labelKey = "cli.billing.choice_cancel"
	}
	if descKey == "" {
		descKey = "cli.billing.desc_do_nothing"
	}
	return c.billingRow(value, labelKey, descKey)
}

// Manage on portal row
func (c *CLI) portalRow() slashConfirmChoice {
	return c.billingRow("portal", "cli.billing.choice_manage_on_portal", "cli.billing.desc_open_billing_page")
}

// Runs the choice modal and returns the normalized value, empty when dismissed
func (c *CLI) modalChoice(title, detail string, choices []slashConfirmChoice) string {
	raw, ok := c.promptTextInputModal(title, detail, choices, 120)
	if !ok {
		return ""
	}
	return c.normalizeSlashConfirmChoice(raw, choices)
}

// Blank line, the icon and bold title, then the rule when asked
func (c *CLI) blockHeader(icon, title string, rule bool) {
	infoPrint("")
	cprint("  " + icon + " " + boldTTY(title))
	if rule {
		infoPrint("  " + billingRule)
	}
}

// Dim check mark success line
func (c *CLI) billingOK(msg string) { cprint("  " + dimANSI + "✓ " + msg + resetANSI) }

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

// Pool env the billing calls authenticate through
func (c *CLI) billingEnv() provider.PoolEnv {
	env := provider.HomePoolEnv(home.Dir(c.ctx))
	env.Ctx = c.ctx
	return env
}

// The one time remote spending step up, true only when granted, refusals printed
func (c *CLI) stepUpRemoteSpending(explain, noninteractiveMsg, declinedMsg, notGrantedMsg string) bool {
	infoPrint("")
	infoPrint("  " + c.t("cli.billing.one_time_setup", nil))
	c.billingDim(explain, "", false)
	if c.app == nil {
		infoPrint(noninteractiveMsg)
		return false
	}
	choice := c.modalChoice(c.t("cli.billing.allow_remote_spending_title", nil), c.t("cli.billing.allow_remote_spending_detail", nil),
		[]slashConfirmChoice{
			c.billingRow("yes", "cli.billing.choice_allow_remote_spending", "cli.billing.desc_allow_remote_spending"),
			c.billingRow("no", "cli.billing.choice_not_now", "cli.billing.desc_not_now")})
	if choice != "yes" {
		infoPrint(declinedMsg)
		return false
	}
	infoPrint("  " + c.t("cli.billing.opening_browser_remote_spending", nil))
	granted, err := func() (granted bool, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = errors.New(sideFailureText(r))
			}
		}()
		return provider.StepUpNousBillingScope(c.billingEnv(), console().Out, true, 15*time.Second, nil)
	}()
	if err != nil {
		infoPrint("  " + c.t("cli.billing.couldnt_allow_remote_spending", map[string]any{"error": pyrepr.ExceptionMessage(err)}))
		return false
	}
	if !granted {
		infoPrint(notGrantedMsg)
	}
	return granted
}

// Portal deep link line of a billing error that carries one
func (c *CLI) printPortalLine(be *provider.BillingError) {
	if be != nil && pyrepr.Truthy(be.PortalURL) {
		cprint("  " + c.t("cli.billing.portal_line", map[string]any{"url": be.PortalURL}))
	}
}

// Opens a portal URL in a graphical browser, false over remote sessions and text only browsers
func (c *CLI) openURLInBrowser(url string) bool {
	if url == "" {
		return false
	}
	if provider.IsRemoteSession() || !provider.CanOpenGraphicalBrowser() {
		return false
	}
	return provider.OpenBrowserURL(url) == nil
}

// Opens the URL or prints it when no graphical browser can be used
func (c *CLI) openOrPrintURL(url string) {
	if !c.openURLInBrowser(url) {
		infoPrint("  " + c.t("cli.billing.open_this_url", map[string]any{"url": url}))
	}
}

// Billing error of err, nil for any other failure
func asBillingError(err error) *provider.BillingError {
	var be *provider.BillingError
	if errors.As(err, &be) {
		return be
	}
	return nil
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

// Whether a decimal is a finite number
func decimalFinite(d *provider.PyDecimal) bool {
	if d == nil || d.IsNaN() {
		return false
	}
	s := d.String()
	return s != "Infinity" && s != "-Infinity"
}

// Float value of a decimal as float(Decimal) gives
func decimalFloat(d *provider.PyDecimal) float64 {
	f, err := strconv.ParseFloat(d.String(), 64)
	if err != nil {
		panic(err)
	}
	return f
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
	case c.app == nil:
		infoPrint("")
		if manageURL != "" {
			infoPrint("  " + c.t("cli.subscription.manage_url_line", map[string]any{"url": manageURL}))
			infoPrint("  " + c.t("cli.subscription.open_then_rerun", nil))
		}
	case isFree:
		c.subscriptionFreeCatalog(state, manageURL)
	default:
		c.subscriptionChangeMenu(state, manageURL)
	}
}

// Free plan catalog whose pick deep links the portal with the chosen plan
func (c *CLI) subscriptionFreeCatalog(state *provider.SubscriptionState, manageURL string) {
	tiers := provider.SelectableTiers(state)
	if len(tiers) == 0 {
		c.subscriptionOpenPortal(manageURL, c.t("cli.subscription.start_subscription", nil))
		return
	}
	c.blockHeader("☤", c.t("cli.subscription.choose_plan", nil), true)
	rows := make([]string, len(tiers))
	for i, tier := range tiers {
		rows[i] = must(provider.FormatTierRow(tier))
		infoPrint("  " + strconv.Itoa(i+1) + ". " + rows[i])
	}
	c.billingDim(c.t("cli.subscription.start_opens_portal", nil), "", false)
	var choices []slashConfirmChoice
	for i, tier := range tiers {
		choices = append(choices, slashConfirmChoice{Value: optText(tier.TierID), Label: rows[i],
			Desc: c.t("cli.subscription.desc_start_on_portal", map[string]any{"name": tier.Name})})
	}
	choices = append(choices, c.cancelRow("", "", ""))
	raw, answered := c.promptTextInputModal(c.t("cli.subscription.start_subscription", nil), c.t("cli.subscription.pick_plan_detail", nil), choices, 120)
	digit := pystr.Strip(raw)
	choice := ""
	if n, err := strconv.Atoi(digit); err == nil && isASCIIDigits(digit) && n >= 1 && n <= len(tiers) {
		choice = optText(tiers[n-1].TierID)
	}
	if choice == "" && answered {
		choice = c.normalizeSlashConfirmChoice(raw, choices)
	}
	if choice == "" || choice == "cancel" {
		infoPrint("  " + c.t("cli.subscription.cancelled_no_plan_started", nil))
		return
	}
	tierURL, ok := provider.SubscriptionManageURL(state, choice)
	if !ok || tierURL == "" {
		tierURL = manageURL
	}
	if tierURL == "" {
		c.billingDim(c.t("cli.billing.no_manage_url", nil), "", false)
		return
	}
	var label any = c.t("cli.subscription.your_plan", nil)
	for _, tier := range tiers {
		if optText(tier.TierID) == choice {
			label = tier.Name
			break
		}
	}
	if c.openURLInBrowser(tierURL) {
		infoPrint("  " + c.t("cli.subscription.opening_portal_to_start", map[string]any{"plan": label}))
	} else {
		infoPrint("  " + c.t("cli.subscription.open_url_to_start", map[string]any{"plan": label, "url": tierURL}))
	}
	infoPrint("  " + c.t("cli.subscription.finish_in_browser", nil))
}

// Whether s is a non empty run of ASCII digits
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Opens or copies the manage subscription URL, the portal hand off
func (c *CLI) subscriptionOpenPortal(manageURL, verb string) {
	if verb == "" {
		verb = c.t("cli.subscription.manage_your_subscription", nil)
	}
	infoPrint("")
	if manageURL == "" {
		c.billingDim(c.t("cli.billing.no_manage_url", nil), "", false)
		return
	}
	choices := []slashConfirmChoice{
		{Value: "open", Label: verb, Desc: c.t("cli.subscription.desc_open_subscription_page", nil)},
		c.billingRow("copy", "cli.subscription.choice_copy_link", "cli.subscription.desc_copy_link"),
		c.cancelRow("", "", "")}
	switch c.modalChoice(verb, "", choices) {
	case "open":
		c.openOrPrintURL(manageURL)
		infoPrint("")
		infoPrint("  " + c.t("cli.subscription.finish_in_browser", nil))
	case "copy":
		copied := false
		probeCall(func() {
			c.writeOSC52Clipboard(manageURL)
			copied = true
		})
		if copied {
			infoPrint("  " + c.t("cli.subscription.copied", map[string]any{"url": manageURL}))
		} else {
			infoPrint("  " + c.t("cli.subscription.manage_url", map[string]any{"url": manageURL}))
		}
	default:
		infoPrint("  " + c.t("cli.billing.cancelled_yellow", nil))
	}
}

// In terminal change menu for a paid admin
func (c *CLI) subscriptionChangeMenu(state *provider.SubscriptionState, manageURL string) {
	cur := state.Current
	changeRow := c.billingRow("change", "cli.subscription.choice_change_plan", "cli.subscription.desc_change_plan")
	var head []slashConfirmChoice
	if cur != nil && (cur.CancelAtPeriodEnd || pyrepr.Truthy(cur.PendingDowngradeTierName)) {
		keepName := pyrepr.Or(cur.TierName, c.t("cli.subscription.your_plan", nil))
		head = []slashConfirmChoice{{Value: "keep", Label: c.t("cli.subscription.choice_keep_plan", map[string]any{"plan": keepName}),
			Desc: c.t("cli.subscription.desc_keep_plan", nil)}, changeRow}
	} else {
		head = []slashConfirmChoice{changeRow, c.billingRow("cancel_sub", "cli.subscription.choice_cancel_subscription", "cli.subscription.desc_cancel_subscription")}
	}
	choices := append(head, c.portalRow(), c.cancelRow("close", "cli.billing.choice_close", ""))
	switch c.modalChoice(c.t("cli.subscription.manage_your_subscription", nil), "", choices) {
	case "change":
		c.subscriptionPickTier(state)
	case "keep":
		c.subscriptionApply(state, "resume", "", "", true)
	case "cancel_sub":
		c.subscriptionConfirmCancel(state)
	case "portal":
		c.subscriptionOpenPortal(manageURL, "")
	default:
		infoPrint("  " + c.t("cli.subscription.closed_no_change", nil))
	}
}

// Tier picker leading to the preview and confirm
func (c *CLI) subscriptionPickTier(state *provider.SubscriptionState) {
	cur := state.Current
	selectable := provider.SelectableTiers(state)
	if len(selectable) == 0 {
		infoPrint("  " + c.t("cli.subscription.no_other_plans", nil))
		return
	}
	var choices []slashConfirmChoice
	for _, tier := range selectable {
		direction := c.t("cli.subscription.direction_downgrade", nil)
		if must(provider.IsUpgrade(state, optText(tier.TierID))) {
			direction = c.t("cli.subscription.direction_upgrade", nil)
		}
		choices = append(choices, slashConfirmChoice{Value: optText(tier.TierID),
			Label: c.t("cli.subscription.tier_row", map[string]any{"row": must(provider.FormatTierRow(tier)), "direction": direction}),
			Desc:  c.t("cli.subscription.desc_switch_to", map[string]any{"name": tier.Name})})
	}
	choices = append(choices, c.cancelRow("", "cli.billing.choice_back", ""))
	var plan any = c.t("cli.billing.plan_free", nil)
	if cur != nil {
		plan = cur.TierName
	}
	choice := c.modalChoice(c.t("cli.subscription.change_plan_title", nil), c.t("cli.subscription.current_pick_detail", map[string]any{"plan": plan}), choices)
	if choice == "" || choice == "cancel" {
		infoPrint("  " + c.t("cli.subscription.cancelled_no_plan_change", nil))
		return
	}
	c.subscriptionPreviewAndConfirm(state, choice, true)
}

// Previews a plan change, states its effect and applies it on confirm
func (c *CLI) subscriptionPreviewAndConfirm(state *provider.SubscriptionState, tierID string, allowStepup bool) {
	c.billingDim(c.t("cli.subscription.checking_change", nil), "", false)
	payload, err := provider.PostNousSubscriptionPreview(c.billingEnv(), tierID, billingTimeout)
	if err != nil {
		be := asBillingError(err)
		if be == nil {
			panic(err)
		}
		if be.Kind == provider.BillingErrorScopeRequired {
			if allowStepup {
				c.subscriptionHandleScopeRequired(state, "preview", tierID, "")
			} else {
				infoPrint("  " + c.t("cli.billing.stepup_stale", nil))
			}
			return
		}
		c.subscriptionRenderError(be)
		return
	}
	payloadMap, _ := payload.(map[string]any)
	p := must(provider.SubscriptionChangePreviewFromPayload(payloadMap))
	target := pyrepr.Or(p.TargetTierName, c.t("cli.subscription.the_selected_plan", nil))
	infoPrint("")
	if p.Effect == "no_op" {
		c.billingDim(c.t("cli.subscription.already_on_nothing_to_change", map[string]any{"plan": target}), "", false)
		return
	}
	if p.Effect != "charge_now" && p.Effect != "scheduled" {
		cprint("  \U0001f7e1 " + pyrepr.Str(pyrepr.Or(p.Reason, c.t("cli.subscription.cannot_confirm_here", nil))))
		plan := ""
		if must(provider.IsUpgrade(state, tierID)) {
			plan = tierID
		}
		if mu, ok := provider.SubscriptionManageURL(state, plan); ok && mu != "" {
			infoPrint("  " + c.t("cli.billing.manage_on_portal_url", map[string]any{"url": mu}))
		}
		return
	}
	tag := c.t("cli.subscription.tag_scheduled_not_today", nil)
	if p.Effect == "charge_now" {
		tag = c.t("cli.subscription.tag_charged_now", nil)
	}
	cprint("  " + boldTTY(c.t("cli.subscription.confirm_plan_change", nil)) + "  " + dimTTY(tag))
	var payLabel, kind string
	var confirmChoices []slashConfirmChoice
	if p.Effect == "charge_now" {
		amt := ""
		if p.AmountDueNowCents != nil {
			amt = "$" + strconv.FormatFloat(float64(*p.AmountDueNowCents)/100, 'f', 2, 64)
		}
		charged := c.t("cli.subscription.charged_prorated_now", nil)
		if amt != "" {
			charged = c.t("cli.subscription.charged_amount_now", map[string]any{"amount": amt})
		}
		cprint("  " + c.t("cli.subscription.upgrade_will_be_charged", map[string]any{"plan": target, "charged": charged}))
		cardLine := c.t("cli.subscription.card_on_subscription_charged", nil)
		probeCall(func() {
			bs, err := provider.BuildBillingState(c.ctx, 6*time.Second)
			if err != nil || !bs.LoggedIn || bs.Card == nil || bs.Card.ResolvedVia == nil {
				return
			}
			if via := *bs.Card.ResolvedVia; via == "subPin" || via == "customerDefault" {
				cardLine = c.t("cli.subscription.named_card_on_subscription_charged", map[string]any{"card": bs.Card.Masked()})
			}
		})
		c.billingDim(cardLine, "", false)
		payLabel = c.t("cli.subscription.choice_upgrade_now_prorated", nil)
		if amt != "" {
			payLabel = c.t("cli.subscription.choice_pay_and_upgrade", map[string]any{"amount": amt})
		}
		kind = "upgrade"
		confirmChoices = []slashConfirmChoice{
			c.cancelRow("", "cli.billing.choice_go_back", "cli.billing.desc_do_not_charge"),
			{Value: "yes", Label: payLabel, Desc: c.t("cli.subscription.desc_charge_and_upgrade", nil)}}
	} else {
		when := any(c.t("cli.subscription.end_of_billing_period", nil))
		if s, ok := p.EffectiveAt.(string); ok && s != "" && len([]rune(s)) >= 10 {
			when = string([]rune(s)[:10])
		}
		cprint("  " + c.t("cli.subscription.change_takes_effect", map[string]any{"plan": target, "when": when}))
		payLabel = c.t("cli.subscription.choice_schedule_change", map[string]any{"plan": target})
		kind = "schedule"
		confirmChoices = []slashConfirmChoice{
			{Value: "yes", Label: payLabel, Desc: c.t("cli.subscription.desc_apply_change", nil)},
			c.cancelRow("", "cli.billing.choice_go_back", "cli.subscription.desc_do_not_change")}
	}
	if p.MonthlyCreditsDelta != nil && !decimalIsZero(p.MonthlyCreditsDelta) {
		c.billingDim(c.t("cli.subscription.monthly_credits_change", map[string]any{"delta": p.MonthlyCreditsDelta.String()}), "", false)
	}
	if c.modalChoice(payLabel, "", confirmChoices) != "yes" {
		infoPrint("  " + c.t("cli.subscription.cancelled_no_plan_change", nil))
		return
	}
	c.subscriptionApply(state, kind, tierID, "", allowStepup)
}

// Whether a decimal is zero, the falsy Decimal
func decimalIsZero(d *provider.PyDecimal) bool {
	eq, err := d.Equal(provider.DecimalFromInt(0))
	return err == nil && eq
}

// Confirms then schedules a cancellation at period end
func (c *CLI) subscriptionConfirmCancel(state *provider.SubscriptionState) {
	cur := state.Current
	end := ""
	if cur != nil && pyrepr.Truthy(cur.CycleEndsAt) {
		end = fmtRenews(cur.CycleEndsAt)
	}
	if end == "" {
		end = c.t("cli.subscription.end_of_billing_period", nil)
	}
	infoPrint("")
	cprint("  " + boldTTY(c.t("cli.subscription.confirm_cancellation", nil)) + "  " + dimTTY(c.t("cli.subscription.tag_scheduled_not_today", nil)))
	var plan any = c.t("cli.subscription.your_plan", nil)
	if cur != nil {
		plan = cur.TierName
	}
	cprint("  " + c.t("cli.subscription.cancel_stays_active_until", map[string]any{"plan": plan, "end": end}))
	c.billingDim(c.t("cli.subscription.keep_remaining_credits", nil), "", false)
	choices := []slashConfirmChoice{
		c.billingRow("yes", "cli.subscription.choice_cancel_subscription", "cli.subscription.desc_cancel_subscription"),
		c.billingRow("cancel", "cli.billing.choice_go_back", "cli.subscription.desc_keep_your_plan")}
	if c.modalChoice(c.t("cli.subscription.cancel_subscription_question", nil), "", choices) != "yes" {
		infoPrint("  " + c.t("cli.subscription.cancelled_plan_unchanged", nil))
		return
	}
	c.subscriptionApply(state, "cancel", "", "", true)
}

// Runs an upgrade, schedule, cancel or resume, a scope denial stepping up and replaying once with the same key
func (c *CLI) subscriptionApply(state *provider.SubscriptionState, kind, arg, idempotencyKey string, allowStepup bool) {
	key := ""
	if kind == "upgrade" {
		key = idempotencyKey
		if key == "" {
			key = provider.NewIdempotencyKey()
		}
	}
	env := c.billingEnv()
	var err error
	if kind == "upgrade" {
		var res any
		if res, err = provider.PostNousSubscriptionUpgrade(env, arg, key, billingTimeout); err == nil {
			m, _ := res.(map[string]any)
			status, _ := m["status"].(string)
			name := pyrepr.Or(m["targetTierName"], c.t("cli.subscription.your_new_plan", nil))
			if okKey, ok := upgradeOKKeys[status]; ok {
				c.billingOK(c.t(okKey, map[string]any{"name": name}))
			} else if lineKey, ok := upgradeStatusKeys[status]; ok {
				cprint("  " + c.t(lineKey, nil))
				if pyrepr.Truthy(m["recoveryUrl"]) {
					cprint("  " + c.t("cli.billing.portal_line", map[string]any{"url": m["recoveryUrl"]}))
				}
			} else {
				c.subscriptionRenderUpgradeAmbiguous(nil)
			}
			return
		}
	} else {
		switch kind {
		case "schedule":
			_, err = provider.PutNousSubscriptionPendingChange(env, arg, false, billingTimeout)
		case "cancel":
			_, err = provider.PutNousSubscriptionPendingChange(env, nil, true, billingTimeout)
		case "resume":
			_, err = provider.DeleteNousSubscriptionPendingChange(env, billingTimeout)
		}
		if err == nil {
			if okKey, ok := pendingOKKeys[kind]; ok {
				c.billingOK(c.t(okKey, nil))
			}
			c.billingDim(c.t("cli.subscription.rerun_to_review", nil), "", false)
			return
		}
	}
	be := asBillingError(err)
	if be == nil {
		panic(err)
	}
	if be.Kind == provider.BillingErrorScopeRequired {
		if allowStepup {
			c.subscriptionHandleScopeRequired(state, kind, arg, key)
		} else {
			infoPrint("  " + c.t("cli.billing.stepup_stale", nil))
		}
		return
	}
	preCharge := be.Kind.IsTransient() || be.Kind == provider.BillingErrorSessionRevoked || be.Kind == provider.BillingErrorRemoteSpendingRevoked
	code := optText(be.ErrorCode)
	ambiguous := code == "network_error" || code == "endpoint_unavailable" || be.Status == 0 || be.Status >= 500
	if kind == "upgrade" && ambiguous && !preCharge {
		c.subscriptionRenderUpgradeAmbiguous(be)
	} else {
		c.subscriptionRenderError(be)
	}
}

// Steps up the token scope then replays the held preview or action once
func (c *CLI) subscriptionHandleScopeRequired(state *provider.SubscriptionState, kind, arg, idempotencyKey string) {
	granted := c.stepUpRemoteSpending(c.t("cli.subscription.stepup_explain", nil),
		"  "+c.t("cli.subscription.stepup_noninteractive", nil), "  "+c.t("cli.subscription.stepup_declined", nil),
		"  "+c.t("cli.subscription.stepup_not_granted", nil))
	if !granted {
		return
	}
	c.billingOK(c.t("cli.billing.remote_spending_allowed", nil))
	probeCall(provider.InvalidateNousBillingCachedToken)
	fresh := state
	probeCall(func() {
		if s, err := provider.BuildSubscriptionState(c.ctx, billingTimeout); err == nil {
			fresh = s
		}
	})
	if kind == "preview" {
		c.subscriptionPreviewAndConfirm(fresh, arg, false)
	} else {
		c.subscriptionApply(fresh, kind, arg, idempotencyKey, false)
	}
}

// Renders a subscription billing error with its portal line
func (c *CLI) subscriptionRenderError(be *provider.BillingError) {
	msg := be.Error()
	if msg == "" {
		msg = c.t("cli.billing.something_went_wrong", nil)
	}
	switch optText(be.ErrorCode) {
	case "insufficient_scope":
		cprint("  " + c.t("cli.subscription.remote_spending_not_allowed_yet", nil))
	case "subscription_mutation_rejected", "preview_rejected":
		cprint("  \U0001f7e1 " + msg)
	default:
		cprint("  \U0001f534 " + msg)
	}
	c.printPortalLine(be)
}

// Ambiguous upgrade outcome steering to a re check rather than a blind retry
func (c *CLI) subscriptionRenderUpgradeAmbiguous(be *provider.BillingError) {
	cprint("  " + c.t("cli.subscription.upgrade_ambiguous", nil))
	c.billingDim(c.t("cli.subscription.rerun_to_check_plan", nil), "", false)
	c.printPortalLine(be)
}
