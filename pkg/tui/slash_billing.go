package tui

import (
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"
)

// Refusals a charge poll cannot settle, the ones leaving the outcome unknown and the ones it waits out
var (
	chargeUnknownRefusals = map[string]bool{"insufficient_scope": true, "remote_spending_revoked": true, "session_revoked": true}
	chargeRetryRefusals   = map[string]bool{"endpoint_unavailable": true, "internal_error": true, "network_error": true,
		"rate_limited": true, "stripe_unavailable": true, "temporarily_unavailable": true}
)

// Cadence of the charge poll, how long it polls at most and the longest throttle wait it honours
const (
	chargePollEvery    = 2 * time.Second
	chargePollCap      = 5 * time.Minute
	chargeMaxRetryWait = 30 * time.Second
)

// Dollar amount the server accepts, whole dollars or up to two decimals
var topupAmountRe = regexp.MustCompile(`^\d+(\.\d{1,2})?$`)

// One /topup run, the state it opened with, its window and the requests behind every screen
type topup struct {
	a     *app
	r     *slashRun
	first map[string]any
	card  *sheetCard
}

func (t *topup) sys(text string) { t.a.sys(text) }

// Prints a failed request unless a newer command or another session replaced the run
func (t *topup) guardedErr(err error) {
	if !t.r.stale() {
		t.r.fail(err)
	}
}

// Retry hint of a throttled or unavailable refusal, empty without a retry_after
func retrySuffix(after any) string {
	seconds, isNumber := after.(float64)
	if !isNumber || seconds == 0 || seconds != seconds {
		return ""
	}
	return tr("slashCmd.topup.error.retryIn", math.Max(1, math.Floor(seconds/60+0.5)))
}

// Prints a billing refusal in the user's words with the portal link, a revoke closing the window instead
func (t *topup) renderError(env map[string]any) tea.Cmd {
	code, _ := env["error"].(string)
	switch code {
	case "insufficient_scope":
		t.sys(tr("slashCmd.topup.error.insufficientScope"))
	case "remote_spending_revoked":
		who := tr("slashCmd.topup.error.revokedByYou")
		if env["actor"] == "admin" {
			who = tr("slashCmd.topup.error.revokedByAdmin")
		}
		t.sys(tr("slashCmd.topup.error.revokedReconnect", who))
		return t.card.close()
	case "session_revoked":
		t.sys(tr("slashCmd.topup.error.sessionRevoked"))
		return t.card.close()
	case "cli_billing_disabled", "remote_spending_disabled":
		t.sys(tr("slashCmd.topup.error.remoteSpendingDisabled"))
	case "role_required":
		t.sys(tr("slashCmd.topup.error.roleRequired"))
	case "consent_required":
		t.sys(tr("slashCmd.topup.error.consentRequired"))
	case "org_access_denied":
		t.sys(tr("slashCmd.topup.error.orgAccessDenied"))
	case "upgrade_cap_exceeded":
		t.sys(tr("slashCmd.topup.error.upgradeCapExceeded"))
	case "auto_top_up_disabled_failures":
		t.sys(tr("slashCmd.topup.error.autoTopUpDisabledFailures"))
	case "idempotency_conflict":
		t.sys(tr("slashCmd.topup.error.idempotencyConflict"))
	case "no_payment_method":
		t.sys(tr("slashCmd.topup.error.noPaymentMethod"))
	case "monthly_cap_exceeded":
		payload, _ := env["payload"].(map[string]any)
		if remaining := payload["remainingUsd"]; remaining != nil {
			t.sys(tr("slashCmd.topup.error.monthlyCapExceededRemaining", jsValueString(remaining)))
		} else {
			t.sys(tr("slashCmd.topup.error.monthlyCapExceeded"))
		}
	case "rate_limited", "temporarily_unavailable":
		t.sys(tr("slashCmd.topup.error.rateLimited", retrySuffix(env["retry_after"])))
	case "stripe_unavailable":
		t.sys(tr("slashCmd.topup.error.stripeUnavailable", retrySuffix(env["retry_after"])))
	default:
		message := paramStr(env["message"])
		if message == "" {
			message = code
		}
		if message == "" {
			message = tr("slashCmd.topup.error.genericFallback")
		}
		t.sys(tr("slashCmd.topup.error.generic", message))
	}
	if portal := paramStr(env["portal_url"]); portal != "" {
		t.sys(tr("slashCmd.topup.error.portal", portal))
	}
	return nil
}

// Session the step up names, none when the run started without one
func stepUpParams(sid string) map[string]any {
	if sid == "" {
		return map[string]any{}
	}
	return map[string]any{"session_id": sid}
}

// Runs the Remote Spending device flow, then with whether the grant landed, a failed request counting as not yet
func (t *topup) requestRemoteSpending(then func(bool) tea.Cmd) tea.Cmd {
	return gatewayCall(t.a.gw, "billing.step_up", stepUpParams(t.r.sid), func(v any, err error) tea.Cmd {
		r := asRPCResult(v)
		return then(err == nil && truthyJS(r["ok"]) && truthyJS(r["granted"]))
	})
}

