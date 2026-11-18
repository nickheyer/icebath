package tui

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"
)

// Cadence and reads of the check that an upgrade landed on the plan
const (
	upgradeConfirmEvery    = 2 * time.Second
	upgradeConfirmAttempts = 15
)

// One /subscription run, the state it opened with, its window and the requests behind every screen
type planRun struct {
	a     *app
	r     *slashRun
	first map[string]any
	card  *sheetCard
}

func (p *planRun) sys(text string) { p.a.sys(text) }

// Gateway request answering nil on any failure, the reference screens deciding what nil means
func (p *planRun) call(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(p.a.gw, method, params, func(v any, err error) tea.Cmd {
		if err != nil {
			return then(nil)
		}
		return then(asRPCResult(v))
	})
}

// Card a subscription charge would use, nil when billing.state cannot say
func (p *planRun) fetchCard(then func(map[string]any) tea.Cmd) tea.Cmd {
	return p.call("billing.state", map[string]any{}, func(r map[string]any) tea.Cmd {
		card, _ := r["card"].(map[string]any)
		if !truthyJS(r["ok"]) {
			card = nil
		}
		return then(card)
	})
}

// Manage subscription page on the portal origin, pinned to the org and the plan, empty without a usable portal
func manageSubscriptionURL(s map[string]any, tier string) string {
	portal, _ := s["portal_url"].(string)
	u := parseSafeURL(portal)
	if portal == "" || u == nil {
		return ""
	}
	host := u.Host
	if port := u.Port(); u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		host = strings.TrimSuffix(host, ":"+port)
	}
	var query []string
	if org := s["org_id"]; truthyJS(org) {
		query = append(query, "org_id="+url.QueryEscape(jsValueString(org)))
	}
	if tier != "" {
		query = append(query, "plan="+url.QueryEscape(tier))
	}
	link := u.Scheme + "://" + host + "/manage-subscription"
	if len(query) > 0 {
		link += "?" + strings.Join(query, "&")
	}
	return link
}

// Opens the manage subscription page, pinned to a plan when one is named, and says where it went
func (p *planRun) openManageLink(tier string) {
	link := manageSubscriptionURL(p.first, tier)
	switch {
	case link == "":
		p.sys(tr("slashCmd.subscription.manageUrlFailed"))
	case openExternalURL(link):
		p.sys(tr("slashCmd.subscription.openingManage"))
	default:
		p.sys(tr("slashCmd.subscription.openBrowserFailedManage", link))
	}
}

// Opens a portal page in the browser, the link printed when no browser opened
func (p *planRun) openPortal(link string) {
	if openExternalURL(link) {
		p.sys(tr("slashCmd.subscription.openingPortal"))
		return
	}
	p.sys(tr("slashCmd.subscription.openBrowserFailedPortal", link))
}

// Outcome of the Remote Spending step up, the typed denial and its message when not granted
type stepUpAnswer struct {
	granted bool
	code    string
	message string
}

// Runs the Remote Spending device flow, an unreachable billing service answering as a denial
func (p *planRun) requestRemoteSpending(then func(stepUpAnswer) tea.Cmd) tea.Cmd {
	return gatewayCall(p.a.gw, "billing.step_up", stepUpParams(p.r.sid), func(v any, err error) tea.Cmd {
		if err != nil {
			return then(stepUpAnswer{message: tr("slashCmd.subscription.billingUnreachable")})
		}
		r := asRPCResult(v)
		return then(stepUpAnswer{granted: truthyJS(r["ok"]) && truthyJS(r["granted"]), code: paramStr(r["error"]), message: paramStr(r["message"])})
	})
}

// Change the confirm screen shows and applies, its kind, quote, target plan and the upgrade idempotency key
type planChange struct {
	kind    string
	preview map[string]any
	tier    string
	key     string
}

// Outcome the result screen shows, the portal page that finishes it and the plan an upgrade waits to land on
type planResult struct {
	message  string
	ok       bool
	recovery string
	tier     string
}

// Request the step up replays once Remote Spending is allowed, a plan quote, the confirmed change or a resume
type planRetry struct{ kind, tier string }

// Whether an answer is the missing Remote Spending refusal the step up resolves
func isScopeDenial(r map[string]any) bool {
	return r != nil && !truthyJS(r["ok"]) && r["error"] == "insufficient_scope"
}

// First non empty text among the values, the fallback when none has any
func firstText(fallback string, values ...any) string {
	for _, v := range values {
		if text, _ := v.(string); text != "" {
			return text
		}
	}
	return fallback
}

