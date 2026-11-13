package app

import (
	"math"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
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
		c.billingOfferKeepGoingOff(state)
		return
	}
	if c.app == nil {
		c.billingPortalHint(state, "")
		return
	}
	c.billingDim(c.t("cli.billing.add_funds_now_hint", nil), "", false)
	amounts := []*provider.PyDecimal{nil}
	if ar != nil && ar.Enabled {
		amounts = []*provider.PyDecimal{ar.ReloadToUSD, ar.ThresholdUSD}
	}
	allFinite := true
	for _, a := range amounts {
		allFinite = allFinite && decimalFinite(a)
	}
	autoLine := c.t("cli.billing.refill_when_low_generic", nil)
	if allFinite {
		autoLine = c.t("cli.billing.refill_when_low_amounts", map[string]any{"reload_to": fmtMoney(ar.ReloadToUSD), "threshold": fmtMoney(ar.ThresholdUSD)})
	}
	c.billingDim(autoLine, "", false)
	infoPrint("  " + billingRule)
	choices := c.topupMenuChoices()
	if state.KeepGoingOnCredits != nil {
		choices = append(choices[:2], append([]slashConfirmChoice{c.keepGoingRow()}, choices[2:]...)...)
	}
	switch c.modalChoice(c.t("cli.billing.topup_title", nil), "", choices) {
	case "buy":
		c.billingBuyFlow(state)
	case "auto":
		c.billingAutoReloadFlow(state)
	case "hermes":
		c.billingKeepGoingOnCreditsFlow(state)
	case "limit":
		c.billingLimitScreen(state)
	case "portal":
		c.billingOpenPortal(state)
	default:
		infoPrint("  " + c.t("cli.billing.cancelled", nil))
	}
}

// Opens the billing portal or prints its URL
func (c *CLI) billingOpenPortal(state *provider.BillingState) {
	if !pyrepr.Truthy(state.PortalURL) {
		infoPrint("  " + c.t("cli.billing.no_portal_url", nil))
		return
	}
	c.openOrPrintURL(pyrepr.Str(state.PortalURL))
	infoPrint("  " + c.t("cli.billing.complete_in_browser", nil))
}

// Guided add card loop on the portal with bounded re checks, the refreshed state or nil
func (c *CLI) billingAddCardFlow(state *provider.BillingState) *provider.BillingState {
	c.blockHeader("\U0001f4b3", c.t("cli.billing.add_card_first", nil), false)
	cprint("  " + c.t("cli.billing.no_saved_card", nil))
	c.billingDim(c.t("cli.billing.add_card_once_hint", nil), "", false)
	choices := c.addCardChoices()
loop:
	for i := 0; i < 8; i++ {
		switch c.modalChoice(c.t("cli.billing.add_card_title", nil), "", choices) {
		case "portal":
			c.billingOpenPortal(state)
			c.billingDim(c.t("cli.billing.add_card_then_recheck", nil), "", false)
		case "recheck":
			var fresh *provider.BillingState
			probeCall(func() {
				if s, err := provider.BuildBillingState(c.ctx, billingTimeout); err == nil {
					fresh = s
				}
			})
			if fresh != nil && fresh.LoggedIn {
				state = fresh
			}
			if state.Card != nil {
				c.billingOK(c.t("cli.billing.card_found_continuing", map[string]any{"card": state.Card.Display()}))
				return state
			}
			infoPrint("  " + c.t("cli.billing.still_no_card", nil))
		default:
			break loop
		}
	}
	infoPrint("  " + c.t("cli.billing.cancelled_no_funds", nil))
	return nil
}