// Polls a charge until it settles, fails, is refused or the cap passes, a newer command cancelling it
func (t *topup) pollCharge(chargeID, portal string, start time.Time) tea.Cmd {
	if t.r.stale() {
		return nil
	}
	again := func(wait time.Duration) tea.Cmd {
		return tea.Tick(wait, func(time.Time) tea.Msg {
			return asyncDoneMsg{run: func() tea.Cmd { return t.pollCharge(chargeID, portal, start) }}
		})
	}
	return gatewayCall(t.a.gw, "billing.charge_status", map[string]any{"charge_id": chargeID}, func(v any, err error) tea.Cmd {
		status := asRPCResult(v)
		if err == nil && status == nil {
			err = errors.New(tr("session.common.invalidResponse", "billing.charge_status"))
		}
		if err != nil {
			t.guardedErr(err)
			if !t.r.stale() {
				t.sys(tr("slashCmd.topup.charge.unconfirmed"))
			}
			return nil
		}
		timedOut := t.a.store.now().Sub(start) >= chargePollCap
		if !truthyJS(status["ok"]) {
			code, _ := status["error"].(string)
			switch {
			case chargeUnknownRefusals[code]:
				if t.r.stale() {
					return nil
				}
				shown := t.renderError(status)
				t.sys(tr("slashCmd.topup.charge.unconfirmed"))
				return shown
			case chargeRetryRefusals[code] && timedOut:
				t.chargeTimedOut(portal)
				return nil
			case chargeRetryRefusals[code]:
				seconds := 5.0
				if after, isNumber := status["retry_after"].(float64); isNumber {
					seconds = after
				}
				return again(min(time.Duration(seconds*float64(time.Second)), chargeMaxRetryWait))
			}
			if !t.r.stale() {
				reason := paramStr(status["message"])
				if reason == "" {
					reason = code
				}
				if reason == "" {
					reason = tr("slashCmd.topup.charge.couldNotCheckFallback")
				}
				t.sys(tr("slashCmd.topup.charge.couldNotCheck", reason))
			}
			return nil
		}
		switch status["status"] {
		case "settled":
			if !t.r.stale() {
				amount := tr("slashCmd.topup.charge.creditsFallback")
				if truthyJS(status["amount_usd"]) {
					amount = "$" + jsValueString(status["amount_usd"])
				}
				t.sys(tr("slashCmd.topup.charge.settled", amount))
			}
			return nil
		case "failed":
			if !t.r.stale() {
				t.chargeFailed(status["reason"], portal)
			}
			return nil
		}
		if timedOut {
			t.chargeTimedOut(portal)
			return nil
		}
		return again(chargePollEvery)
	})
}

// Says the charge is still processing after the poll cap, with the portal link
func (t *topup) chargeTimedOut(portal string) {
	if t.r.stale() {
		return
	}
	t.sys(tr("slashCmd.topup.charge.timedOut"))
	if portal != "" {
		t.sys(tr("slashCmd.topup.error.portal", portal))
	}
}

// Prints why a charge failed by its reason, then the portal link
func (t *topup) chargeFailed(reason any, portal string) {
	text, _ := reason.(string)
	switch trimJS(text) {
	case "authentication_required":
		t.sys(tr("slashCmd.topup.charge.authenticationRequired"))
	case "payment_method_expired":
		t.sys(tr("slashCmd.topup.charge.paymentMethodExpired"))
	case "card_declined":
		t.sys(tr("slashCmd.topup.charge.cardDeclined"))
	case "processing_error":
		t.sys(tr("slashCmd.topup.charge.failedReason", "processing_error"))
	default:
		if text == "" {
			text = "processing_error"
		}
		t.sys(tr("slashCmd.topup.charge.failedReason", text))
	}
	if portal != "" {
		t.sys(tr("slashCmd.topup.error.portal", portal))
	}
}

// Typed amount checked against the dollar format and the bounds of the opening state, the amount or why not
func (t *topup) validate(raw string) (string, string) {
	cleaned := trimJS(strings.TrimPrefix(trimJS(raw), "$"))
	if cleaned == "" || !topupAmountRe.MatchString(cleaned) {
		return "", tr("slashCmd.topup.validate.invalid")
	}
	value, _ := strconv.ParseFloat(cleaned, 64)
	if !(value > 0) {
		return "", tr("slashCmd.topup.validate.notPositive")
	}
	if low := t.first["min_usd"]; low != nil && value < jsNumberOf(low) {
		return "", tr("slashCmd.topup.validate.minimum", jsValueString(low))
	}
	if high := t.first["max_usd"]; high != nil && value > jsNumberOf(high) {
		return "", tr("slashCmd.topup.validate.maximum", jsValueString(high))
	}
	return cleaned, ""
}

// Billing write with no payload beyond ok, a refusal printed, then with whether it landed
func (t *topup) mutate(method string, params map[string]any, then func(bool) tea.Cmd) tea.Cmd {
	return gatewayCall(t.a.gw, method, params, func(v any, err error) tea.Cmd {
		if err != nil {
			t.guardedErr(err)
			return then(false)
		}
		r := asRPCResult(v)
		if truthyJS(r["ok"]) {
			return then(true)
		}
		var shown tea.Cmd
		if r != nil {
			shown = t.renderError(r)
		}
		return tea.Batch(shown, then(false))
	})
}