// Failed answer as a result with its portal recovery
func errorResult(r map[string]any) *planResult {
	return &planResult{message: firstText(tr("subscription.result.genericError"), r["message"], r["error"]), recovery: paramStr(r["portal_url"])}
}

// Chargeless schedule, cancel or resume answer as a result
func mutationResult(r map[string]any, done string) *planResult {
	if truthyJS(r["ok"]) {
		return &planResult{message: firstText(done, r["message"]), ok: true}
	}
	return errorResult(r)
}

// Upgrade answer as a result, card verification and declines handed to the portal and an unknown outcome never retried blind
func upgradeResult(r map[string]any, tier string) *planResult {
	recovery := paramStr(r["recovery_url"])
	switch {
	case r == nil:
		return &planResult{message: tr("subscription.result.upgradeUnconfirmed")}
	case r["reason"] == "authentication_required", r["reason"] == "subscription_payment_intent_requires_action":
		return &planResult{message: tr("subscription.result.verifyCard"), recovery: recovery}
	case r["reason"] == "card_declined":
		return &planResult{message: tr("subscription.result.cardDeclinedTryDifferent"), recovery: recovery}
	case truthyJS(r["ok"]) && r["status"] == "already_on_tier":
		return &planResult{message: tr("subscription.result.alreadyOn", nullishText(r, "target_tier_name", tr("subscription.shared.thisPlan"))), ok: true}
	case truthyJS(r["ok"]) && r["status"] == "upgraded":
		return &planResult{message: tr("subscription.result.upgraded", nullishText(r, "target_tier_name", tr("subscription.shared.yourNewPlan"))), ok: true, tier: tier}
	case r["status"] == "requires_action":
		return &planResult{message: tr("subscription.result.requiresAction"), recovery: recovery}
	case r["status"] == "payment_failed":
		return &planResult{message: tr("subscription.result.paymentFailed"), recovery: recovery}
	}
	return errorResult(r)
}

// Failed step up as a result with the recovery its typed denial needs
func stepUpDenialResult(res stepUpAnswer) *planResult {
	switch res.code {
	case "session_revoked":
		return &planResult{message: tr("subscription.stepUpDenial.sessionRevoked")}
	case "remote_spending_revoked":
		return &planResult{message: firstText(tr("subscription.stepUpDenial.remoteSpendingRevoked"), res.message)}
	case "rate_limited":
		return &planResult{message: tr("subscription.stepUpDenial.rateLimited")}
	}
	return &planResult{message: firstText(tr("subscription.stepUpDenial.notAllowed"), res.message)}
}

// Result of a replay refused again after the grant, shown instead of a second step up
func scopeStillDenied() *planResult {
	return &planResult{message: tr("subscription.result.scopeStillDenied")}
}

// Date part of an ISO time, the end of the billing period when there is none
func shortDate(iso any) string {
	if text, _ := iso.(string); len(text) >= 10 {
		return text[:10]
	}
	return tr("subscription.shared.periodEndFallback")
}

// Upfront charge of a quote in dollars, empty when the quote names none
func centsDisplay(cents any) string {
	if n, isNumber := cents.(float64); isNumber {
		return "$" + strconv.FormatFloat(n/100, 'f', 2, 64)
	}
	return ""
}

// Scheduled cancellation or downgrade of the plan, where it goes and when, false when nothing is scheduled
func pendingTransition(current map[string]any) (string, string, bool) {
	switch {
	case current == nil:
		return "", "", false
	case truthyJS(current["cancel_at_period_end"]):
		return tr("subscription.status.cancels"), nullishText(current, "cancellation_effective_display", shortDate(current["cancellation_effective_at"])), true
	case truthyJS(current["pending_downgrade_tier_name"]):
		return jsValueString(current["pending_downgrade_tier_name"]), nullishText(current, "pending_downgrade_display", shortDate(current["pending_downgrade_at"])), true
	}
	return "", "", false
}