// Screen two, the preset or custom amount pick leading to the confirm and charge
func (c *CLI) billingBuyFlow(state *provider.BillingState) {
	if !c.billingRequireAdmin(state, "\U0001f4b3 ", "cli.billing.killswitch_reason_buy") {
		return
	}
	if c.app == nil {
		c.blockHeader("\U0001f4b3", c.t("cli.billing.add_funds", nil), false)
		presets := make([]string, len(state.ChargePresets))
		for i := range state.ChargePresets {
			presets[i] = fmtMoney(&state.ChargePresets[i])
		}
		infoPrint("  " + c.t("cli.billing.presets", map[string]any{"presets": strings.Join(presets, ", ")}))
		infoPrint("  " + c.t("cli.billing.run_interactive_to_purchase", nil))
		c.billingPortalHint(state, "")
		return
	}
	if state.Card == nil {
		if state = c.billingAddCardFlow(state); state == nil || state.Card == nil {
			return
		}
	}
	var presetChoices []slashConfirmChoice
	for i := range state.ChargePresets {
		p := &state.ChargePresets[i]
		presetChoices = append(presetChoices, slashConfirmChoice{Value: p.String(), Label: fmtMoney(p), Desc: c.t("cli.billing.desc_one_time_purchase", nil)})
	}
	presetChoices = append(presetChoices, c.billingRow("custom", "cli.billing.choice_custom_amount", "cli.billing.desc_custom_amount"), c.cancelRow("", "", ""))
	card := state.Card
	detail := c.t("cli.billing.no_saved_card_short", nil)
	if card != nil {
		detail = c.t("cli.billing.payment_line", map[string]any{"card": card.Display()})
	}
	choice := c.modalChoice(c.t("cli.billing.add_funds", nil), detail, presetChoices)
	if choice == "" || choice == "cancel" {
		infoPrint("  " + c.t("cli.billing.cancelled_no_funds", nil))
		return
	}
	var amount provider.PyDecimal
	if choice == "custom" {
		entered, ok := c.promptTextInput("  " + c.t("cli.billing.amount_prompt", nil) + " ")
		if !ok {
			infoPrint("  " + c.t("cli.billing.cancelled_no_funds", nil))
			return
		}
		v := must(provider.ValidateChargeAmount(entered, state.MinUSD, state.MaxUSD))
		if !v.OK {
			infoPrint("  \U0001f534 " + v.Error)
			return
		}
		amount = *v.Amount
	} else {
		d, err := provider.ParseDecimal(choice)
		if err != nil {
			infoPrint("  " + c.t("cli.billing.invalid_selection", nil))
			return
		}
		amount = d
	}
	c.billingConfirmAndCharge(state, amount)
}

// Screen three, the total and consent then the charge polled to settlement
func (c *CLI) billingConfirmAndCharge(state *provider.BillingState, amount provider.PyDecimal) {
	card := state.Card
	c.blockHeader("\U0001f4b3", c.t("cli.billing.confirm_purchase", nil), true)
	infoPrint("  " + c.t("cli.billing.total_line", map[string]any{"amount": fmtMoney(&amount)}))
	if card != nil {
		infoPrint("  " + c.t("cli.billing.payment_line", map[string]any{"card": card.Display()}))
		if card.Provenance() == "" {
			c.billingDim(c.t("cli.billing.portal_card_will_be_charged", nil), "", false)
		}
	}
	infoPrint("  " + billingRule)
	c.billingDim(c.t("cli.billing.consent_charge", nil), "", false)
	confirmChoices := []slashConfirmChoice{
		{Value: "pay", Label: c.t("cli.billing.choice_pay_now", map[string]any{"amount": fmtMoney(&amount)}), Desc: c.t("cli.billing.desc_submit_charge", nil)},
		c.billingRow("portal", "cli.billing.choice_manage_on_portal", "cli.billing.desc_manage_card_billing"),
		c.cancelRow("", "cli.billing.choice_go_back", "cli.billing.desc_do_not_charge")}
	if c.app == nil {
		infoPrint("  " + c.t("cli.billing.run_interactive_to_confirm", nil))
		return
	}
	detail := c.t("cli.billing.no_saved_card_lower", nil)
	if card != nil {
		detail = card.Display()
	}
	switch c.modalChoice(c.t("cli.billing.pay_question", map[string]any{"amount": fmtMoney(&amount)}), detail, confirmChoices) {
	case "portal":
		c.billingOpenPortal(state)
		return
	case "pay":
	default:
		infoPrint("  " + c.t("cli.billing.cancelled_no_funds", nil))
		return
	}
	key := provider.NewIdempotencyKey()
	c.billingSubmitAndPoll(state, amount, key, "  \U0001f534 "+c.t("cli.billing.no_charge_id", nil), c.t("cli.billing.charge_submitted", nil),
		func() { c.billingHandleScopeRequired(state, &amount, key) })
}