// Turns keep going on credits on or off, a missing Remote Spending grant running the step up before one resend
func (t *topup) keepGoing(enabled bool, then func(bool) tea.Cmd) tea.Cmd {
	params := map[string]any{"enabled": enabled}
	return gatewayCall(t.a.gw, "billing.keep_going_on_credits", params, func(v any, err error) tea.Cmd {
		if err != nil {
			t.guardedErr(err)
			return then(false)
		}
		r := asRPCResult(v)
		if truthyJS(r["ok"]) {
			return then(true)
		}
		if r != nil && r["error"] == "insufficient_scope" {
			return t.requestRemoteSpending(func(granted bool) tea.Cmd {
				if granted {
					return t.mutate("billing.keep_going_on_credits", params, then)
				}
				return tea.Batch(t.renderError(r), then(false))
			})
		}
		var shown tea.Cmd
		if r != nil {
			shown = t.renderError(r)
		}
		return tea.Batch(shown, then(false))
	})
}

// Starts a charge and polls it once accepted, then with submitted, needs_remote_spending or error
func (t *topup) charge(amount, key string, then func(string) tea.Cmd) tea.Cmd {
	t.sys(tr("slashCmd.topup.charge.submitted"))
	params := map[string]any{"amount_usd": amount}
	if key != "" {
		params["idempotency_key"] = key
	}
	return gatewayCall(t.a.gw, "billing.charge", params, func(v any, err error) tea.Cmd {
		if err != nil {
			t.guardedErr(err)
			return then("error")
		}
		r := asRPCResult(v)
		switch {
		case r == nil:
			return then("error")
		case truthyJS(r["ok"]) && truthyJS(r["charge_id"]):
			return tea.Batch(t.pollCharge(jsValueString(r["charge_id"]), paramStr(t.first["portal_url"]), t.a.store.now()), then("submitted"))
		case r["error"] == "insufficient_scope":
			return then("needs_remote_spending")
		}
		return tea.Batch(t.renderError(r), then("error"))
	})
}

// Opens the portal in the browser and says so
func (t *topup) openPortal(link string) {
	openExternalURL(link)
	t.sys(tr("slashCmd.topup.openingPortal", link))
}

// Reads the billing state again, nil when the read failed or was refused
func (t *topup) refreshState(then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(t.a.gw, "billing.state", map[string]any{}, func(v any, err error) tea.Cmd {
		r := asRPCResult(v)
		if err != nil || !truthyJS(r["ok"]) {
			return then(nil)
		}
		return then(r)
	})
}

// Purchase held from the amount pick through the confirm and the Remote Spending step up
type heldCharge struct{ amount, key string }

// What the open billing screen holds, reset each time another screen opens
type billingScreen struct {
	sel        int
	typing     bool
	checking   bool
	submitting bool
	custom     sheetField
	threshold  sheetField
	reloadTo   sheetField
	problem    string
	phase      string
	row        int
}

// Screens of the billing window, overview, buy, confirm, step up, auto-reload, limit and keep going on credits
type billingFlow struct {
	t       *topup
	state   map[string]any
	screen  string
	pending *heldCharge
	visit   int
	local   billingScreen
}

func (f *billingFlow) title() string  { return tr("billing.title") }
func (f *billingFlow) close() tea.Cmd { return f.t.card.close() }

// Shows a screen with fresh screen state, the auto-reload amounts filled from the billing state
func (f *billingFlow) open(screen string) {
	f.screen, f.visit, f.local = screen, f.visit+1, billingScreen{phase: "prompt"}
	if screen == "autoreload" {
		ar, _ := f.state["auto_reload"].(map[string]any)
		f.local.threshold, f.local.reloadTo = newSheetField(prefillAmount(ar["threshold_usd"])), newSheetField(prefillAmount(ar["reload_to_usd"]))
	}
}

// Raw dollar amount of the state without its dollar sign, empty for none
func prefillAmount(raw any) string {
	if raw == nil {
		return ""
	}
	return trimJS(strings.TrimPrefix(jsValueString(raw), "$"))
}

// Card a charge would use as the state shows it, its display text before the masked number
func cardText(card map[string]any) string {
	return nullishText(card, "display", jsValueString(card["masked"]))
}

// Payment line of the buy and confirm screens
func (f *billingFlow) payLine() string {
	if card, _ := f.state["card"].(map[string]any); card != nil {
		return tr("billing.common.payment", cardText(card))
	}
	return tr("billing.common.noSavedCard")
}

// Flips keep going on credits and prints the new setting once the gateway confirmed it
func (f *billingFlow) applyKeepGoing(enabled bool) tea.Cmd {
	return f.t.keepGoing(enabled, func(ok bool) tea.Cmd {
		switch {
		case ok && enabled:
			f.t.sys(tr("billing.keepGoing.turnedOn"))
		case ok:
			f.t.sys(tr("billing.keepGoing.turnedOff"))
		}
		return nil
	})
}

func (f *billingFlow) draw(d *sheetDraw) {
	switch f.screen {
	case "overview":
		f.drawOverview(d)
	case "buy":
		f.drawBuy(d)
	case "confirm":
		f.drawConfirm(d)
	case "stepup":
		f.drawStepUp(d)
	case "autoreload":
		f.drawAutoReload(d)
	case "limit":
		f.drawLimit(d)
	case "hermes":
		f.drawKeepGoing(d)
	}
}