// Plan status line in dollars, echoing a scheduled change and what is left
func planStatusLine(s map[string]any) string {
	usage, _ := s["usage"].(map[string]any)
	current, _ := s["current"].(map[string]any)
	plan := nullishText(current, "tier_name", nullishText(usage, "plan_name", ""))
	if plan == "" {
		return tr("subscription.status.freePlan")
	}
	flip := ""
	if to, _, scheduled := pendingTransition(current); scheduled {
		flip = tr("subscription.status.transition", to)
	}
	left := ""
	if truthyJS(usage["total_spendable_display"]) {
		left = tr("subscription.status.left", jsValueString(usage["total_spendable_display"]))
	}
	if usage["status"] == "low" && left != "" {
		return tr("subscription.status.plan", plan) + flip + left
	}
	tail := tr("subscription.status.viewOnly")
	if truthyJS(s["can_change_plan"]) {
		tail = ""
		if truthyJS(usage["renews_display"]) {
			tail = tr("subscription.status.renews", jsValueString(usage["renews_display"]))
		}
	}
	return tr("subscription.status.plan", plan) + flip + left + tail
}

// Enabled paid tiers of the catalog by tier order, the current one kept only when asked
func planTiers(s map[string]any, withCurrent bool) []map[string]any {
	var out []map[string]any
	rows, _ := s["tiers"].([]any)
	for _, raw := range rows {
		tier, _ := raw.(map[string]any)
		if tier != nil && truthyJS(tier["is_enabled"]) && jsNumberOf(tier["tier_order"]) > 0 && (withCurrent || !truthyJS(tier["is_current"])) {
			out = append(out, tier)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return jsNumberOf(out[i]["tier_order"]) < jsNumberOf(out[j]["tier_order"]) })
	return out
}

// One row of a subscription screen, its label, tone and what it runs
type planRow struct {
	label string
	tone  string
	run   func() tea.Cmd
}

// What the open subscription screen holds, reset each time another screen opens
type subscriptionScreen struct {
	sel        int
	busy       bool
	submitting bool
	chargeCard string
	phase      string
	applied    string
	attempts   int
}

// Screens of the subscription window, overview, picker, confirm, result, step up and the team view
type subscriptionFlow struct {
	p       *planRun
	state   map[string]any
	screen  string
	pending *planChange
	result  *planResult
	retry   *planRetry
	visit   int
	local   subscriptionScreen
}

func (f *subscriptionFlow) title() string  { return tr("subscription.title") }
func (f *subscriptionFlow) close() tea.Cmd { return f.p.card.close() }

// Shows a screen with fresh state and starts what it loads, the card a charge uses or the upgrade check
func (f *subscriptionFlow) open(screen string) tea.Cmd {
	f.screen, f.visit, f.local = screen, f.visit+1, subscriptionScreen{phase: "prompt", applied: "confirmed"}
	if f.p.card.closed {
		return nil
	}
	visit := f.visit
	switch {
	case screen == "confirm" && f.effect() == "charge_now" && f.pending.kind != "cancellation":
		return f.p.fetchCard(func(card map[string]any) tea.Cmd {
			if f.visit == visit && card != nil && (card["resolved_via"] == "subPin" || card["resolved_via"] == "customerDefault") {
				f.local.chargeCard = jsValueString(card["masked"])
			}
			return nil
		})
	case screen == "result" && f.result != nil && f.result.tier != "":
		f.local.applied = "applying"
		return f.confirmUpgrade(visit, f.result.tier)
	}
	return nil
}

// Reads the plan again every two seconds until the upgrade landed or the reads ran out
func (f *subscriptionFlow) confirmUpgrade(visit int, tier string) tea.Cmd {
	return tea.Tick(upgradeConfirmEvery, func(time.Time) tea.Msg {
		return asyncDoneMsg{run: func() tea.Cmd {
			if f.visit != visit || f.p.card.closed {
				return nil
			}
			f.local.attempts++
			return f.p.call("subscription.state", map[string]any{}, func(fresh map[string]any) tea.Cmd {
				if f.visit != visit || f.p.card.closed {
					return nil
				}
				current, _ := fresh["current"].(map[string]any)
				switch {
				case current["tier_id"] == tier:
					f.local.applied = "confirmed"
				case f.local.attempts >= upgradeConfirmAttempts:
					f.local.applied = "timed_out"
				default:
					return f.confirmUpgrade(visit, tier)
				}
				return nil
			})
		}}
	})
}

// Shows an outcome on the result screen
func (f *subscriptionFlow) toResult(r *planResult) tea.Cmd {
	f.result = r
	return f.open("result")
}

// Opens the step up holding the request it replays, or a result when this is already the replay
func (f *subscriptionFlow) toStepUp(retry *planRetry, allowStepUp bool) tea.Cmd {
	if !allowStepUp {
		return f.toResult(scopeStillDenied())
	}
	f.retry = retry
	return f.open("stepup")
}