// Posts the charge then polls it, a scope denial going to onScope when given
func (c *CLI) billingSubmitAndPoll(state *provider.BillingState, amount provider.PyDecimal, key, missingMsg, statusMsg string, onScope func()) {
	result, err := provider.PostNousBillingCharge(c.billingEnv(), amount.String(), key, billingTimeout)
	if err != nil {
		be := asBillingError(err)
		if be == nil {
			panic(err)
		}
		if onScope != nil && be.Kind == provider.BillingErrorScopeRequired {
			onScope()
		} else {
			c.billingRenderChargeError(state, be)
		}
		return
	}
	m, _ := result.(map[string]any)
	chargeID := m["chargeId"]
	if !pyrepr.Truthy(chargeID) {
		infoPrint(missingMsg)
		return
	}
	cprint("  " + dimTTY(statusMsg))
	c.billingPollCharge(state, chargeID, amount)
}

// Polls the charge every two seconds for five minutes, settled being the ledger truth
func (c *CLI) billingPollCharge(state *provider.BillingState, chargeID any, amount provider.PyDecimal) {
	deadline := time.Now().Add(300 * time.Second)
	for time.Now().Before(deadline) {
		status, err := provider.GetNousChargeStatus(c.billingEnv(), chargeID, billingTimeout)
		if err != nil {
			be := asBillingError(err)
			if be == nil {
				panic(err)
			}
			if be.Kind.IsTransient() {
				wait := 5
				if be.RetryAfter != nil && *be.RetryAfter != 0 {
					wait = *be.RetryAfter
				}
				time.Sleep(time.Duration(min(wait, 30)) * time.Second)
				continue
			}
			infoPrint("  " + c.t("cli.billing.could_not_check_charge", map[string]any{"error": be.Error()}))
			return
		}
		m, _ := status.(map[string]any)
		switch m["status"] {
		case "settled":
			added := fmtMoney(&amount)
			if amt := m["amountUsd"]; pyrepr.Truthy(amt) {
				added = fmtMoney(provider.ParseMoney(amt))
			}
			infoPrint("  " + c.t("cli.billing.added_to_balance", map[string]any{"amount": added}))
			return
		case "failed":
			c.billingRenderChargeFailed(state, m["reason"])
			return
		}
		time.Sleep(2 * time.Second)
	}
	infoPrint("  " + c.t("cli.billing.still_processing", nil))
	c.billingPortalHint(state, "")
}

// Copy for a failed charge reason followed by the portal funnel
func (c *CLI) billingRenderChargeFailed(state *provider.BillingState, reason any) {
	text := ""
	if pyrepr.Truthy(reason) {
		text = pystr.Strip(pyrepr.Str(reason))
	}
	if key, ok := chargeFailedKeys[text]; ok {
		infoPrint("  " + c.t(key, nil))
	} else {
		shown := text
		if shown == "" {
			shown = "processing_error"
		}
		infoPrint("  " + c.t("cli.billing.charge_failed_generic", map[string]any{"reason": shown}))
	}
	c.billingPortalHint(state, "")
}

// Submit time billing error copy, revocations first and transients before scope, then the portal line
func (c *CLI) billingRenderChargeError(state *provider.BillingState, be *provider.BillingError) {
	code := optText(be.ErrorCode)
	portalURL := pyrepr.Or(be.PortalURL, state.PortalURL)
	switch {
	case be.Kind == provider.BillingErrorRemoteSpendingRevoked || code == "remote_spending_revoked":
		who := c.t("cli.billing.revoked_by_you", nil)
		if be.Actor == "admin" {
			who = c.t("cli.billing.revoked_by_admin", nil)
		}
		infoPrint("  " + c.t("cli.billing.revoked_reconnect", map[string]any{"who": who}))
	case be.Kind == provider.BillingErrorSessionRevoked || code == "session_revoked":
		infoPrint("  " + c.t("cli.billing.session_logged_out", nil))
	case chargeErrorKeys[code] != "" || be.Code == "remote_spending_disabled":
		key := chargeErrorKeys[code]
		if key == "" {
			key = chargeErrorKeys["cli_billing_disabled"]
		}
		infoPrint("  " + c.t(key, nil))
	case code == "monthly_cap_exceeded":
		if remaining, ok := be.Payload["remainingUsd"]; ok && remaining != nil {
			infoPrint("  " + c.t("cli.billing.monthly_cap_reached_headroom", map[string]any{"remaining": remaining}))
		} else {
			infoPrint("  " + c.t("cli.billing.monthly_cap_reached", nil))
		}
	case be.Kind.IsTransient():
		mins := ""
		if be.RetryAfter != nil && *be.RetryAfter != 0 {
			mins = c.t("cli.billing.try_again_in_min", map[string]any{"minutes": max(1, int(math.RoundToEven(float64(*be.RetryAfter)/60)))})
		}
		infoPrint("  " + c.t("cli.billing.too_many_charges", map[string]any{"mins": mins}))
	case code == "insufficient_scope":
		infoPrint("  " + c.t("cli.billing.remote_spending_needs_approval", nil))
	default:
		infoPrint("  \U0001f534 " + be.Error())
	}
	if pyrepr.Truthy(portalURL) {
		infoPrint("  " + c.t("cli.billing.portal_line", map[string]any{"url": portalURL}))
	}
}