func (f *billingFlow) key(id, text string) tea.Cmd {
	switch f.screen {
	case "overview":
		items := f.overviewItems()
		return menuKey(&f.local.sel, len(items), id, text, func(i int) tea.Cmd { return f.chooseOverview(items[i].id) }, f.close)
	case "buy":
		return f.keyBuy(id, text)
	case "confirm":
		return f.keyConfirm(id, text)
	case "stepup":
		return f.keyStepUp(id, text)
	case "autoreload":
		return f.keyAutoReload(id, text)
	case "limit":
		return menuKey(&f.local.sel, 2, id, text, f.chooseLimit, f.back)
	case "hermes":
		return menuKey(&f.local.sel, 2, id, text, f.chooseKeepGoing, f.back)
	}
	return nil
}

// Back on the overview
func (f *billingFlow) back() tea.Cmd {
	f.open("overview")
	return nil
}

// One row of the overview, what it does and its label
type billingItem struct{ id, label string }

// Whether the full billing menu shows, an admin of an org with remote spending on
func (f *billingFlow) full() bool {
	return truthyJS(f.state["is_admin"]) && truthyJS(f.state["cli_billing_enabled"])
}

// Keep going row and status line of a window plan, and whether a limited admin may still turn it off
func (f *billingFlow) keepGoingChrome() (row, line string, offOnly bool) {
	if f.state["keep_going_on_credits"] == nil {
		return "", "", false
	}
	if truthyJS(f.state["keep_going_on_credits"]) {
		return tr("billing.keepGoing.rowTurnOff"), tr("billing.keepGoing.onLine"), !f.full() && truthyJS(f.state["is_admin"])
	}
	return tr("billing.keepGoing.rowTurnOn"), tr("billing.keepGoing.offLine"), false
}

// Overview rows, the full menu for a billing admin and the portal and cancel rows otherwise
func (f *billingFlow) overviewItems() []billingItem {
	row, _, offOnly := f.keepGoingChrome()
	var items []billingItem
	if f.full() {
		items = append(items, billingItem{"buy", tr("billing.overview.addFunds")}, billingItem{"autoreload", tr("billing.overview.autoReload")})
	}
	if row != "" && (f.full() || offOnly) {
		items = append(items, billingItem{"keepGoing", row})
	}
	if f.full() {
		items = append(items, billingItem{"limit", tr("billing.overview.monthlyLimit")})
	}
	return append(items, billingItem{"portal", tr("billing.common.manageOnPortal")}, billingItem{"cancel", tr("billing.common.cancel")})
}

// Runs an overview row, turning keep going off at once and asking before turning it on
func (f *billingFlow) chooseOverview(id string) tea.Cmd {
	switch id {
	case "buy", "autoreload", "limit":
		f.open(id)
		return nil
	case "keepGoing":
		if truthyJS(f.state["keep_going_on_credits"]) {
			return tea.Batch(f.applyKeepGoing(false), f.close())
		}
		f.open("hermes")
		return nil
	case "portal":
		if portal := paramStr(f.state["portal_url"]); portal != "" {
			f.t.openPortal(portal)
		}
	}
	return f.close()
}

func (f *billingFlow) drawOverview(d *sheetDraw) {
	s := f.state
	d.line(tr("billing.overview.title", jsTemplate(s, "balance_display")), d.c.Accent, true)
	if truthyJS(s["org_name"]) {
		d.line(orgLine(s, "billing.overview.org"), d.c.Muted, false)
	}
	d.usageBars(s["usage"])
	if ar, _ := s["auto_reload"].(map[string]any); ar != nil {
		auto := tr("billing.overview.autoReloadOff")
		if truthyJS(ar["enabled"]) {
			auto = tr("billing.overview.autoReloadOn", jsTemplate(ar, "threshold_display"), jsTemplate(ar, "reload_to_display"))
		}
		d.line(auto, d.c.Muted, false)
	}
	if _, line, _ := f.keepGoingChrome(); line != "" {
		d.line(line, d.c.Muted, false)
	}
	if f.full() {
		hint := tr("billing.overview.noCardHint")
		if card, _ := s["card"].(map[string]any); card != nil {
			hint = tr("billing.overview.card", cardText(card))
		}
		d.line(hint, d.c.Muted, false)
	}
	note := ""
	switch {
	case !truthyJS(s["is_admin"]):
		note = tr("billing.overview.needsBillingPermissions")
	case !truthyJS(s["cli_billing_enabled"]):
		note = tr("billing.overview.remoteSpendingOff")
	}
	if note != "" {
		d.blank()
		d.line(note, d.c.Warn, false)
	}
	d.blank()
	items := f.overviewItems()
	labels := make([]string, len(items))
	for i, item := range items {
		labels[i] = item.label
	}
	d.menu(labels, shownRow(f.local.sel, len(items)), func(i int) tea.Cmd { return f.chooseOverview(items[i].id) })
	d.blank()
	d.footer(tr("billing.overview.hint", float64(len(items))))
}