// Quotes a plan change and shows it on the confirm screen, the step up or the failure
func (f *subscriptionFlow) previewAndRoute(tier string, allowStepUp bool) tea.Cmd {
	return f.p.call("subscription.preview", map[string]any{"subscription_type_id": tier}, func(quote map[string]any) tea.Cmd {
		switch {
		case quote == nil:
			return f.toResult(&planResult{message: tr("subscription.result.couldNotPreview")})
		case isScopeDenial(quote):
			return f.toStepUp(&planRetry{kind: "preview", tier: tier}, allowStepUp)
		case !truthyJS(quote["ok"]):
			return f.toResult(errorResult(quote))
		}
		f.pending = &planChange{kind: "tier_change", preview: quote, tier: tier}
		if quote["effect"] == "charge_now" {
			f.pending.kind, f.pending.key = "upgrade", uuid.NewString()
		}
		return f.open("confirm")
	})
}

// Applies the confirmed change, a cancellation, an upgrade that charges now or a scheduled switch
func (f *subscriptionFlow) applyPendingAndRoute(pending *planChange, allowStepUp bool) tea.Cmd {
	if pending == nil {
		return f.open("overview")
	}
	route := func(result func(map[string]any) *planResult) func(map[string]any) tea.Cmd {
		return func(r map[string]any) tea.Cmd {
			if isScopeDenial(r) {
				return f.toStepUp(&planRetry{kind: "apply"}, allowStepUp)
			}
			return f.toResult(result(r))
		}
	}
	switch pending.kind {
	case "cancellation":
		return f.p.call("subscription.change", map[string]any{"cancel": true}, route(func(r map[string]any) *planResult {
			return mutationResult(r, tr("subscription.result.cancellationScheduled"))
		}))
	case "upgrade":
		params := map[string]any{"subscription_type_id": pending.tier}
		if pending.key != "" {
			params["idempotency_key"] = pending.key
		}
		return f.p.call("subscription.upgrade", params, route(func(r map[string]any) *planResult { return upgradeResult(r, pending.tier) }))
	}
	return f.p.call("subscription.change", map[string]any{"subscription_type_id": pending.tier}, route(func(r map[string]any) *planResult {
		return mutationResult(r, tr("subscription.result.changeScheduled"))
	}))
}

// Undoes the scheduled change and shows the outcome or the step up
func (f *subscriptionFlow) resumeAndRoute(allowStepUp bool) tea.Cmd {
	return f.p.call("subscription.resume", map[string]any{}, func(r map[string]any) tea.Cmd {
		if isScopeDenial(r) {
			return f.toStepUp(&planRetry{kind: "resume"}, allowStepUp)
		}
		return f.toResult(mutationResult(r, tr("subscription.result.resumed")))
	})
}

// Effect of the change on the confirm screen, scheduled for a cancellation and blocked without a quote
func (f *subscriptionFlow) effect() string {
	if f.pending != nil && f.pending.kind == "cancellation" {
		return "scheduled"
	}
	if f.pending != nil && f.pending.preview["effect"] != nil {
		return jsValueString(f.pending.preview["effect"])
	}
	return "blocked"
}

// Whether the run is a team terminal, whose shared balance has no personal plan to change
func (f *subscriptionFlow) team() bool { return f.state["context"] == "team" }

func (f *subscriptionFlow) draw(d *sheetDraw) {
	if f.team() {
		f.drawTeam(d)
		return
	}
	switch f.screen {
	case "overview":
		f.drawOverview(d)
	case "picker":
		f.drawPicker(d)
	case "confirm":
		f.drawConfirm(d)
	case "result":
		f.drawResult(d)
	case "stepup":
		f.drawStepUp(d)
	}
}

func (f *subscriptionFlow) key(id, text string) tea.Cmd {
	if f.team() {
		if id == "esc" || id == "enter" {
			return f.close()
		}
		return nil
	}
	var rows []planRow
	back := f.close
	switch f.screen {
	case "overview":
		rows = f.overviewRows()
	case "picker":
		rows, back = f.pickerRows(), f.toOverview
	case "confirm":
		rows, back = f.confirmRows(), f.confirmBack
	case "result":
		rows = f.resultRows()
	case "stepup":
		rows, back = f.stepUpRows(), f.stepUpBack
	}
	return menuKey(&f.local.sel, len(rows), id, text, func(i int) tea.Cmd { return rows[i].run() }, back)
}