// Steps up on insufficient scope, re checks the org gate and card, then resumes the amount on confirm
func (c *CLI) billingHandleScopeRequired(state *provider.BillingState, amount *provider.PyDecimal, idempotencyKey string) {
	amountStr := c.t("cli.billing.your_topup", nil)
	if amount != nil {
		amountStr = fmtMoney(amount)
	}
	granted := c.stepUpRemoteSpending(c.t("cli.billing.charge_stepup_explain", map[string]any{"amount": amountStr}),
		"  "+c.t("cli.billing.charge_stepup_noninteractive", nil), "  "+c.t("cli.billing.charge_stepup_declined", nil),
		"  "+c.t("cli.billing.charge_stepup_not_granted", nil))
	if !granted {
		return
	}
	fresh := must(provider.BuildBillingState(c.ctx, billingTimeout))
	if !(fresh.LoggedIn && fresh.CLIBillingEnabled) {
		infoPrint("  " + c.t("cli.billing.allowed_but_org_off", nil))
		c.billingPortalHint(fresh, "")
		return
	}
	if fresh.Card == nil {
		infoPrint("  ✓ " + c.t("cli.billing.allowed_no_card", nil))
		c.billingDim(c.t("cli.billing.topup_on_portal_to_continue", nil), "", false)
		c.billingPortalHint(fresh, "")
		return
	}
	if amount == nil {
		infoPrint("  ✓ " + c.t("cli.billing.allowed_run_topup", nil))
		return
	}
	infoPrint("  ✓ " + c.t("cli.billing.remote_spending_allowed", nil))
	resumeChoices := []slashConfirmChoice{
		{Value: "resume", Label: c.t("cli.billing.choice_resume_topup", map[string]any{"amount": fmtMoney(amount)}), Desc: c.t("cli.billing.desc_resume_topup", nil)},
		c.cancelRow("", "", "cli.billing.desc_do_not_charge")}
	if c.modalChoice(c.t("cli.billing.resume_topup_title", nil), c.t("cli.billing.resume_topup_detail", map[string]any{"amount": fmtMoney(amount)}), resumeChoices) != "resume" {
		infoPrint("  " + c.t("cli.billing.cancelled_no_funds", nil))
		return
	}
	key := idempotencyKey
	if key == "" {
		key = provider.NewIdempotencyKey()
	}
	c.billingSubmitAndPoll(fresh, *amount, key, "  "+c.t("cli.billing.no_charge_id", nil), c.t("cli.billing.resuming_topup", nil), nil)
}

// Asks one auto reload amount, empty keeping the current value while editing, false when cancelled
func (c *CLI) billingAskAmount(state *provider.BillingState, label string, current *provider.PyDecimal, currentlyOn bool) (*provider.PyDecimal, bool) {
	prompt := "  " + c.t("cli.billing.amount_prompt_labeled", map[string]any{"label": label})
	if currentlyOn {
		if cur := fmtMoney(current); cur != "" {
			prompt += " [" + cur + "]: "
		} else {
			prompt += ": "
		}
	} else {
		prompt += ": "
	}
	raw, ok := c.promptTextInput(prompt)
	if !ok {
		infoPrint("  " + c.t("cli.billing.cancelled_yellow", nil))
		return nil, false
	}
	if pystr.Strip(raw) == "" && currentlyOn {
		return current, true
	}
	v := must(provider.ValidateChargeAmount(raw, state.MinUSD, state.MaxUSD))
	if !v.OK || v.Amount == nil {
		infoPrint("  \U0001f534 " + v.Error)
		return nil, false
	}
	return v.Amount, true
}