// Buy rows, the presets with a custom amount, or the add card steps when no card is on file
func (f *billingFlow) buyRows() ([]string, bool) {
	if f.state["card"] == nil {
		return []string{tr("billing.buy.addCardOnPortal"), tr("billing.buy.checkAgain"), tr("billing.common.back")}, true
	}
	presets, _ := f.state["charge_presets_display"].([]any)
	rows := make([]string, 0, len(presets)+2)
	for _, preset := range presets {
		rows = append(rows, jsValueString(preset))
	}
	return append(rows, tr("billing.buy.customAmount"), tr("billing.common.cancel")), false
}

// Holds the amount with a fresh idempotency key and asks to confirm it
func (f *billingFlow) toConfirm(amount string) {
	f.pending = &heldCharge{amount: amount, key: uuid.NewString()}
	f.open("confirm")
}

// Reads the billing state again so a card added on the portal turns the add card steps into the presets
func (f *billingFlow) recheck() tea.Cmd {
	if f.local.checking {
		return nil
	}
	f.local.checking = true
	visit := f.visit
	return f.t.refreshState(func(fresh map[string]any) tea.Cmd {
		here := f.visit == visit
		if here {
			f.local.checking = false
		}
		if fresh == nil {
			if here {
				f.local.problem = tr("billing.buy.refreshFailed")
			}
			return nil
		}
		if here {
			f.local.problem = ""
		}
		f.state = fresh
		if card, _ := fresh["card"].(map[string]any); card != nil {
			f.t.sys(tr("billing.buy.cardFound", cardText(card)))
		} else {
			f.t.sys(tr("billing.buy.stillNoCard"))
		}
		return nil
	})
}

// Runs a buy row, the add card steps without a card and the presets or the custom amount with one
func (f *billingFlow) chooseBuy(i int) tea.Cmd {
	rows, noCard := f.buyRows()
	presets := len(rows) - 2
	switch {
	case noCard && i == 0:
		if portal := paramStr(f.state["portal_url"]); portal != "" {
			f.t.openPortal(portal)
			f.t.sys(tr("billing.buy.addCardThenCheckAgain"))
		} else {
			f.local.problem = tr("billing.buy.portalLinkFailed")
		}
	case noCard && i == 1:
		return f.recheck()
	case noCard, i > presets:
		f.open("overview")
	case i < presets:
		raw := listAt(f.state["charge_presets"], i)
		if raw == nil {
			raw = listAt(f.state["charge_presets_display"], i)
		}
		amount, problem := f.t.validate(prefillAmount(raw))
		if problem != "" {
			f.local.problem = problem
			return nil
		}
		f.toConfirm(amount)
	default:
		f.local.problem, f.local.typing = "", true
	}
	return nil
}

func (f *billingFlow) keyBuy(id, text string) tea.Cmd {
	l := &f.local
	if id == "esc" {
		if l.typing {
			l.typing, l.problem = false, ""
			return nil
		}
		return f.back()
	}
	if l.typing {
		if id != "enter" {
			l.custom.edit(id, text)
			return nil
		}
		amount, problem := f.t.validate(l.custom.text())
		if problem != "" {
			l.problem = problem
			return nil
		}
		f.toConfirm(amount)
		return nil
	}
	rows, _ := f.buyRows()
	switch id {
	case "up":
		if l.sel > 0 {
			l.sel--
		}
	case "down":
		if l.sel < len(rows)-1 {
			l.sel++
		}
	case "enter":
		return f.chooseBuy(shownRow(l.sel, len(rows)))
	case "":
		if n := leadingInt(text); n >= 1 && n <= len(rows) {
			return f.chooseBuy(n - 1)
		}
	}
	return nil
}

func (f *billingFlow) drawBuy(d *sheetDraw) {
	l := &f.local
	d.line(tr("billing.buy.title"), d.c.Accent, true)
	if l.typing {
		d.line(f.payLine(), d.c.Muted, false)
		d.blank()
		d.field(tr("billing.buy.enterCustomAmount"), &l.custom, true, nil)
		if l.problem != "" {
			d.line(l.problem, d.c.Error, false)
		}
		d.blank()
		d.footer(tr("billing.buy.typingHint"))
		return
	}
	rows, noCard := f.buyRows()
	if noCard {
		d.line(tr("billing.buy.noSavedCardLine"), d.c.Text, false)
		d.line(tr("billing.buy.addCardOnce"), d.c.Muted, false)
	} else {
		d.line(f.payLine(), d.c.Muted, false)
	}
	d.blank()
	d.menu(rows, shownRow(l.sel, len(rows)), f.chooseBuy)
	if l.problem != "" {
		d.line(l.problem, d.c.Error, false)
	}
	d.blank()
	footer := tr("billing.buy.hint", float64(len(rows)))
	if noCard && l.checking {
		footer = tr("billing.buy.checkingForCard")
	}
	d.footer(footer)
}

// Amount and idempotency key of the held purchase, empty once it was let go
func (f *billingFlow) held() (string, string) {
	if f.pending == nil {
		return "", ""
	}
	return f.pending.amount, f.pending.key
}

