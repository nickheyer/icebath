package provider

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Total spendable dollars below which a paid account is flagged low
const LowBalanceThresholdUSD = 5.0

func finiteUSD(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	f := *v
	return &f
}

// FormatUSD renders dollars with thousands separators and cents, nil reads as zero
func FormatUSD(v *float64) string {
	value := 0.0
	if v != nil {
		value = *v
	}
	text := strconv.FormatFloat(math.Abs(value), 'f', 2, 64)
	whole, cents, _ := strings.Cut(text, ".")
	var b strings.Builder
	for i, r := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	sign := ""
	if math.Signbit(value) {
		sign = "-"
	}
	return "$" + sign + b.String() + "." + cents
}

// NousLoggedIn reports a Nous access token in the local auth state, failing closed
func NousLoggedIn(ctx context.Context) bool {
	state, err := NewStore(home.Dir(ctx)).GetProviderAuthState("nous")
	if err != nil {
		return false
	}
	tok, _ := state["access_token"].(string)
	return strings.TrimSpace(tok) != ""
}

// FetchNousAccount is the wall clock bounded fresh Portal account fetch
func FetchNousAccount(ctx context.Context, timeout time.Duration) (*NousPortalAccountInfo, error) {
	return FetchPortalAccount(ctx, timeout)
}