// Back on the overview
func (f *subscriptionFlow) toOverview() tea.Cmd { return f.open("overview") }

// Overview rows, undo or change for a plan admin, the paid plans on free, the portal and close
func (f *subscriptionFlow) overviewRows() []planRow {
	s := f.state
	current, _ := s["current"].(map[string]any)
	free := !truthyJS(current["tier_id"])
	name := nullishText(current, "tier_name", tr("subscription.shared.yourPlan"))
	_, _, scheduled := pendingTransition(current)
	var rows []planRow
	change := planRow{label: tr("subscription.overview.changePlan"), run: func() tea.Cmd {
		f.pending = nil
		return f.open("picker")
	}}
	if truthyJS(s["can_change_plan"]) && !free {
		if scheduled {
			rows = append(rows, planRow{label: tr("subscription.overview.keepPlanUndo", name), run: func() tea.Cmd {
				if f.local.busy {
					return nil
				}
				f.local.busy = true
				return f.resumeAndRoute(true)
			}}, change)
		} else {
			rows = append(rows, change, planRow{label: tr("subscription.overview.cancelSubscription"), run: func() tea.Cmd {
				f.pending = &planChange{kind: "cancellation"}
				return f.open("confirm")
			}})
		}
	}
	var plans []map[string]any
	if free {
		plans = planTiers(s, true)
	}
	for _, tier := range plans {
		suffix := ""
		if credits := jsNumberOf(strings.ReplaceAll(paramStr(tier["monthly_credits"]), ",", "")); credits > 0 && credits-credits == 0 {
			suffix = tr("subscription.overview.creditsPerMonth", enUSNumber(credits))
		}
		id := jsValueString(tier["tier_id"])
		rows = append(rows, planRow{label: tr("subscription.overview.tierRow", jsValueString(tier["name"]), jsValueString(tier["dollars_per_month_display"])) + suffix, run: func() tea.Cmd {
			if f.local.busy {
				return nil
			}
			f.local.busy = true
			f.p.openManageLink(id)
			return f.close()
		}})
	}
	if !free || len(plans) == 0 {
		label := tr("subscription.shared.manageOnPortal")
		if free {
			label = tr("subscription.overview.startSubscription")
		}
		rows = append(rows, planRow{label: label, run: func() tea.Cmd {
			if truthyJS(s["portal_url"]) {
				f.p.openManageLink("")
			} else {
				f.p.sys(tr("subscription.overview.noPortalUrl"))
			}
			return f.close()
		}})
	}
	return append(rows, planRow{label: tr("subscription.shared.close"), run: f.close})
}

// Menu rows numbered as the reference menu draws them, their tones left to the action rows
func (d *sheetDraw) planMenu(rows []planRow, sel int) {
	labels := make([]string, len(rows))
	for i, row := range rows {
		labels[i] = row.label
	}
	d.menu(labels, shownRow(sel, len(rows)), func(i int) tea.Cmd { return rows[i].run() })
}

// Unnumbered action rows in their own tones
func (d *sheetDraw) planActions(rows []planRow, sel int) {
	sel = shownRow(sel, len(rows))
	for i, row := range rows {
		d.action(row.label, row.tone, i == sel, row.run)
	}
}

func (f *subscriptionFlow) drawOverview(d *sheetDraw) {
	s := f.state
	current, _ := s["current"].(map[string]any)
	usage, _ := s["usage"].(map[string]any)
	name := nullishText(current, "tier_name", tr("subscription.shared.yourPlan"))
	if to, when, scheduled := pendingTransition(current); scheduled {
		d.line(tr("subscription.overview.scheduledChange"), d.c.Warn, true)
		d.styled(toneStyle(d.c.Text).Render(name+" ") + toneStyle(d.c.Warn).Render("──▶ ") + toneStyle(d.c.Text).Render(to) + toneStyle(d.c.Muted).Render(" · "+when))
		d.line(tr("subscription.overview.keepUntilThen", name), d.c.Muted, false)
		d.blank()
	}
	d.line(planStatusLine(s), d.c.Accent, true)
	d.usageBars(s["usage"])
	if !truthyJS(current["tier_id"]) {
		d.blank()
		d.line("> "+tr("subscription.overview.freeNudge"), d.c.Warn, false)
	}
	if usage["status"] == "low" {
		d.blank()
		d.line("! "+tr("subscription.overview.lowNudge", nullishText(usage, "total_spendable_display", tr("subscription.overview.lowNudgeAmountFallback"))), d.c.Warn, false)
	}
	if truthyJS(s["org_name"]) {
		d.line(orgLine(s, "subscription.overview.org"), d.c.Muted, false)
	}
	d.blank()
	d.planMenu(f.overviewRows(), f.local.sel)
	d.blank()
	d.footer(tr("subscription.overview.hint"))
}