// Charges the held amount once, a missing Remote Spending grant opening the step up and anything else closing
func (f *billingFlow) pay() tea.Cmd {
	if f.local.submitting {
		return nil
	}
	f.local.submitting = true
	amount, key := f.held()
	return f.t.charge(amount, key, func(outcome string) tea.Cmd {
		if outcome == "needs_remote_spending" {
			f.open("stepup")
			return nil
		}
		return f.close()
	})
}

// Lets the held purchase go and goes back to the amounts
func (f *billingFlow) confirmBack() tea.Cmd {
	f.pending = nil
	f.open("buy")
	return nil
}

func (f *billingFlow) keyConfirm(id, text string) tea.Cmd {
	switch {
	case id == "esc", id == "" && strings.ToLower(text) == "n":
		return f.confirmBack()
	case id == "" && strings.ToLower(text) == "y":
		return f.pay()
	case id == "up":
		f.local.sel = 0
	case id == "down":
		f.local.sel = 1
	case id == "enter" && f.local.sel == 0:
		return f.pay()
	case id == "enter":
		return f.confirmBack()
	}
	return nil
}

func (f *billingFlow) drawConfirm(d *sheetDraw) {
	amount, _ := f.held()
	d.line(tr("billing.confirm.title"), d.c.Accent, true)
	d.line(tr("billing.confirm.total", "$"+amount), d.c.Text, false)
	d.line(f.payLine(), d.c.Muted, false)
	if card, _ := f.state["card"].(map[string]any); card != nil && !truthyJS(card["resolved_via"]) {
		d.line(tr("billing.confirm.portalCardCharged"), d.c.Muted, false)
	}
	d.line(tr("billing.confirm.authorization"), d.c.Muted, false)
	d.blank()
	d.action(tr("billing.confirm.payNow", "$"+amount), d.c.Ok, f.local.sel == 0, f.pay)
	d.action(tr("billing.common.cancel"), "", f.local.sel == 1, f.confirmBack)
	d.blank()
	d.footer(tr("billing.confirm.hint"))
}

// Starts the Remote Spending device flow, holding the purchase until the grant lands
func (f *billingFlow) allow() tea.Cmd {
	if f.local.phase != "prompt" {
		return nil
	}
	f.local.phase = "waiting"
	f.t.sys(tr("billing.stepUp.openingBrowser"))
	visit := f.visit
	return f.t.requestRemoteSpending(func(granted bool) tea.Cmd {
		if !granted {
			f.t.sys(tr("billing.stepUp.notAllowed"))
			return f.close()
		}
		if f.visit == visit {
			f.local.phase = "granted"
		}
		return nil
	})
}

// Charges the held purchase again once Remote Spending is allowed
func (f *billingFlow) resumeCharge() tea.Cmd {
	if f.local.phase != "granted" {
		return nil
	}
	f.local.phase = "resuming"
	f.t.sys(tr("billing.stepUp.allowedResuming"))
	amount, key := f.held()
	return f.t.charge(amount, key, func(outcome string) tea.Cmd {
		if outcome == "needs_remote_spending" {
			f.t.sys(tr("billing.stepUp.stillNeedsApproval"))
		}
		return f.close()
	})
}

// Declines the step up, nothing charged
func (f *billingFlow) decline() tea.Cmd {
	f.t.sys(tr("billing.stepUp.declined"))
	return f.close()
}

func (f *billingFlow) keyStepUp(id, text string) tea.Cmd {
	switch f.local.phase {
	case "waiting", "resuming":
		if id == "esc" {
			return f.close()
		}
		return nil
	case "granted":
		switch id {
		case "esc":
			return f.close()
		case "enter":
			return f.resumeCharge()
		}
		return nil
	}
	switch {
	case id == "esc", id == "" && strings.ToLower(text) == "n":
		return f.decline()
	case id == "" && strings.ToLower(text) == "y":
		return f.allow()
	case id == "up":
		f.local.sel = 0
	case id == "down":
		f.local.sel = 1
	case id == "enter" && f.local.sel == 0:
		return f.allow()
	case id == "enter":
		return f.decline()
	}
	return nil
}

func (f *billingFlow) drawStepUp(d *sheetDraw) {
	amount, _ := f.held()
	amount = "$" + amount
	switch f.local.phase {
	case "waiting":
		d.line(tr("billing.stepUp.title"), d.c.Accent, true)
		d.line(tr("billing.stepUp.waitingForBrowser"), d.c.Warn, false)
		d.line(tr("billing.stepUp.approveInPage"), d.c.Muted, false)
		d.line(tr("billing.stepUp.heldHere", amount), d.c.Muted, false)
		d.blank()
		d.footer(tr("billing.stepUp.escCancel"))
	case "granted":
		d.line(tr("billing.stepUp.grantedTitle"), d.c.Ok, true)
		d.line(tr("billing.stepUp.readyToFinish", amount), d.c.Text, false)
		d.blank()
		d.action(tr("billing.stepUp.pressEnterToResume"), d.c.Ok, true, f.resumeCharge)
		d.blank()
		d.footer(tr("billing.stepUp.grantedHint"))
	case "resuming":
		d.line(tr("billing.stepUp.title"), d.c.Accent, true)
		d.line(tr("billing.stepUp.resuming", amount), d.c.Muted, false)
		d.blank()
		d.footer(tr("billing.stepUp.escCancel"))
	default:
		d.line(tr("billing.stepUp.promptTitle"), d.c.Warn, true)
		d.line(tr("billing.stepUp.promptBody"), d.c.Text, false)
		d.line(tr("billing.stepUp.promptDetail", amount), d.c.Muted, false)
		d.blank()
		d.action(tr("billing.stepUp.allow"), d.c.Ok, f.local.sel == 0, f.allow)
		d.action(tr("billing.stepUp.notNow"), "", f.local.sel == 1, f.decline)
		d.blank()
		d.footer(tr("billing.stepUp.promptHint"))
	}
}