var (
	isoDateRe     = regexp.MustCompile(`^(\d{4})(-?)(\d{2})(-?)(\d{2})(.*)$`)
	isoTimeRe     = regexp.MustCompile(`^\d{2}(:?\d{2}(:?\d{2}([.,]\d{1,9})?)?)?(Z|[+-]\d{2}(:?\d{2}(:?\d{2}([.,]\d{1,6})?)?)?)?$`)
	strptimeDayRe = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})$`)
)

func validDate(y, m, d int) bool {
	if m < 1 || m > 12 || d < 1 {
		return false
	}
	t := time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC)
	return t.Day() == d && int(t.Month()) == m
}

// Date of an ISO 8601 date or timestamp as Python fromisoformat accepts it
func isoDate(text string) (time.Time, bool) {
	m := isoDateRe.FindStringSubmatch(text)
	if m == nil || m[2] != m[4] {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[3])
	d, _ := strconv.Atoi(m[5])
	if !validDate(y, mo, d) {
		return time.Time{}, false
	}
	if rest := m[6]; rest != "" {
		if !isoTimeRe.MatchString(rest[1:]) || (rest[0] != 'T' && rest[0] != ' ') {
			return time.Time{}, false
		}
	}
	return time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC), true
}

// FormatRenews renders an ISO date or timestamp as Jul 24, 2026, unparseable text comes back unchanged
func FormatRenews(value string) (string, bool) {
	text := strings.TrimSpace(value)
	if text == "" {
		return "", false
	}
	dt, ok := isoDate(text)
	if !ok {
		head := text
		if len(head) > 10 {
			head = head[:10]
		}
		m := strptimeDayRe.FindStringSubmatch(head)
		if m == nil {
			return text, true
		}
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if !validDate(y, mo, d) {
			return text, true
		}
		dt = time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	}
	return fmt.Sprintf("%s %d, %d", dt.Format("Jan"), dt.Day(), dt.Year()), true
}

// UsageBar is spent of total plus remaining, a top-up bar has no denominator and renders full
type UsageBar struct {
	Kind         string
	RemainingUSD float64
	TotalUSD     float64
	SpentUSD     float64
}

// PctUsed is the percent of the plan allowance used, false for a top-up bar or no allowance
func (b UsageBar) PctUsed() (int, bool) {
	if b.Kind != "plan" || b.TotalUSD <= 0 {
		return 0, false
	}
	return max(0, min(100, int(math.RoundToEven(b.SpentUSD/b.TotalUSD*100)))), true
}

// FillFraction is the share of the bar that reads as remaining
func (b UsageBar) FillFraction() float64 {
	if b.TotalUSD <= 0 {
		return 0
	}
	return math.Max(0, math.Min(1, b.RemainingUSD/b.TotalUSD))
}

// UsageModel is the dollar usage model shared by usage and subscription views
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

// HasTopup reports a positive top-up balance
func (m UsageModel) HasTopup() bool { return m.TopupRemainingUSD != nil && *m.TopupRemainingUSD > 0 }

func unavailableUsage() UsageModel { return UsageModel{Status: "free"} }

func floatRef(v float64) *float64 { return &v }

// UsageModelFromAccount builds the dollar usage model from a Portal account, unavailable for window plans
func UsageModelFromAccount(info *NousPortalAccountInfo) UsageModel {
	if info == nil || !info.LoggedIn || applyWindowSeedFields(info.UsageWindow, nil) {
		return unavailableUsage()
	}
	var subRemaining, topupRemaining, totalUsable, monthly *float64
	if acc := info.PaidServiceAccessInfo; acc != nil {
		subRemaining = finiteUSD(acc.SubscriptionCreditsRemaining)
		topupRemaining = finiteUSD(acc.PurchasedCreditsRemaining)
		totalUsable = finiteUSD(acc.TotalUsableCredits)
	}
	planName, renewsAt := "", ""
	if sub := info.Subscription; sub != nil {
		if sub.Plan != nil {
			planName = *sub.Plan
		}
		if sub.CurrentPeriodEnd != nil {
			renewsAt = *sub.CurrentPeriodEnd
		}
		monthly = finiteUSD(sub.MonthlyCredits)
	}
	hasSubscription := planName != "" || (monthly != nil && *monthly > 0)
	hasTopup := topupRemaining != nil && *topupRemaining > 0
	total := totalUsable
	if total == nil && (subRemaining != nil || topupRemaining != nil) {
		sum := 0.0
		for _, v := range []*float64{subRemaining, topupRemaining} {
			if v != nil {
				sum += *v
			}
		}
		total = &sum
	}
	status := "healthy"
	switch {
	case info.PaidServiceAccess != nil && !*info.PaidServiceAccess:
		status = "depleted"
	case !hasSubscription && !hasTopup:
		status = "free"
	case total != nil && *total < LowBalanceThresholdUSD:
		status = "low"
	}
	model := UsageModel{Available: true, Status: status, PlanName: planName, RenewsAt: renewsAt,
		SubscriptionRemainingUSD: subRemaining, TopupRemainingUSD: topupRemaining, TotalSpendableUSD: total}
	model.RenewsDisplay, _ = FormatRenews(renewsAt)
	if monthly != nil && *monthly > 0 && subRemaining != nil {
		model.PlanBar = &UsageBar{Kind: "plan", RemainingUSD: math.Max(0, math.Min(*monthly, *subRemaining)), TotalUSD: *monthly, SpentUSD: math.Max(0, *monthly-*subRemaining)}
	}
	if hasTopup {
		model.TopupBar = &UsageBar{Kind: "topup", RemainingUSD: *topupRemaining, TotalUSD: *topupRemaining}
	}
	return model
}

// BuildUsageModel fetches the account and builds the usage model, the dev fixture env short circuits
func BuildUsageModel(ctx context.Context, timeout time.Duration) UsageModel {
	if fixture, ok := devFixtureUsageModel(); ok {
		return fixture
	}
	if !NousLoggedIn(ctx) {
		return unavailableUsage()
	}
	info, err := FetchNousAccount(ctx, timeout)
	if err != nil {
		return unavailableUsage()
	}
	return UsageModelFromAccount(info)
}

func fixturePlanBar(remaining, spent float64) *UsageBar {
	return &UsageBar{Kind: "plan", RemainingUSD: remaining, TotalUSD: 20.0, SpentUSD: spent}
}

// Fixture usage model named by the dev credits fixture env
func devFixtureUsageModel() (UsageModel, bool) {
	name := strings.ToLower(strings.TrimSpace(os.Getenv(DevCreditsFixtureEnv)))
	switch name {
	case "mid":
		name = "healthy"
	case "top-up":
		name = "topup"
	}
	plus := UsageModel{Available: true, PlanName: "Plus", RenewsAt: "2026-07-01"}
	switch name {
	case "free":
		return UsageModel{Available: true, Status: "free"}, true
	case "healthy":
		plus.Status, plus.SubscriptionRemainingUSD, plus.TotalSpendableUSD, plus.PlanBar = "healthy", floatRef(14.0), floatRef(14.0), fixturePlanBar(14.0, 6.0)
	case "topup":
		plus.Status, plus.SubscriptionRemainingUSD, plus.TopupRemainingUSD, plus.TotalSpendableUSD = "healthy", floatRef(14.0), floatRef(12.0), floatRef(26.0)
		plus.PlanBar, plus.TopupBar = fixturePlanBar(14.0, 6.0), &UsageBar{Kind: "topup", RemainingUSD: 12.0, TotalUSD: 12.0}
	case "low":
		plus.Status, plus.SubscriptionRemainingUSD, plus.TotalSpendableUSD, plus.PlanBar = "low", floatRef(3.4), floatRef(3.4), fixturePlanBar(3.4, 16.6)
	case "depleted":
		plus.Status, plus.SubscriptionRemainingUSD, plus.TotalSpendableUSD, plus.PlanBar = "depleted", floatRef(0.0), floatRef(0.0), fixturePlanBar(0.0, 20.0)
	default:
		return UsageModel{}, false
	}
	return plus, true
}

// Portal entitlement copy, a billing wall on a usage window account gets the window next step
func nousEntitlementMessage(capability string, billing bool) string {
	info := GetNousPortalAccountInfo(creditsCtx(), true)
	if billing {
		if guidance := FormatUsageWindowBlockGuidance(info); guidance != "" {
			return guidance
		}
	}
	return FormatNousPortalEntitlementMessage(info, capability, true)
}

// Emits each line of the message as a hint, false when there is nothing to print
func printGuidance(emit func(...string), message string) bool {
	if message == "" {
		return false
	}
	for _, line := range pySplitLines(message) {
		emit("   💡 " + line)
	}
	return true
}

// PrintNousEntitlementGuidance emits the Portal entitlement copy for a capability
func PrintNousEntitlementGuidance(emit func(...string), capability string) bool {
	return printGuidance(emit, nousEntitlementMessage(capability, false))
}

// BillingOrEntitlementMessage is the next step copy for a billing classified failure on any provider
func BillingOrEntitlementMessage(capability, provider, baseURL, model string, unverified bool) string {
	if IsNousInferenceRoute(provider, baseURL) {
		return nousEntitlementMessage(capability, true)
	}
	providerLabel := strings.TrimSpace(provider)
	if providerLabel == "" {
		providerLabel = "the selected provider"
	}
	modelLabel := strings.TrimSpace(model)
	if modelLabel == "" {
		modelLabel = "the selected model"
	}
	if strings.ToLower(strings.TrimSpace(provider)) == "anthropic" {
		switchLine := "You can also switch to an Anthropic API key or another provider with /model <model> --provider <provider>."
		if unverified {
			return strings.Join([]string{
				providerLabel + " reported that your Claude subscription usage may be exhausted for " + modelLabel +
					" (included quota + extra-usage credits) — but this specific error is not proof of a billing problem.",
				"If https://claude.ai/settings/usage still shows quota remaining, this is probably NOT a billing problem: on a Claude subscription (OAuth) token Anthropic returns this same message when its content filter rejects part of the request — typically a phrase in the system prompt.",
				"If usage really is exhausted: wait for the billing cycle to reset, or add extra usage at https://claude.ai/settings/usage",
				switchLine,
				"Retry with a fresh credential state: `icebath auth reset anthropic`. Until that cooldown clears, this error can be replayed from cache without contacting the API.",
			}, "\n")
		}
		return strings.Join([]string{
			providerLabel + " reported that your Claude subscription usage is exhausted for " + modelLabel + " (included quota + extra-usage credits).",
			"Options: wait for the billing cycle to reset, or add extra usage at https://claude.ai/settings/usage",
			switchLine,
		}, "\n")
	}
	link := BuildBillingBlock(provider, baseURL, model, "")
	if link.ProviderLabel != "" {
		providerLabel = link.ProviderLabel
	}
	lines := []string{
		providerLabel + " reported that billing, credits, or account entitlement is exhausted for " + modelLabel + ".",
		"Add credits or update billing with that provider, then retry.",
	}
	if link.BillingURL != "" {
		lines = append(lines, providerLabel+" billing: "+link.BillingURL)
	}
	lines = append(lines, "You can switch providers temporarily with /model <model> --provider <provider>.")
	return strings.Join(lines, "\n")
}

// BillingBlockDict is the structured billing descriptor, flagged unverified when the cause may be a filter
func BillingBlockDict(provider, baseURL, model, message string, unverified bool) map[string]any {
	block := BuildBillingBlock(provider, baseURL, model, message).ToDict()
	if unverified {
		block["unverified"] = true
	}
	return block
}

// BillingTerminalLabel prefixes a billing failure summary without asserting an unverified exhaustion
func BillingTerminalLabel(summary string, unverified bool) string {
	if unverified {
		return "Provider reported usage/credit exhaustion (unverified — the same error can be a content-filter rejection, not billing): " + summary
	}
	return "Billing or credits exhausted: " + summary
}

// PrintBillingOrEntitlementGuidance emits the billing or entitlement next steps, false when there are none
func PrintBillingOrEntitlementGuidance(emit func(...string), capability, provider, baseURL, model string, unverified bool) bool {
	return printGuidance(emit, BillingOrEntitlementMessage(capability, provider, baseURL, model, unverified))
}