// Screen four, threshold and reload to amounts patched after consent, or turning auto reload off
func (c *CLI) billingAutoReloadFlow(state *provider.BillingState) {
	if !c.billingRequireAdmin(state, "\U0001f4b3 ", "cli.billing.killswitch_reason_buy") {
		return
	}
	card := state.Card
	ar := state.AutoReload
	currentlyOn := ar != nil && ar.Enabled
	c.blockHeader("\U0001f4b3", c.t("cli.billing.auto_reload", nil), true)
	c.billingDim(c.t("cli.billing.auto_reload_hint", nil), "", false)
	if card == nil {
		infoPrint("  " + c.t("cli.billing.no_saved_card_manage_portal", nil))
		c.billingPortalHint(state, "")
		return
	}
	infoPrint("  " + c.t("cli.billing.card_on_file", map[string]any{"card": card.Masked()}))
	current := ""
	if currentlyOn {
		current = c.t("cli.billing.auto_reload_rule", map[string]any{"threshold": fmtMoney(ar.ThresholdUSD), "reload_to": fmtMoney(ar.ReloadToUSD)})
		infoPrint("  " + c.t("cli.billing.currently", map[string]any{"rule": current}))
	}
	if c.app == nil {
		infoPrint("  " + c.t("cli.billing.run_interactive_to_configure", nil))
		c.billingPortalHint(state, "")
		return
	}
	if currentlyOn {
		top := c.modalChoice(c.t("cli.billing.auto_reload", nil), c.t("cli.billing.auto_reload_on_detail", map[string]any{"rule": current}),
			c.autoReloadTopChoices())
		if top == "off" {
			c.billingAutoReloadDisable(state)
			return
		}
		if top != "edit" {
			infoPrint("  " + c.t("cli.billing.cancelled_yellow", nil))
			return
		}
	}
	var curThreshold, curReload *provider.PyDecimal
	if currentlyOn {
		curThreshold, curReload = ar.ThresholdUSD, ar.ReloadToUSD
	}
	thresholdAmt, ok := c.billingAskAmount(state, c.t("cli.billing.threshold_label", nil), curThreshold, currentlyOn)
	if !ok {
		return
	}
	reloadAmt, ok := c.billingAskAmount(state, c.t("cli.billing.reload_to_label", nil), curReload, currentlyOn)
	if !ok {
		return
	}
	if reloadAmt == nil || thresholdAmt == nil || must(reloadAmt.Cmp(*thresholdAmt)) <= 0 {
		infoPrint("  " + c.t("cli.billing.reload_must_exceed_threshold", nil))
		return
	}
	c.billingDim(c.t("cli.billing.auto_reload_consent", map[string]any{"card": card.Masked(), "threshold": fmtMoney(thresholdAmt)}), "", true)
	if c.modalChoice(c.t("cli.billing.turn_on_auto_reload_question", nil),
		c.t("cli.billing.auto_reload_rule_cap", map[string]any{"threshold": fmtMoney(thresholdAmt), "reload_to": fmtMoney(reloadAmt)}),
		c.autoReloadAgreeChoices()) != "agree" {
		infoPrint("  " + c.t("cli.billing.cancelled_yellow", nil))
		return
	}
	if c.billingPatchAutoTopUp(state, true, decimalFloat(thresholdAmt), decimalFloat(reloadAmt)) {
		infoPrint("  " + c.t("cli.billing.auto_reload_enabled", map[string]any{"threshold": fmtMoney(thresholdAmt), "reload_to": fmtMoney(reloadAmt)}))
	}
}

// Patches auto top up, scope denials stepping up and other errors rendered, true on success
func (c *CLI) billingPatchAutoTopUp(state *provider.BillingState, enabled bool, threshold, topUp float64) bool {
	_, err := provider.PatchNousAutoTopUp(c.billingEnv(), enabled, threshold, topUp, billingTimeout)
	return c.billingPatchOutcome(state, err)
}

// Routes a billing patch failure to the step up or the error renderer, true when it succeeded
func (c *CLI) billingPatchOutcome(state *provider.BillingState, err error) bool {
	if err == nil {
		return true
	}
	be := asBillingError(err)
	if be == nil {
		panic(err)
	}
	if be.Kind == provider.BillingErrorScopeRequired {
		c.billingHandleScopeRequired(state, nil, "")
		return false
	}
	c.billingRenderChargeError(state, be)
	return false
}

