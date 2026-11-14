package usage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/internal/model/portal"
	"github.com/nickheyer/icebath/internal/model/providers"
)

const (
	accountFetchTimeout = 10 * time.Second
	portalCreditsTitle  = "Icebath Portal credits"
	depletedLine        = "Status: access depleted — top up to restore"
	topTierUsedUp       = "Monthly large-model usage is used up."
	windowPausedLine    = "Status: included usage paused. Your subscription payment failed. Update your card to restore it. " +
		"Until then, icebath uses your credits."
	windowPausedOtherLine = "Status: included usage paused. See Billing in the Portal. Until then, icebath uses your credits."
	keepGoingOffLine      = "Keep going on credits: off. icebath waits for the reset (turn on in /topup)."
	keepGoingOnLine       = "Keep going on credits: on. Uses your credits when included usage runs out."
)

var errAccountTimeout = errors.New("usage: Icebath Portal account fetch timed out")

func finite(v *float64) (float64, bool) {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return 0, false
	}
	return *v, true
}

func (t *Tracker) portalLoggedIn(ctx context.Context) bool {
	raw, ok := t.portal.State(ctx)
	if !ok {
		return false
	}
	var state struct {
		AccessToken any `json:"access_token"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return false
	}
	token, isText := state.AccessToken.(string)
	return isText && strings.TrimSpace(token) != ""
}

func (t *Tracker) fetchAccount(ctx context.Context, timeout time.Duration) (*portal.AccountInfo, error) {
	done := make(chan *portal.AccountInfo, 1)
	if !t.spawn(ctx, func(bg context.Context) {
		done <- t.portal.AccountInfo(bg, portal.AccountOptions{ForceFresh: true})
	}) {
		return nil, errClosed
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case info := <-done:
		return info, nil
	case <-timer.C:
		return nil, errAccountTimeout
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func gaugeWindow(label string, usedBP int, resetsAtMS *int64) providers.UsageWindow {
	used := max(0.0, min(100.0, float64(usedBP)/portal.GaugeFull*100.0))
	w := providers.UsageWindow{Label: label, UsedPercent: &used, Scope: scopeAccount}
	if resetsAtMS != nil {
		w.ResetAt = time.UnixMilli(*resetsAtMS).UTC()
	}
	return w
}

func topTierLine(resetAt time.Time, keepGoing *bool) string {
	resets := ""
	if !resetAt.IsZero() {
		local := resetAt.Local()
		resets = " Resets " + local.Format("Jan") + " " + strconv.Itoa(local.Day()) + "."
	}
	tail := " Past this, large models use your credits."
	if isFalse(keepGoing) {
		tail = " Your other included usage is still available."
	}
	return topTierUsedUp + resets + tail
}

func usageWindowRows(gauges []portal.WindowGauge, pausedReason string, keepGoing *bool) ([]providers.UsageWindow, []string) {
	if len(gauges) == 0 || len(portal.AllScope(gauges)) == 0 {
		return nil, nil
	}
	var rows []providers.UsageWindow
	var topRow *providers.UsageWindow
	topFull := false
	for _, g := range gauges {
		rows = append(rows, gaugeWindow(portal.GaugeTitle(g), g.UsedBP, g.ResetsAtMS))
		if g.Scope == portal.ScopeTopTier {
			row := rows[len(rows)-1]
			topRow, topFull = &row, g.Full()
		}
	}
	var lines []string
	if topFull {
		var reset time.Time
		if topRow != nil {
			reset = topRow.ResetAt
		}
		lines = append(lines, topTierLine(reset, keepGoing))
	}
	switch {
	case pausedReason != "":
		if portal.PausedForPayment(pausedReason) {
			lines = append(lines, windowPausedLine)
		} else {
			lines = append(lines, windowPausedOtherLine)
		}
	case isFalse(keepGoing):
		lines = append(lines, keepGoingOffLine)
	case isTrue(keepGoing):
		lines = append(lines, keepGoingOnLine)
	}
	return rows, lines
}

func portalSnapshot(windows []providers.UsageWindow, details, tail []string, plan string, usageWindow bool) *providers.AccountUsage {
	if len(windows) == 0 && len(details) == 0 {
		return nil
	}
	snap := newSnapshot(providers.ProviderPortal, "portal-account", windows, append(details, tail...))
	snap.Title, snap.Plan, snap.UsageWindow = portalCreditsTitle, plan, usageWindow
	return snap
}

func portalCreditsSnapshot(info *portal.AccountInfo) *providers.AccountUsage {
	if info == nil || !info.LoggedIn {
		return nil
	}
	access, sub := info.PaidServiceAccessInfo, info.Subscription
	plan := ""
	if sub != nil {
		plan = sub.Plan
	}
	if window := info.UsageWindow; window != nil {
		if rows, lines := usageWindowRows(window.Gauges, window.PausedReason, window.KeepGoingOnCredits); len(rows) > 0 {
			details := append([]string(nil), lines...)
			if access != nil {
				if topup, ok := finite(access.PurchasedCreditsRemaining); ok {
					details = append(details, "Top-up credits: "+FormatUSD(topup))
				}
			}
			if isFalse(info.PaidServiceAccess) && len(lines) == 0 {
				details = append(details, depletedLine)
			}
			if sub != nil && sub.CurrentPeriodEnd != "" {
				renews := FormatRenews(sub.CurrentPeriodEnd)
				if renews == "" {
					renews = sub.CurrentPeriodEnd
				}
				details = append(details, "Renews: "+renews)
			}
			return portalSnapshot(rows, details, []string{"Top up via /topup"}, plan, true)
		}
	}
	var windows []providers.UsageWindow
	var details []string
	if sub != nil {
		capUSD, capOK := finite(sub.MonthlyCredits)
		remaining, remainingOK := finite(sub.CreditsRemaining)
		if capOK && capUSD > 0 && remainingOK && remaining <= capUSD {
			used := max(0.0, min(100.0, (capUSD-remaining)/capUSD*100.0))
			windows = append(windows, providers.UsageWindow{
				Label: "Subscription", UsedPercent: &used, Detail: FormatUSD(remaining) + " of " + FormatUSD(capUSD) + " left", Scope: scopeAccount,
			})
		}
	}
	if access != nil {
		for _, field := range []struct {
			value *float64
			label string
		}{
			{access.SubscriptionCreditsRemaining, "Subscription credits"},
			{access.PurchasedCreditsRemaining, "Top-up credits"},
			{access.TotalUsableCredits, "Total usable"},
		} {
			if value, ok := finite(field.value); ok {
				details = append(details, field.label+": "+FormatUSD(value))
			}
		}
	}
	if sub != nil {
		if rollover, ok := finite(sub.RolloverCredits); ok && rollover > 0 {
			details = append(details, "Rollover: "+FormatUSD(rollover))
		}
		if sub.CurrentPeriodEnd != "" {
			details = append(details, "Renews: "+sub.CurrentPeriodEnd)
		}
	}
	if isFalse(info.PaidServiceAccess) {
		details = append(details, depletedLine)
	}
	return portalSnapshot(windows, details, []string{"Top up: " + portal.TopupURL(info), "(or run /topup)"}, plan, false)
}

func headerGaugePercent(bp int) float64 {
	return max(0.0, min(100.0, float64(bp)/portal.GaugeFull*100.0))
}

func applyHeaderGauge(snap *providers.AccountUsage, s *CreditsState) *providers.AccountUsage {
	if snap == nil || !snap.UsageWindow || s == nil || !s.FromHeader || !s.IsUsageWindow() {
		return snap
	}
	type headerGauge struct {
		bp       int
		resetsAt *int64
	}
	byTitle := map[string]headerGauge{}
	for _, g := range s.Gauges {
		byTitle[portal.GaugeTitle(g)] = headerGauge{g.UsedBP, g.ResetsAtMS}
	}
	nowMS := time.Now().UnixMilli()
	windows := make([]providers.UsageWindow, len(snap.Windows))
	changed := false
	for i, w := range snap.Windows {
		windows[i] = w
		hg, ok := byTitle[w.Label]
		if !ok || w.UsedPercent == nil || (hg.resetsAt != nil && nowMS >= *hg.resetsAt) {
			continue
		}
		header := headerGaugePercent(hg.bp)
		if header <= *w.UsedPercent {
			continue
		}
		changed = true
		windows[i].UsedPercent = &header
	}
	if !changed {
		return snap
	}
	out := *snap
	out.Windows = windows
	out.Details = append([]string(nil), snap.Details...)
	if top, ok := s.TopTierGauge(); ok {
		title := portal.GaugeTitle(top)
		for _, w := range windows {
			if w.Label != title {
				continue
			}
			if w.UsedPercent != nil && *w.UsedPercent >= 100 {
				present := false
				for _, line := range out.Details {
					if strings.HasPrefix(line, topTierUsedUp) {
						present = true
						break
					}
				}
				if !present {
					out.Details = append(out.Details, topTierLine(w.ResetAt, s.KeepGoingOnCredits))
				}
			}
			break
		}
	}
	return &out
}

func (t *Tracker) CreditsLines(ctx context.Context, markdown bool, header *CreditsState) (lines []string, usageWindow bool) {
	ctx = t.bind(ctx)
	if !t.portalLoggedIn(ctx) {
		return nil, false
	}
	info, err := t.fetchAccount(ctx, accountFetchTimeout)
	if err != nil {
		return nil, false
	}
	snap := applyHeaderGauge(portalCreditsSnapshot(info), header)
	return RenderAccountUsageLines(snap, markdown), snap != nil && snap.UsageWindow
}

type CreditsFields struct {
	Lines       []string `json:"credits_lines,omitempty"`
	UsageWindow bool     `json:"credits_usage_window,omitempty"`
}

func (t *Tracker) CreditsUsageFields(ctx context.Context, s *CreditsState) CreditsFields {
	lines, window := t.CreditsLines(ctx, false, s)
	if len(lines) == 0 {
		return CreditsFields{}
	}
	return CreditsFields{Lines: lines, UsageWindow: window}
}

type CreditsView struct {
	LoggedIn     bool
	BalanceLines []string
	IdentityLine string
	TopupURL     string
	Depleted     bool
}

func (t *Tracker) CreditsView(ctx context.Context, markdown bool) CreditsView {
	ctx = t.bind(ctx)
	if !t.portalLoggedIn(ctx) {
		return CreditsView{}
	}
	info, err := t.fetchAccount(ctx, accountFetchTimeout)
	if err != nil || info == nil || !info.LoggedIn {
		return CreditsView{}
	}
	var balance []string
	for _, line := range RenderAccountUsageLines(portalCreditsSnapshot(info), markdown) {
		trimmed := strings.TrimLeftFunc(line, unicode.IsSpace)
		if strings.HasPrefix(trimmed, "Top up:") || strings.HasPrefix(trimmed, "Top up via") || strings.HasPrefix(trimmed, "(or run") {
			continue
		}
		balance = append(balance, line)
	}
	var who []string
	if info.Email != "" {
		who = append(who, info.Email)
	}
	if info.OrgName != "" {
		who = append(who, "org "+info.OrgName)
	}
	view := CreditsView{LoggedIn: true, BalanceLines: balance, TopupURL: portal.TopupURL(info), Depleted: isFalse(info.PaidServiceAccess)}
	if len(who) > 0 {
		view.IdentityLine = "Topping up as " + strings.Join(who, " / ")
	}
	return view
}