// Plans the current one can switch to by tier order with their direction, then back
func (f *subscriptionFlow) pickerRows() []planRow {
	order := 0.0
	rows, _ := f.state["tiers"].([]any)
	for _, raw := range rows {
		if tier, _ := raw.(map[string]any); truthyJS(tier["is_current"]) {
			order = jsNumberOf(tier["tier_order"])
			break
		}
	}
	var out []planRow
	for _, tier := range planTiers(f.state, false) {
		direction := tr("subscription.picker.downgrade")
		if jsNumberOf(tier["tier_order"]) > order {
			direction = tr("subscription.picker.upgrade")
		}
		id := jsValueString(tier["tier_id"])
		out = append(out, planRow{label: tr("subscription.picker.tierRow", jsValueString(tier["name"]), jsValueString(tier["dollars_per_month_display"]), direction), run: func() tea.Cmd {
			if f.local.busy {
				return nil
			}
			f.local.busy = true
			return f.previewAndRoute(id, true)
		}})
	}
	return append(out, planRow{label: tr("subscription.shared.back"), run: f.toOverview})
}

func (f *subscriptionFlow) drawPicker(d *sheetDraw) {
	current, _ := f.state["current"].(map[string]any)
	rows := f.pickerRows()
	d.line(tr("subscription.picker.title"), d.c.Accent, true)
	d.line(tr("subscription.picker.current", nullishText(current, "tier_name", tr("subscription.shared.freePlan"))), d.c.Muted, false)
	d.blank()
	if len(rows) == 1 {
		d.line(tr("subscription.picker.noOtherPlans"), d.c.Muted, false)
	}
	d.planMenu(rows, f.local.sel)
	d.blank()
	d.footer(tr("subscription.picker.hint"))
}

// Leaves the confirm screen unless the change is being applied, a cancellation going to the overview
func (f *subscriptionFlow) confirmBack() tea.Cmd {
	if f.local.submitting {
		return nil
	}
	cancellation := f.pending != nil && f.pending.kind == "cancellation"
	f.pending = nil
	if cancellation {
		return f.open("overview")
	}
	return f.open("picker")
}

// Primary row of the confirm screen by effect, then back
func (f *subscriptionFlow) confirmRows() []planRow {
	c := f.p.a.store.ui.Theme.Color
	apply := func() tea.Cmd {
		if f.local.submitting || f.pending == nil {
			return nil
		}
		f.local.submitting = true
		return f.applyPendingAndRoute(f.pending, true)
	}
	var rows []planRow
	switch effect := f.effect(); {
	case f.pending != nil && f.pending.kind == "cancellation":
		rows = append(rows, planRow{label: tr("subscription.overview.cancelSubscription"), tone: c.Warn, run: apply})
	case effect == "charge_now":
		label := tr("subscription.confirm.upgradeNowProrated")
		if amount := centsDisplay(f.pending.preview["amount_due_now_cents"]); amount != "" {
			label = tr("subscription.confirm.payAndUpgrade", amount)
		}
		rows = append(rows, planRow{label: label, tone: c.Ok, run: apply})
	case effect == "scheduled":
		rows = append(rows, planRow{label: tr("subscription.confirm.scheduleChangeTo", f.targetName()), tone: c.Ok, run: apply})
	case effect == "blocked":
		rows = append(rows, planRow{label: tr("subscription.shared.manageOnPortal"), run: func() tea.Cmd {
			f.p.openManageLink("")
			return f.close()
		}})
	}
	return append(rows, planRow{label: tr("subscription.shared.back"), run: f.confirmBack})
}

// Plan the quote switches to, the selected plan when the quote names none
func (f *subscriptionFlow) targetName() string {
	var preview map[string]any
	if f.pending != nil {
		preview = f.pending.preview
	}
	return nullishText(preview, "target_tier_name", tr("subscription.shared.theSelectedPlan"))
}

