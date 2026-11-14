package usage

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/portal"
)

const (
	lowBalanceThresholdUSD = 5.0

	BarPlan  = "plan"
	BarTopup = "topup"

	StatusFree     = "free"
	StatusLow      = "low"
	StatusHealthy  = "healthy"
	StatusDepleted = "depleted"
)

func FormatRenews(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	parsed, ok := parseISO(text)
	if !ok {
		prefix := text
		if runeLen(prefix) > 10 {
			prefix = string([]rune(prefix)[:10])
		}
		day, err := time.Parse("2006-1-2", prefix)
		if err != nil {
			return text
		}
		parsed = day
	}
	return parsed.Format("Jan") + " " + strconv.Itoa(parsed.Day()) + ", " + strconv.Itoa(parsed.Year())
}

type UsageBar struct {
	Kind         string
	RemainingUSD float64
	TotalUSD     float64
	SpentUSD     float64
}

func (b UsageBar) PctUsed() (int, bool) {
	if b.Kind != BarPlan || b.TotalUSD <= 0 {
		return 0, false
	}
	return max(0, min(100, int(math.RoundToEven(b.SpentUSD/b.TotalUSD*100)))), true
}

func (b UsageBar) FillFraction() float64 {
	if b.TotalUSD <= 0 {
		return 0
	}
	return max(0.0, min(1.0, b.RemainingUSD/b.TotalUSD))
}

type UsageModel struct {
	Available                bool
	Status                   string
	PlanName                 string
	RenewsAt                 string
	RenewsDisplay            string
	SubscriptionRemainingUSD *float64
	TopupRemainingUSD        *float64
	TotalSpendableUSD        *float64
	PlanBar                  *UsageBar
	TopupBar                 *UsageBar
}

func (m UsageModel) HasTopup() bool {
	return m.TopupRemainingUSD != nil && *m.TopupRemainingUSD > 0
}

func finitePtr(v *float64) *float64 {
	f, ok := finite(v)
	if !ok {
		return nil
	}
	return &f
}

func UsageModelFromAccount(info *portal.AccountInfo) UsageModel {
	if info == nil || !info.LoggedIn || windowSeedReady(info.UsageWindow) {
		return UsageModel{Available: false, Status: StatusFree}
	}
	var subRemaining, topup, totalUsable, monthly *float64
	if access := info.PaidServiceAccessInfo; access != nil {
		subRemaining, topup, totalUsable = finitePtr(access.SubscriptionCreditsRemaining), finitePtr(access.PurchasedCreditsRemaining), finitePtr(access.TotalUsableCredits)
	}
	planName, renewsAt := "", ""
	if sub := info.Subscription; sub != nil {
		planName, renewsAt, monthly = sub.Plan, sub.CurrentPeriodEnd, finitePtr(sub.MonthlyCredits)
	}
	hasSubscription := planName != "" || (monthly != nil && *monthly > 0)
	hasTopup := topup != nil && *topup > 0
	total := totalUsable
	if total == nil && (subRemaining != nil || topup != nil) {
		sum := 0.0
		for _, part := range []*float64{subRemaining, topup} {
			if part != nil {
				sum += *part
			}
		}
		total = &sum
	}
	status := StatusHealthy
	switch {
	case isFalse(info.PaidServiceAccess):
		status = StatusDepleted
	case !hasSubscription && !hasTopup:
		status = StatusFree
	case total != nil && *total < lowBalanceThresholdUSD:
		status = StatusLow
	}
	m := UsageModel{
		Available: true, Status: status, PlanName: planName, RenewsAt: renewsAt, RenewsDisplay: FormatRenews(renewsAt),
		SubscriptionRemainingUSD: subRemaining, TopupRemainingUSD: topup, TotalSpendableUSD: total,
	}
	if monthly != nil && *monthly > 0 && subRemaining != nil {
		m.PlanBar = &UsageBar{Kind: BarPlan, RemainingUSD: max(0.0, min(*monthly, *subRemaining)), TotalUSD: *monthly, SpentUSD: max(0.0, *monthly-*subRemaining)}
	}
	if hasTopup {
		m.TopupBar = &UsageBar{Kind: BarTopup, RemainingUSD: *topup, TotalUSD: *topup}
	}
	return m
}

func (t *Tracker) UsageModel(ctx context.Context) UsageModel {
	ctx = t.bind(ctx)
	if !t.portalLoggedIn(ctx) {
		return UsageModel{Available: false, Status: StatusFree}
	}
	info, err := t.fetchAccount(ctx, accountFetchTimeout)
	if err != nil {
		return UsageModel{Available: false, Status: StatusFree}
	}
	return UsageModelFromAccount(info)
}