// Offers turning keep going on credits off to an admin even with remote spending off
func (c *CLI) billingOfferKeepGoingOff(state *provider.BillingState) {
	if !(must(state.CanChangePlan()) && state.KeepGoingOnCredits != nil && *state.KeepGoingOnCredits && c.app != nil) {
		return
	}
	if c.modalChoice(c.t("cli.billing.keep_going_is_on_title", nil), "", c.keepGoingOffChoices()) == "off" {
		c.billingKeepGoingOnCreditsFlow(state)
	}
}

// Flips keep going on credits, asking first only when turning it on
func (c *CLI) billingKeepGoingOnCreditsFlow(state *provider.BillingState) {
	turnOn := state.KeepGoingOnCredits == nil || !*state.KeepGoingOnCredits
	if turnOn && c.modalChoice(c.t("cli.billing.keep_going_confirm_title", nil), c.t("cli.billing.keep_going_confirm_body", nil),
		c.keepGoingAgreeChoices()) != "agree" {
		infoPrint("  " + c.t("cli.billing.cancelled_yellow", nil))
		return
	}
	c.billingSetKeepGoing(state, turnOn, true)
}

// Patches keep going on credits, a scope denial stepping up and sending the same change once
func (c *CLI) billingSetKeepGoing(state *provider.BillingState, turnOn, allowStepup bool) {
	_, err := provider.PatchNousKeepGoingOnCredits(c.billingEnv(), turnOn, billingTimeout)
	if err != nil {
		be := asBillingError(err)
		if be == nil {
			panic(err)
		}
		if be.Kind == provider.BillingErrorScopeRequired {
			if !allowStepup {
				c.billingRenderChargeError(state, be)
				return
			}
			if c.stepUpRemoteSpending(c.t("cli.subscription.stepup_explain", nil),
				"  "+c.t("cli.subscription.stepup_noninteractive", nil), "  "+c.t("cli.subscription.stepup_declined", nil),
				"  "+c.t("cli.subscription.stepup_not_granted", nil)) {
				c.billingSetKeepGoing(state, turnOn, false)
			}
			return
		}
		c.billingRenderChargeError(state, be)
		return
	}
	key := "cli.billing.keep_going_turned_off"
	if turnOn {
		key = "cli.billing.keep_going_turned_on"
	}
	infoPrint("  " + c.t(key, nil))
	if c.agentRef() != nil {
		probeCall(func() { provider.ApplyKeepGoingChange(c.agentRef().CreditsCapture(), turnOn) })
	}
}

// Patches auto reload off, echoing the current amounts or zero the endpoint still requires
func (c *CLI) billingAutoReloadDisable(state *provider.BillingState) {
	ar := state.AutoReload
	thr, rel := 0.0, 0.0
	if ar != nil && ar.ThresholdUSD != nil {
		thr = decimalFloat(ar.ThresholdUSD)
	}
	if ar != nil && ar.ReloadToUSD != nil {
		rel = decimalFloat(ar.ReloadToUSD)
	}
	if c.billingPatchAutoTopUp(state, false, thr, rel) {
		infoPrint("  " + c.t("cli.billing.auto_reload_disabled", nil))
	}
}

// Screen five, the read only monthly spend limit with the portal funnel
func (c *CLI) billingLimitScreen(state *provider.BillingState) {
	c.blockHeader("\U0001f4b3", c.t("cli.billing.monthly_spend_limit", nil), true)
	limit := state.MonthlyCap
	if limit == nil || limit.LimitUSD == nil {
		c.billingDim(c.t("cli.billing.no_monthly_cap", nil), "", false)
	} else {
		ceiling := ""
		if limit.IsDefaultCeiling {
			ceiling = c.t("cli.billing.default_ceiling_suffix", nil)
		}
		infoPrint("  " + c.t("cli.billing.used_this_month", map[string]any{"spent": fmtMoney(limit.SpentThisMonthUSD), "limit": fmtMoney(limit.LimitUSD), "ceiling": ceiling}))
	}
	c.billingDim(c.t("cli.billing.monthly_limit_read_only", nil), "", false)
	c.billingPortalHint(state, "")
}