func (f *subscriptionFlow) drawConfirm(d *sheetDraw) {
	var preview map[string]any
	if f.pending != nil {
		preview = f.pending.preview
	}
	cancellation := f.pending != nil && f.pending.kind == "cancellation"
	effect := f.effect()
	heading := tr("subscription.confirm.titleChange")
	if cancellation {
		heading = tr("subscription.confirm.titleCancellation")
	}
	title := toneStyle(d.c.Accent).Bold(true).Render(heading)
	switch effect {
	case "charge_now":
		title += toneStyle(d.c.Ok).Render(" · " + tr("subscription.confirm.chipChargedNow"))
	case "scheduled":
		title += toneStyle(d.c.Warn).Render(" · " + tr("subscription.confirm.chipScheduled"))
	}
	d.styled(title)
	if f.local.submitting {
		d.line(tr("subscription.confirm.working"), d.c.Muted, false)
	}
	delta := func() {
		if truthyJS(preview["monthly_credits_delta"]) {
			d.line(tr("subscription.confirm.creditsDelta", jsValueString(preview["monthly_credits_delta"])), d.c.Muted, false)
		}
	}
	current, _ := f.state["current"].(map[string]any)
	switch {
	case cancellation:
		d.line(tr("subscription.confirm.cancelBody", nullishText(current, "tier_name", tr("subscription.shared.yourPlan")), shortDate(current["cycle_ends_at"])), d.c.Text, false)
		d.line(tr("subscription.confirm.cancelKeepCredits"), d.c.Muted, false)
	case effect == "charge_now":
		charge := tr("subscription.confirm.chargedProrated")
		if amount := centsDisplay(preview["amount_due_now_cents"]); amount != "" {
			charge = tr("subscription.confirm.chargedNow", amount)
		}
		d.line(tr("subscription.confirm.upgradeTo", f.targetName())+" "+charge, d.c.Text, false)
		delta()
		card := tr("subscription.confirm.cardChargedGeneric")
		if f.local.chargeCard != "" {
			card = tr("subscription.confirm.cardCharged", f.local.chargeCard)
		}
		d.line(card, d.c.Muted, false)
	case effect == "scheduled":
		d.line(tr("subscription.confirm.scheduledBody", f.targetName(), shortDate(preview["effective_at"])), d.c.Text, false)
		delta()
	case effect == "no_op":
		d.line(tr("subscription.confirm.noOp", f.targetName()), d.c.Muted, false)
	case effect == "blocked":
		d.line(nullishText(preview, "reason", tr("subscription.confirm.blockedFallback")), d.c.Warn, false)
	}
	d.blank()
	d.planActions(f.confirmRows(), f.local.sel)
	d.blank()
	d.footer(tr("subscription.confirm.hint"))
}

// Portal row when the outcome needs the portal to finish, then close
func (f *subscriptionFlow) resultRows() []planRow {
	closeRow := planRow{label: tr("subscription.shared.close"), run: f.close}
	if f.result == nil || f.result.recovery == "" {
		return []planRow{closeRow}
	}
	recovery := f.result.recovery
	return []planRow{{label: tr("subscription.resultScreen.openPortalToFinish"), tone: f.p.a.store.ui.Theme.Color.Accent, run: func() tea.Cmd {
		f.p.openPortal(recovery)
		return f.close()
	}}, closeRow}
}

func (f *subscriptionFlow) drawResult(d *sheetDraw) {
	r := f.result
	if r == nil {
		r = &planResult{}
	}
	applying := r.ok && f.local.applied == "applying"
	timedOut := r.ok && f.local.applied == "timed_out"
	heading, tone, message := tr("subscription.resultScreen.titleFailed"), d.c.Warn, r.message
	switch {
	case applying:
		heading = tr("subscription.resultScreen.titleApplying")
	case timedOut:
		heading, message = tr("subscription.resultScreen.titleStillApplying"), tr("subscription.resultScreen.stillApplying")
	case r.ok:
		heading = tr("subscription.resultScreen.titleDone")
	}
	if r.ok {
		tone = d.c.Ok
	}
	d.line(heading, tone, true)
	d.line(message, d.c.Text, false)
	if r.ok && !applying && !timedOut {
		d.line(tr("subscription.resultScreen.rerunHint"), d.c.Muted, false)
	}
	d.blank()
	d.planActions(f.resultRows(), f.local.sel)
	d.blank()
	d.footer(tr("subscription.resultScreen.hint"))
}