// Auto-reload settings of the state and the card auto-refill charges when it is not the card on file
func (f *billingFlow) autoReload() (map[string]any, map[string]any) {
	ar, _ := f.state["auto_reload"].(map[string]any)
	card, _ := ar["card"].(map[string]any)
	if card["kind"] != "distinct" {
		card = nil
	}
	return ar, card
}

// Auto-reload actions, turn off only while it is on and the portal row for a distinct card
func (f *billingFlow) autoReloadActions() []string {
	ar, distinct := f.autoReload()
	actions := []string{"turnOn"}
	if truthyJS(ar["enabled"]) {
		actions = append(actions, "turnOff")
	}
	if distinct != nil && truthyJS(f.state["portal_url"]) {
		actions = append(actions, "manageCard")
	}
	return append(actions, "cancel")
}

// Validates both amounts, the reload amount above the threshold, the problem shown otherwise
func (f *billingFlow) validatePair() (string, string, bool) {
	l := &f.local
	threshold, problem := f.t.validate(l.threshold.text())
	if problem != "" {
		l.problem = tr("billing.autoReload.thresholdError", problem)
		return "", "", false
	}
	reloadTo, problem := f.t.validate(l.reloadTo.text())
	if problem != "" {
		l.problem = tr("billing.autoReload.reloadToError", problem)
		return "", "", false
	}
	if jsNumberOf(reloadTo) <= jsNumberOf(threshold) {
		l.problem = tr("billing.autoReload.reloadToMustExceedThreshold")
		return "", "", false
	}
	l.problem = ""
	return threshold, reloadTo, true
}

// Number of an amount as Number reads it, zero when it reads none
func amountOrZero(raw any) float64 {
	if n := jsNumberOf(prefillAmount(raw)); n == n {
		return n
	}
	return 0
}

// Runs an auto-reload action, saving the amounts, turning it off, opening the portal or going back
func (f *billingFlow) chooseAutoReload(action string) tea.Cmd {
	portal := paramStr(f.state["portal_url"])
	switch action {
	case "turnOn":
		if f.state["card"] == nil {
			f.t.sys(tr("billing.autoReload.noCardManageOnPortal"))
			if portal != "" {
				f.t.openPortal(portal)
			}
			return f.close()
		}
		threshold, reloadTo, ok := f.validatePair()
		if !ok {
			return nil
		}
		params := map[string]any{"enabled": true, "threshold": jsNumberOf(threshold), "top_up_amount": jsNumberOf(reloadTo)}
		return tea.Batch(f.t.mutate("billing.auto_reload", params, func(saved bool) tea.Cmd {
			if saved {
				f.t.sys(tr("billing.autoReload.turnedOn", "$"+threshold, "$"+reloadTo))
			}
			return nil
		}), f.close())
	case "turnOff":
		ar, _ := f.autoReload()
		params := map[string]any{"enabled": false, "threshold": amountOrZero(ar["threshold_usd"]), "top_up_amount": amountOrZero(ar["reload_to_usd"])}
		return tea.Batch(f.t.mutate("billing.auto_reload", params, func(saved bool) tea.Cmd {
			if saved {
				f.t.sys(tr("billing.autoReload.turnedOff"))
			}
			return nil
		}), f.close())
	case "manageCard":
		if portal != "" {
			f.t.openPortal(portal)
		}
		return f.close()
	}
	return f.back()
}

func (f *billingFlow) keyAutoReload(id, text string) tea.Cmd {
	l := &f.local
	actions := f.autoReloadActions()
	editing := l.row < 2
	switch id {
	case "esc":
		return f.back()
	case "up":
		if l.row > 0 {
			l.row--
		}
		return nil
	case "down":
		if l.row < 2+len(actions)-1 {
			l.row++
		}
		return nil
	case "tab":
		if editing {
			l.row = 1 - l.row
		}
		return nil
	case "enter":
		if editing {
			l.row++
			return nil
		}
		if i := l.row - 2; i < len(actions) {
			return f.chooseAutoReload(actions[i])
		}
		return f.chooseAutoReload("cancel")
	}
	if editing {
		field := &l.threshold
		if l.row == 1 {
			field = &l.reloadTo
		}
		field.edit(id, text)
		return nil
	}
	if n := leadingInt(text); id == "" && n >= 1 && n <= len(actions) {
		return f.chooseAutoReload(actions[n-1])
	}
	return nil
}