// Starts the Remote Spending device flow, holding the change until the user continues after the grant
func (f *subscriptionFlow) enableRemoteSpending() tea.Cmd {
	if f.local.phase != "prompt" {
		return nil
	}
	f.local.phase = "waiting"
	visit := f.visit
	return f.p.requestRemoteSpending(func(res stepUpAnswer) tea.Cmd {
		if f.visit != visit {
			return nil
		}
		if res.granted {
			f.local.phase = "granted"
			return nil
		}
		f.retry = nil
		return f.toResult(stepUpDenialResult(res))
	})
}

// Replays the held request once after the grant, a repeated refusal showing a result instead of another step up
func (f *subscriptionFlow) replay() tea.Cmd {
	if f.local.phase != "granted" {
		return nil
	}
	f.local.phase = "resuming"
	retry := f.retry
	f.retry = nil
	switch {
	case retry == nil:
		return f.open("overview")
	case retry.kind == "preview":
		return f.previewAndRoute(retry.tier, false)
	case retry.kind == "resume":
		return f.resumeAndRoute(false)
	}
	return f.applyPendingAndRoute(f.pending, false)
}

// Abandons the step up unless the replay already runs, a late grant then charging nothing
func (f *subscriptionFlow) stepUpBack() tea.Cmd {
	if f.local.phase == "resuming" {
		return nil
	}
	target := "overview"
	if f.retry != nil && f.retry.kind == "apply" {
		target = "confirm"
	}
	f.retry = nil
	return f.open(target)
}

// Rows of the step up, allow before the browser flow and continue after the grant
func (f *subscriptionFlow) stepUpRows() []planRow {
	c := f.p.a.store.ui.Theme.Color
	cancel := planRow{label: tr("subscription.shared.cancel"), run: f.stepUpBack}
	switch f.local.phase {
	case "granted":
		label := tr("subscription.stepUp.continue")
		if f.retry != nil && f.retry.kind == "apply" {
			label = tr("subscription.stepUp.continueChange")
		}
		return []planRow{{label: label, tone: c.Ok, run: f.replay}, cancel}
	case "prompt":
		return []planRow{{label: tr("subscription.stepUp.allow"), tone: c.Ok, run: f.enableRemoteSpending}, cancel}
	}
	return nil
}

func (f *subscriptionFlow) drawStepUp(d *sheetDraw) {
	d.line(tr("subscription.stepUp.title"), d.c.Accent, true)
	footer := tr("subscription.stepUp.hint")
	switch f.local.phase {
	case "prompt":
		d.line(tr("subscription.stepUp.promptBody"), d.c.Text, false)
		d.line(tr("subscription.stepUp.promptNote"), d.c.Muted, false)
	case "waiting":
		d.line(tr("subscription.stepUp.waiting"), d.c.Muted, false)
		footer = tr("subscription.stepUp.hintWaiting")
	case "granted":
		d.line(tr("subscription.stepUp.granted"), d.c.Ok, false)
	case "resuming":
		d.line(tr("subscription.stepUp.resuming"), d.c.Muted, false)
		footer = tr("subscription.stepUp.hintWorking")
	}
	d.blank()
	d.planActions(f.stepUpRows(), f.local.sel)
	d.blank()
	d.footer(footer)
}

func (f *subscriptionFlow) drawTeam(d *sheetDraw) {
	s := f.state
	d.line(tr("subscription.team.title"), d.c.Accent, true)
	if truthyJS(s["org_name"]) {
		d.line(orgLine(s, "subscription.overview.org"), d.c.Muted, false)
	}
	d.blank()
	d.line(tr("subscription.team.body", nullishText(s, "org_name", tr("subscription.team.orgFallback"))), d.c.Text, false)
	d.line(tr("subscription.team.personalNote"), d.c.Muted, false)
	d.blank()
	d.footer(tr("subscription.team.hint"))
}

// Command that shows the plan and opens the subscription window on it, upgrade naming it too
func (a *app) subscriptionCommands() []slashCommand {
	return []slashCommand{{name: "subscription", aliases: []string{"upgrade"}, help: tr("slashCmd.core.help.subscription"), run: func(r *slashRun) tea.Cmd {
		return r.rpc("subscription.state", map[string]any{}, func(s map[string]any) tea.Cmd {
			if !truthyJS(s["logged_in"]) {
				r.sys(tr("slashCmd.subscription.notLoggedIn"))
				return nil
			}
			p := &planRun{a: a, r: r, first: s}
			f := &subscriptionFlow{p: p, state: s}
			p.card = newSheetCard(a, "tui:subscription", f)
			return tea.Batch(f.open("overview"), a.wm.ShowOverlay(p.card))
		})
	}}}
}