func (f *billingFlow) drawAutoReload(d *sheetDraw) {
	l := &f.local
	_, distinct := f.autoReload()
	card, _ := f.state["card"].(map[string]any)
	d.line(tr("billing.autoReload.title"), d.c.Accent, true)
	d.line(tr("billing.autoReload.description"), d.c.Muted, false)
	cardLine := tr("billing.common.noSavedCard")
	if card != nil {
		cardLine = tr("billing.autoReload.cardOnFile", jsValueString(card["masked"]))
	}
	d.line(cardLine, d.c.Muted, false)
	chargeName := tr("billing.autoReload.yourCard")
	if card != nil {
		chargeName = jsValueString(card["masked"])
	}
	if distinct != nil {
		var parts []string
		if truthyJS(distinct["brand"]) {
			parts = append(parts, jsValueString(distinct["brand"]))
		}
		if truthyJS(distinct["last4"]) {
			parts = append(parts, "••"+jsValueString(distinct["last4"]))
		}
		chargeName = strings.Join(parts, " ")
		if chargeName == "" {
			chargeName = tr("billing.autoReload.aDifferentCard")
		}
		d.line(tr("billing.autoReload.distinctCardWarning", chargeName), d.c.Warn, false)
	}
	d.blank()
	d.field(tr("billing.autoReload.thresholdLabel"), &l.threshold, l.row == 0, func() tea.Cmd { l.row = 0; return nil })
	d.field(tr("billing.autoReload.reloadToLabel"), &l.reloadTo, l.row == 1, func() tea.Cmd { l.row = 1; return nil })
	d.blank()
	d.line(tr("billing.autoReload.authorization", chargeName), d.c.Muted, false)
	if l.problem != "" {
		d.line(l.problem, d.c.Error, false)
	}
	d.blank()
	for i, action := range f.autoReloadActions() {
		label, tone := tr("billing.common.cancel"), d.c.Text
		switch action {
		case "turnOn":
			label, tone = tr("billing.autoReload.agreeAndTurnOn"), d.c.Ok
		case "turnOff":
			label, tone = tr("billing.autoReload.turnOff"), d.c.Warn
		case "manageCard":
			label, tone = tr("billing.autoReload.manageCardOnPortal"), d.c.Accent
		}
		d.action(label, tone, l.row >= 2 && l.row-2 == i, func() tea.Cmd { return f.chooseAutoReload(action) })
	}
	d.blank()
	d.footer(tr("billing.autoReload.hint"))
}

// Runs a monthly limit row, the portal row closing once the portal opened and going back without a portal
func (f *billingFlow) chooseLimit(i int) tea.Cmd {
	if portal := paramStr(f.state["portal_url"]); i == 0 && portal != "" {
		f.t.openPortal(portal)
		return f.close()
	}
	return f.back()
}

func (f *billingFlow) drawLimit(d *sheetDraw) {
	usage := tr("billing.limit.noCapVisible")
	if limit, _ := f.state["monthly_cap"].(map[string]any); limit != nil && limit["limit_usd"] != nil {
		key := "billing.limit.usage"
		if truthyJS(limit["is_default_ceiling"]) {
			key = "billing.limit.usageDefaultCeiling"
		}
		usage = tr(key, jsTemplate(limit, "spent_display"), jsTemplate(limit, "limit_display"))
	}
	d.line(tr("billing.limit.title"), d.c.Accent, true)
	d.line(usage, d.c.Text, false)
	d.line(tr("billing.limit.readOnly"), d.c.Muted, false)
	d.blank()
	d.menu([]string{tr("billing.common.manageOnPortal"), tr("billing.common.cancel")}, shownRow(f.local.sel, 2), f.chooseLimit)
	d.blank()
	d.footer(tr("billing.limit.hint", float64(2)))
}

// Turns keep going on credits on after the confirm, the cancel row going back
func (f *billingFlow) chooseKeepGoing(i int) tea.Cmd {
	if i != 0 {
		return f.back()
	}
	return tea.Batch(f.applyKeepGoing(true), f.close())
}

func (f *billingFlow) drawKeepGoing(d *sheetDraw) {
	d.line(tr("billing.keepGoing.title"), d.c.Accent, true)
	d.line(tr("billing.keepGoing.body"), d.c.Text, false)
	d.line(tr("billing.keepGoing.offNote"), d.c.Muted, false)
	d.blank()
	d.menu([]string{tr("billing.keepGoing.turnOn"), tr("billing.common.cancel")}, shownRow(f.local.sel, 2), f.chooseKeepGoing)
	d.blank()
	d.footer(tr("billing.buy.hint", float64(2)))
}

// Command that shows the balance and opens the billing window on it
func (a *app) billingCommands() []slashCommand {
	return []slashCommand{{name: "topup", help: tr("slashCmd.core.help.topup"), run: func(r *slashRun) tea.Cmd {
		return r.rpc("billing.state", map[string]any{}, func(s map[string]any) tea.Cmd {
			if !truthyJS(s["logged_in"]) {
				r.sys(tr("slashCmd.topup.notLoggedIn"))
				return nil
			}
			t := &topup{a: a, r: r, first: s}
			f := &billingFlow{t: t, state: s}
			f.open("overview")
			t.card = newSheetCard(a, "tui:billing", f)
			return a.wm.ShowOverlay(t.card)
		})
	}}}
}
