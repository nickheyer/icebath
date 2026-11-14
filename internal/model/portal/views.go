package portal

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/internal/model/auth"
)

const (
	EnvDevBillingFixture      = "ICEBATH_DEV_BILLING_FIXTURE"
	EnvDevSubscriptionFixture = "ICEBATH_DEV_SUBSCRIPTION_FIXTURE"
	devFixturePortal          = "https://portal.icebath.app/billing"
)

const (
	moneyNaN         = "NaN"
	moneySignaling   = "sNaN"
	moneyInfinity    = "Infinity"
	moneyPrecision   = 28
	moneyMaxExponent = 999999999999999999
	moneyMaxZeros    = 1<<31 - 1
)

var ErrMoneyUndefined = errors.New("portal: the amount is NaN, infinite or longer than 28 digits, so the operation has no result")

type Money struct {
	coef    *big.Int
	exp     int64
	neg     bool
	class   string
	payload string
}

func digitValue(r rune) (int, bool) {
	for _, span := range unicode.Nd.R16 {
		if rune(span.Lo) <= r && r <= rune(span.Hi) {
			return int(r-rune(span.Lo)) % 10, true
		}
	}
	for _, span := range unicode.Nd.R32 {
		if rune(span.Lo) <= r && r <= rune(span.Hi) {
			return int(r-rune(span.Lo)) % 10, true
		}
	}
	return 0, false
}

func asciiDecimal(text string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(text) {
		switch {
		case r == '_':
		case r < 0x80:
			b.WriteRune(r)
		default:
			if d, ok := digitValue(r); ok {
				b.WriteByte(byte('0' + d))
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

func specialMoney(s string) (*Money, bool) {
	neg := strings.HasPrefix(s, "-")
	body := s
	if body != "" && (body[0] == '+' || body[0] == '-') {
		body = body[1:]
	}
	lower := strings.ToLower(body)
	if lower == "inf" || lower == "infinity" {
		return &Money{class: moneyInfinity, neg: neg}, true
	}
	for _, class := range []string{moneySignaling, moneyNaN} {
		prefix := strings.ToLower(class)
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		payload := lower[len(prefix):]
		if strings.Trim(payload, "0123456789") != "" {
			return nil, false
		}
		return &Money{class: class, neg: neg, payload: strings.TrimLeft(payload, "0")}, true
	}
	return nil, false
}

func decimalSyntax(s string) bool {
	i, digits := 0, 0
	sign := func() {
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			i++
		}
	}
	run := func() int {
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i - start
	}
	sign()
	digits += run()
	if i < len(s) && s[i] == '.' {
		i++
		digits += run()
	}
	if digits == 0 {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		sign()
		if run() == 0 {
			return false
		}
	}
	return i == len(s)
}

func ParseMoney(text string) (*Money, bool) {
	cleaned := asciiDecimal(text)
	if special, ok := specialMoney(cleaned); ok {
		return special, true
	}
	if !decimalSyntax(cleaned) {
		return nil, false
	}
	neg := cleaned[0] == '-'
	if cleaned[0] == '+' || cleaned[0] == '-' {
		cleaned = cleaned[1:]
	}
	mantissa, exponent, scientific := strings.Cut(strings.ToLower(cleaned), "e")
	whole, frac, _ := strings.Cut(mantissa, ".")
	coef, ok := new(big.Int).SetString(whole+frac, 10)
	if !ok {
		return nil, false
	}
	exp := int64(0)
	if scientific {
		parsed, err := strconv.ParseInt(exponent, 10, 64)
		if err != nil || parsed > moneyMaxExponent || parsed < -moneyMaxExponent {
			return nil, false
		}
		exp = parsed
	}
	return &Money{coef: coef, exp: exp - int64(len(frac)), neg: neg}, true
}

func moneyOf(v any) *Money {
	if v == nil {
		return nil
	}
	if _, isBool := v.(bool); isBool {
		return nil
	}
	m, _ := ParseMoney(textual(v))
	return m
}

func moneyInt(n int64) *Money {
	return &Money{coef: new(big.Int).Abs(big.NewInt(n)), neg: n < 0}
}

func pow10(n int64) *big.Int { return new(big.Int).Exp(big.NewInt(10), big.NewInt(n), nil) }

func (m *Money) nan() bool { return m.class == moneyNaN || m.class == moneySignaling }

func (m *Money) digits() int64 { return int64(len(m.coef.String())) }

func (m *Money) signum() int {
	switch {
	case m.class == "" && m.coef.Sign() == 0:
		return 0
	case m.neg:
		return -1
	}
	return 1
}

func (m *Money) Sign() (int, error) {
	if m.nan() {
		return 0, ErrMoneyUndefined
	}
	return m.signum(), nil
}

func (m *Money) cmpMagnitude(other *Money) int {
	if m.class == moneyInfinity || other.class == moneyInfinity {
		return cmp.Compare(len(m.class), len(other.class))
	}
	if left, right := m.digits()+m.exp, other.digits()+other.exp; left != right {
		return cmp.Compare(left, right)
	}
	a, b := m.coef, other.coef
	if m.exp > other.exp {
		a = new(big.Int).Mul(a, pow10(m.exp-other.exp))
	} else if other.exp > m.exp {
		b = new(big.Int).Mul(b, pow10(other.exp-m.exp))
	}
	return a.Cmp(b)
}

func (m *Money) Cmp(other *Money) (int, error) {
	if m.nan() || other.nan() {
		return 0, ErrMoneyUndefined
	}
	left, right := m.signum(), other.signum()
	if left != right || left == 0 {
		return cmp.Compare(left, right), nil
	}
	return left * m.cmpMagnitude(other), nil
}

func (m *Money) Float64() (float64, error) {
	switch m.class {
	case moneySignaling:
		return 0, ErrMoneyUndefined
	case moneyNaN:
		return math.NaN(), nil
	case moneyInfinity:
		if m.neg {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	}
	sign := ""
	if m.neg {
		sign = "-"
	}
	f, _ := strconv.ParseFloat(sign+m.coef.String()+"e"+strconv.FormatInt(m.exp, 10), 64)
	return f, nil
}

func (m *Money) integral() bool {
	if m.exp >= 0 || m.coef.Sign() == 0 {
		return true
	}
	if -m.exp > m.digits() {
		return false
	}
	return new(big.Int).Rem(m.coef, pow10(-m.exp)).Sign() == 0
}

func (m *Money) cents() (*big.Int, error) {
	if m.class != "" {
		return nil, ErrMoneyUndefined
	}
	shift := m.exp + 2
	if m.coef.Sign() == 0 {
		return new(big.Int), nil
	}
	if shift >= 0 {
		if m.digits()+shift > moneyPrecision {
			return nil, ErrMoneyUndefined
		}
		return new(big.Int).Mul(m.coef, pow10(shift)), nil
	}
	if -shift > m.digits() {
		return new(big.Int), nil
	}
	unit := pow10(-shift)
	quo, rem := new(big.Int).QuoRem(m.coef, unit, new(big.Int))
	switch new(big.Int).Lsh(rem, 1).Cmp(unit) {
	case 1:
		quo.Add(quo, big.NewInt(1))
	case 0:
		if quo.Bit(0) == 1 {
			quo.Add(quo, big.NewInt(1))
		}
	}
	if int64(len(quo.String())) > moneyPrecision {
		return nil, ErrMoneyUndefined
	}
	return quo, nil
}

func (m *Money) wholeCents() (bool, error) {
	if _, err := m.cents(); err != nil {
		return false, err
	}
	shift := m.exp + 2
	if shift >= 0 || m.coef.Sign() == 0 {
		return true, nil
	}
	if -shift > m.digits() {
		return false, nil
	}
	return new(big.Int).Rem(m.coef, pow10(-shift)).Sign() == 0, nil
}

func groupDigits(digits string) string {
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func FormatMoney(m *Money, grouped bool) (string, error) {
	if m == nil {
		return "—", nil
	}
	sign := ""
	if m.neg {
		sign = "-"
	}
	switch m.class {
	case moneySignaling:
		return "", ErrMoneyUndefined
	case moneyNaN:
		return "$" + sign + moneyNaN + m.payload, nil
	case moneyInfinity:
		return "$" + sign + moneyInfinity, nil
	}
	render := func(digits string) string {
		if grouped {
			return groupDigits(digits)
		}
		return digits
	}
	if m.integral() {
		digits := m.coef.String()
		switch {
		case m.exp > moneyMaxZeros:
			return "", ErrMoneyUndefined
		case m.exp > 0:
			digits += strings.Repeat("0", int(m.exp))
		case m.exp < 0 && m.coef.Sign() != 0:
			digits = digits[:int64(len(digits))+m.exp]
		}
		return "$" + sign + render(digits), nil
	}
	cents, err := m.cents()
	if err != nil {
		return "", err
	}
	whole, frac := new(big.Int).QuoRem(cents, big.NewInt(100), new(big.Int))
	return fmt.Sprintf("$%s%s.%02d", sign, render(whole.String()), frac.Int64()), nil
}

func optionalString(raw *object, key string) string {
	return raw.str(key)
}

type CardInfo struct {
	Brand       string
	Last4       string
	ResolvedVia string
}

func (c CardInfo) Masked() string {
	if c.Last4 == "" {
		return c.Brand
	}
	return c.Brand + " ····" + c.Last4
}

func (c CardInfo) Provenance() string {
	switch c.ResolvedVia {
	case "subPin":
		return "the card on your subscription"
	case "customerDefault":
		return "your default card saved on the portal"
	case "autoRefill":
		return "your auto-reload card"
	}
	return ""
}

func (c CardInfo) Display() string {
	if label := c.Provenance(); label != "" {
		return c.Masked() + " — " + label
	}
	return c.Masked()
}

type PaymentMethodInfo struct {
	Kind        string
	Brand       string
	Last4       string
	Wallet      string
	Email       string
	ResolvedVia string
	RawKind     string
}

type MonthlyCap struct {
	LimitUSD          *Money
	SpentThisMonthUSD *Money
	IsDefaultCeiling  bool
}

type AutoReloadCard struct {
	Kind            string
	PaymentMethodID string
	Brand           string
	Last4           string
}

type AutoReload struct {
	Enabled      bool
	ThresholdUSD *Money
	ReloadToUSD  *Money
	Card         *AutoReloadCard
}

func roleIsAdmin(role string) bool {
	upper := strings.ToUpper(role)
	return upper == "OWNER" || upper == "ADMIN"
}

type BillingState struct {
	LoggedIn           bool
	OrgID              string
	OrgSlug            string
	OrgName            string
	Role               string
	CanChangePlanRaw   *bool
	BalanceUSD         *Money
	CLIBillingEnabled  bool
	ChargePresets      []*Money
	MinUSD             *Money
	MaxUSD             *Money
	Card               *CardInfo
	PaymentMethod      *PaymentMethodInfo
	MonthlyCap         *MonthlyCap
	AutoReload         *AutoReload
	KeepGoingOnCredits *bool
	PortalURL          string
	Error              string
}

func (s BillingState) IsAdmin() bool { return roleIsAdmin(s.Role) }

func (s BillingState) CanChangePlan() bool {
	if s.CanChangePlanRaw != nil {
		return *s.CanChangePlanRaw
	}
	return s.IsAdmin()
}

func (s BillingState) CanCharge() bool { return s.CanChangePlan() && s.CLIBillingEnabled }

func parseCard(v any) *CardInfo {
	raw, ok := v.(*object)
	if !ok || raw == nil {
		return nil
	}
	brand, okBrand := raw.get("brand").(string)
	last4, okLast := raw.get("last4").(string)
	if !okBrand || !okLast {
		return nil
	}
	return &CardInfo{Brand: brand, Last4: last4, ResolvedVia: optionalString(raw, "resolvedVia")}
}

func parsePaymentMethod(v any) *PaymentMethodInfo {
	raw, ok := v.(*object)
	if !ok || raw == nil {
		return nil
	}
	kind, isString := raw.get("kind").(string)
	if !isString {
		return nil
	}
	resolvedVia, brand, last4 := optionalString(raw, "resolvedVia"), optionalString(raw, "brand"), optionalString(raw, "last4")
	switch {
	case kind == "card" && brand != "" && last4 != "":
		return &PaymentMethodInfo{Kind: "card", Brand: brand, Last4: last4, Wallet: optionalString(raw, "wallet"), ResolvedVia: resolvedVia}
	case kind == "link":
		return &PaymentMethodInfo{Kind: "link", Email: optionalString(raw, "email"), ResolvedVia: resolvedVia}
	}
	return &PaymentMethodInfo{Kind: "unknown", RawKind: kind, ResolvedVia: resolvedVia}
}

func parseMonthlyCap(v any) *MonthlyCap {
	raw, ok := v.(*object)
	if !ok || raw == nil {
		return nil
	}
	return &MonthlyCap{LimitUSD: moneyOf(raw.get("limitUsd")), SpentThisMonthUSD: moneyOf(raw.get("spentThisMonthUsd")),
		IsDefaultCeiling: truthy(raw.get("isDefaultCeiling"))}
}

func parseAutoReloadCard(v any) *AutoReloadCard {
	raw, ok := v.(*object)
	if !ok || raw == nil {
		return nil
	}
	kind := raw.str("kind")
	switch kind {
	case "canonical", "none":
		return &AutoReloadCard{Kind: kind}
	case "distinct":
		return &AutoReloadCard{Kind: kind, PaymentMethodID: optionalString(raw, "paymentMethodId"),
			Brand: optionalString(raw, "brand"), Last4: optionalString(raw, "last4")}
	}
	return nil
}

func parseAutoReload(v any) *AutoReload {
	raw, ok := v.(*object)
	if !ok || raw == nil {
		return nil
	}
	return &AutoReload{Enabled: truthy(raw.get("enabled")), ThresholdUSD: moneyOf(raw.get("thresholdUsd")),
		ReloadToUSD: moneyOf(raw.get("reloadToUsd")), Card: parseAutoReloadCard(raw.get("card"))}
}

func parseOrgFields(payload *object) (*object, *bool) {
	org := payload.obj("org")
	if org == nil {
		org = newObject()
	}
	return org, boolOf(payload.get("canChangePlan"))
}

func BillingStateFromPayload(payload json.RawMessage, portalURL string) BillingState {
	return billingStateFrom(objectOrEmpty(payload), portalURL)
}

func billingStateFrom(payload *object, portalURL string) BillingState {
	org, canChangePlan := parseOrgFields(payload)
	bounds := payload.obj("bounds")
	presets := []*Money{}
	for _, item := range payload.list("chargePresets") {
		if m := moneyOf(item); m != nil {
			presets = append(presets, m)
		}
	}
	var keepGoing *bool
	if raw := payload.obj("keepGoingOnCredits"); raw != nil {
		keepGoing = boolOf(raw.get("enabled"))
	}
	return BillingState{
		LoggedIn: true, OrgID: textualOrEmpty(org.get("id")), OrgSlug: textualOrEmpty(org.get("slug")), OrgName: textualOrEmpty(org.get("name")),
		Role: textualOrEmpty(org.get("role")), CanChangePlanRaw: canChangePlan, BalanceUSD: moneyOf(payload.get("balanceUsd")),
		CLIBillingEnabled: truthy(payload.get("cliBillingEnabled")), ChargePresets: presets,
		MinUSD: moneyOf(bounds.get("minUsd")), MaxUSD: moneyOf(bounds.get("maxUsd")),
		Card: parseCard(payload.get("card")), PaymentMethod: parsePaymentMethod(payload.get("paymentMethod")),
		MonthlyCap: parseMonthlyCap(payload.get("monthlyCap")), AutoReload: parseAutoReload(payload.get("autoReload")),
		KeepGoingOnCredits: keepGoing, PortalURL: portalURL,
	}
}

type portalFetch struct {
	payload   *object
	portalURL string
	failed    bool
	failure   string
}

func (a *Account) fetchPortalState(ctx context.Context, label string, fetch func(ctx context.Context, timeout time.Duration) (json.RawMessage, error), fallback func(base string) string, timeout time.Duration) portalFetch {
	ctx = a.bind(ctx)
	raw, err := fetch(ctx, timeout)
	if err != nil {
		var billing *BillingError
		switch {
		case errors.As(err, &billing) && billing.NotLoggedIn():
			return portalFetch{failed: true}
		case errors.As(err, &billing):
			slog.Debug(label+" ▸ /state fetch failed (fail-open)", "error", err)
			return portalFetch{failed: true, failure: err.Error()}
		}
		slog.Debug(label+" ▸ /state unexpected error (fail-open)", "error", fmt.Sprintf("%T", err))
		return portalFetch{failed: true, failure: fmt.Sprintf("could not load %s state", label)}
	}
	payload := objectOrEmpty(raw)
	portalURL := ""
	if rawPortal := payload.get("portalUrl"); truthy(rawPortal) {
		portalURL = a.absolutizePortalURL(ctx, rawPortal)
	}
	if portalURL == "" {
		portalURL = fallback(a.resolvePortalBaseURL(ctx, nil))
	}
	return portalFetch{payload: payload, portalURL: portalURL}
}

func (a *Account) BuildBillingState(ctx context.Context, timeout time.Duration) BillingState {
	if fixture, ok := devFixtureBillingState(); ok {
		return fixture
	}
	got := a.fetchPortalState(ctx, "billing", a.BillingState, func(base string) string {
		return strings.TrimRight(base, "/") + "/billing?topup=open"
	}, timeout)
	if got.failed {
		return BillingState{Error: got.failure}
	}
	return billingStateFrom(got.payload, got.portalURL)
}

func fixtureName(env string, aliases map[string]string) string {
	name := strings.ToLower(strings.TrimSpace(os.Getenv(env)))
	if alias, ok := aliases[name]; ok {
		return alias
	}
	return name
}

func devFixtureBillingState() (BillingState, bool) {
	name := fixtureName(EnvDevBillingFixture, map[string]string{
		"logged_out": "logged-out", "loggedout": "logged-out", "card_sub": "card-sub", "card_autoreload": "card-autoreload",
		"autoreload": "card-autoreload", "not-admin": "notadmin", "member": "notadmin", "billing_off": "billing-off", "off": "billing-off",
	})
	if name == "" {
		return BillingState{}, false
	}
	if name == "logged-out" {
		return BillingState{}, true
	}
	threeForty, _ := ParseMoney("3.40")
	state := BillingState{
		LoggedIn: true, OrgID: "org_acme", OrgSlug: "acme", OrgName: "Acme Inc", Role: "OWNER",
		BalanceUSD: threeForty, CLIBillingEnabled: true, MinUSD: moneyInt(5), MaxUSD: moneyInt(500),
		ChargePresets: []*Money{moneyInt(10), moneyInt(25), moneyInt(50)}, PortalURL: devFixturePortal + "?topup=open",
	}
	card := &CardInfo{Brand: "Visa", Last4: "4242"}
	switch name {
	case "nocard":
	case "card":
		state.Card = card
	case "card-sub":
		state.Card = &CardInfo{Brand: "Visa", Last4: "4242", ResolvedVia: "subPin"}
	case "card-autoreload":
		state.Card = card
		state.AutoReload = &AutoReload{Enabled: true, ThresholdUSD: moneyInt(5), ReloadToUSD: moneyInt(25)}
	case "notadmin":
		state.Card, state.Role = card, "MEMBER"
	case "billing-off":
		state.CLIBillingEnabled = false
	default:
		return BillingState{Error: fmt.Sprintf("unknown %s: %s", EnvDevBillingFixture, name)}, true
	}
	return state, true
}

func NewIdempotencyKey() string { return uuid.NewString() }

type AmountValidation struct {
	OK     bool
	Amount *Money
	Error  string
}

func ValidateChargeAmount(raw string, minUSD, maxUSD *Money) (AmountValidation, error) {
	amount, ok := ParseMoney(strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(raw), "$")))
	if !ok {
		return AmountValidation{Error: "Enter a dollar amount, e.g. 100"}, nil
	}
	sign, err := amount.Sign()
	if err != nil {
		return AmountValidation{}, err
	}
	if sign <= 0 {
		return AmountValidation{Error: "Amount must be greater than $0"}, nil
	}
	whole, err := amount.wholeCents()
	if err != nil {
		return AmountValidation{}, err
	}
	if !whole {
		return AmountValidation{Error: "Amount can't be smaller than a cent"}, nil
	}
	for _, bound := range []struct {
		limit *Money
		floor bool
		label string
	}{{minUSD, true, "Minimum is "}, {maxUSD, false, "Maximum is "}} {
		if bound.limit == nil {
			continue
		}
		order, err := amount.Cmp(bound.limit)
		if err != nil {
			return AmountValidation{}, err
		}
		if (bound.floor && order < 0) || (!bound.floor && order > 0) {
			text, err := FormatMoney(bound.limit, false)
			if err != nil {
				return AmountValidation{}, err
			}
			return AmountValidation{Error: bound.label + text}, nil
		}
	}
	return AmountValidation{OK: true, Amount: amount}, nil
}

type CurrentSubscription struct {
	TierID                   string
	TierName                 string
	MonthlyCredits           *Money
	CreditsRemaining         *Money
	CycleEndsAt              string
	PendingDowngradeTierName string
	PendingDowngradeAt       string
	CancelAtPeriodEnd        bool
	CancellationEffectiveAt  string
}

type SubscriptionTier struct {
	TierID          string
	Name            string
	TierOrder       int
	DollarsPerMonth *Money
	MonthlyCredits  *Money
	IsCurrent       bool
	IsEnabled       bool
}

type SubscriptionChangePreview struct {
	Effect              string
	Reason              string
	CurrentTierID       string
	CurrentTierName     string
	TargetTierID        string
	TargetTierName      string
	MonthlyCreditsDelta *Money
	AmountDueNowCents   *int
	EffectiveAt         string
}

type SubscriptionState struct {
	LoggedIn         bool
	OrgName          string
	OrgID            string
	Role             string
	CanChangePlanRaw *bool
	Context          string
	Current          *CurrentSubscription
	Tiers            []SubscriptionTier
	PortalURL        string
	Error            string
}

func (s SubscriptionState) IsAdmin() bool { return roleIsAdmin(s.Role) }

func (s SubscriptionState) CanChangePlan() bool {
	if s.CanChangePlanRaw != nil {
		return *s.CanChangePlanRaw
	}
	return s.IsAdmin()
}

func tierID(raw *object) string {
	if v := raw.get("tierId"); truthy(v) {
		return textual(v)
	}
	return textualOrEmpty(raw.get("id"))
}

func firstTruthy(raw *object, keys ...string) string {
	for _, k := range keys {
		if v := raw.get(k); truthy(v) {
			return textual(v)
		}
	}
	return ""
}

func parseCurrentSubscription(v any) *CurrentSubscription {
	raw, ok := v.(*object)
	if !ok || raw == nil || tierID(raw) == "" {
		return nil
	}
	return &CurrentSubscription{
		TierID: tierID(raw), TierName: firstTruthy(raw, "tierName", "name"),
		MonthlyCredits: moneyOf(raw.get("monthlyCredits")), CreditsRemaining: moneyOf(raw.get("creditsRemaining")),
		CycleEndsAt: textualOrEmpty(raw.get("cycleEndsAt")), PendingDowngradeTierName: textualOrEmpty(raw.get("pendingDowngradeTierName")),
		PendingDowngradeAt: textualOrEmpty(raw.get("pendingDowngradeAt")), CancelAtPeriodEnd: truthy(raw.get("cancelAtPeriodEnd")),
		CancellationEffectiveAt: textualOrEmpty(raw.get("cancellationEffectiveAt")),
	}
}

func parseTier(v any) (SubscriptionTier, bool, error) {
	raw, ok := v.(*object)
	if !ok || raw == nil || tierID(raw) == "" {
		return SubscriptionTier{}, false, nil
	}
	order := int64(0)
	if value := raw.get("tierOrder"); value != nil {
		if order, ok = intOf(value); !ok {
			return SubscriptionTier{}, false, fmt.Errorf("portal: subscription tier %s carries a non-numeric tierOrder", tierID(raw))
		}
	}
	enabled := true
	if v := raw.get("isEnabled"); v != nil {
		enabled = truthy(v)
	}
	return SubscriptionTier{
		TierID: tierID(raw), Name: textualOrEmpty(raw.get("name")), TierOrder: int(order),
		DollarsPerMonth: moneyOf(raw.get("dollarsPerMonthDisplay")), MonthlyCredits: moneyOf(raw.get("monthlyCredits")),
		IsCurrent: truthy(raw.get("isCurrent")), IsEnabled: enabled,
	}, true, nil
}

func SubscriptionChangePreviewFromPayload(raw json.RawMessage) SubscriptionChangePreview {
	payload := objectOrEmpty(raw)
	effect, ok := payload.get("effect").(string)
	if !ok {
		effect = "blocked"
	}
	out := SubscriptionChangePreview{
		Effect: effect, Reason: textualOrEmpty(payload.get("reason")),
		CurrentTierID: textualOrEmpty(payload.get("currentTierId")), CurrentTierName: textualOrEmpty(payload.get("currentTierName")),
		TargetTierID: textualOrEmpty(payload.get("targetTierId")), TargetTierName: textualOrEmpty(payload.get("targetTierName")),
		MonthlyCreditsDelta: moneyOf(payload.get("monthlyCreditsDelta")), EffectiveAt: textualOrEmpty(payload.get("effectiveAt")),
	}
	if cents, ok := claimNumber(payload.get("amountDueNowCents")); ok {
		whole := int(math.Trunc(cents))
		out.AmountDueNowCents = &whole
	}
	return out
}

func SubscriptionStateFromPayload(raw json.RawMessage, portalURL string) (SubscriptionState, error) {
	return subscriptionStateFrom(objectOrEmpty(raw), portalURL)
}

func subscriptionStateFrom(payload *object, portalURL string) (SubscriptionState, error) {
	org, canChangePlan := parseOrgFields(payload)
	scope := payload.str("context")
	if scope != "personal" && scope != "team" {
		scope = "personal"
	}
	current := parseCurrentSubscription(payload.get("current"))
	tiers := []SubscriptionTier{}
	for _, item := range payload.list("tiers") {
		tier, ok, err := parseTier(item)
		if err != nil {
			return SubscriptionState{}, err
		}
		if ok {
			tiers = append(tiers, tier)
		}
	}
	return SubscriptionState{
		LoggedIn: true, OrgName: textualOrEmpty(org.get("name")), OrgID: textualOrEmpty(org.get("id")), Role: textualOrEmpty(org.get("role")),
		CanChangePlanRaw: canChangePlan, Context: scope, Current: current, Tiers: tiers, PortalURL: portalURL,
	}, nil
}

func (a *Account) BuildSubscriptionState(ctx context.Context, timeout time.Duration) (SubscriptionState, error) {
	if fixture, ok := devFixtureSubscriptionState(); ok {
		return fixture, nil
	}
	got := a.fetchPortalState(ctx, "subscription", a.SubscriptionState, func(base string) string { return base }, timeout)
	if got.failed {
		return SubscriptionState{Context: "personal", Error: got.failure}, nil
	}
	return subscriptionStateFrom(got.payload, got.portalURL)
}

func SubscriptionManageURL(state SubscriptionState, tier string) string {
	scheme, netloc := splitOrigin(state.PortalURL)
	if (scheme != "http" && scheme != "https") || netloc == "" {
		return ""
	}
	query := ""
	if u, ok := parsedURL(state.PortalURL); ok {
		query = u.RawQuery
	}
	type param struct{ key, value string }
	params := []param{}
	for _, piece := range strings.FieldsFunc(query, func(r rune) bool { return r == '&' }) {
		key, value, _ := strings.Cut(piece, "=")
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if decoded, err := url.QueryUnescape(value); err == nil {
			value = decoded
		}
		if key == "org_id" || key == "plan" {
			continue
		}
		if i := slices.IndexFunc(params, func(p param) bool { return p.key == key }); i >= 0 {
			params[i].value = value
			continue
		}
		params = append(params, param{key, value})
	}
	if state.OrgID != "" {
		params = append(params, param{"org_id", state.OrgID})
	}
	if tier != "" {
		params = append(params, param{"plan", tier})
	}
	encoded := make([]string, len(params))
	for i, p := range params {
		encoded[i] = url.QueryEscape(p.key) + "=" + url.QueryEscape(p.value)
	}
	out := scheme + "://" + netloc + "/manage-subscription"
	if len(encoded) > 0 {
		out += "?" + strings.Join(encoded, "&")
	}
	return out
}

func SelectableTiers(state SubscriptionState) []SubscriptionTier {
	out := []SubscriptionTier{}
	for _, t := range state.Tiers {
		if t.IsEnabled && !t.IsCurrent && t.TierOrder > 0 {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TierOrder < out[j].TierOrder })
	return out
}

func FormatTierRow(tier SubscriptionTier) (string, error) {
	dollars, err := FormatMoney(tier.DollarsPerMonth, true)
	if err != nil {
		return "", err
	}
	row := fmt.Sprintf("%s · %s/mo", tier.Name, dollars)
	if tier.MonthlyCredits == nil {
		return row, nil
	}
	sign, err := tier.MonthlyCredits.Sign()
	if err != nil {
		return "", err
	}
	if sign > 0 {
		credits, err := FormatMoney(tier.MonthlyCredits, true)
		if err != nil {
			return "", err
		}
		row += fmt.Sprintf(" · %s credits/mo", credits)
	}
	return row, nil
}

func IsUpgrade(state SubscriptionState, tier string) bool {
	orders := map[string]int{}
	for _, t := range state.Tiers {
		orders[t.TierID] = t.TierOrder
	}
	currentOrder, known := 0, false
	if state.Current != nil {
		currentOrder, known = orders[state.Current.TierID]
	}
	if !known {
		for _, t := range state.Tiers {
			if t.IsCurrent {
				currentOrder = t.TierOrder
				break
			}
		}
	}
	return orders[tier] > currentOrder
}

var devTierSpecs = []struct {
	id, name        string
	order           int
	dollars, credit string
}{{"free", "Free", 0, "0", "0"}, {"plus", "Plus", 1, "20", "1000"}, {"super", "Super", 2, "40", "3000"}, {"ultra", "Ultra", 3, "80", "7000"}}

func devTiers(current string) []SubscriptionTier {
	out := []SubscriptionTier{}
	for _, spec := range devTierSpecs {
		dollars, _ := ParseMoney(spec.dollars)
		credits, _ := ParseMoney(spec.credit)
		out = append(out, SubscriptionTier{TierID: spec.id, Name: spec.name, TierOrder: spec.order, DollarsPerMonth: dollars,
			MonthlyCredits: credits, IsCurrent: spec.id == current, IsEnabled: true})
	}
	return out
}

func devPlan(state *SubscriptionState, tier, remaining string) *CurrentSubscription {
	for _, spec := range devTierSpecs {
		if spec.id == tier {
			credits, _ := ParseMoney(spec.credit)
			left, _ := ParseMoney(remaining)
			state.Current = &CurrentSubscription{TierID: spec.id, TierName: spec.name, MonthlyCredits: credits, CreditsRemaining: left, CycleEndsAt: "2026-07-01"}
			state.Tiers = devTiers(spec.id)
		}
	}
	return state.Current
}

func devFixtureSubscriptionState() (SubscriptionState, bool) {
	name := fixtureName(EnvDevSubscriptionFixture, map[string]string{"logged_out": "logged-out", "loggedout": "logged-out",
		"mid-tier": "mid", "top-tier": "top", "member": "not-admin"})
	if name == "" {
		return SubscriptionState{}, false
	}
	if name == "logged-out" {
		return SubscriptionState{Context: "personal"}, true
	}
	state := SubscriptionState{LoggedIn: true, OrgName: "Acme Inc", OrgID: "org_acme", Role: "OWNER", Context: "personal", PortalURL: devFixturePortal, Tiers: []SubscriptionTier{}}
	switch name {
	case "free":
		state.Tiers = devTiers("")
	case "mid":
		devPlan(&state, "plus", "420")
	case "top":
		devPlan(&state, "ultra", "5000")
	case "not-admin":
		devPlan(&state, "plus", "420")
		state.Role = "MEMBER"
	case "downgrade":
		current := devPlan(&state, "super", "1500")
		current.PendingDowngradeTierName, current.PendingDowngradeAt = "Plus", "2026-07-15"
	case "cancel":
		current := devPlan(&state, "plus", "420")
		current.CancelAtPeriodEnd, current.CancellationEffectiveAt = true, "2026-07-01"
	case "team":
		state.Context, state.OrgName, state.OrgID = "team", "Acme Engineering", "org_eng"
	default:
		return SubscriptionState{Context: "personal", Error: fmt.Sprintf("unknown %s: %s", EnvDevSubscriptionFixture, name)}, true
	}
	return state, true
}

type BillingBlock struct {
	Provider      string `json:"provider"`
	ProviderLabel string `json:"provider_label"`
	Model         string `json:"model"`
	BillingURL    string `json:"billing_url"`
	IsPortal      bool   `json:"is_portal"`
	Message       string `json:"message"`
}

func (b BillingBlock) MarshalJSON() ([]byte, error) {
	return pairs("provider", b.Provider, "provider_label", b.ProviderLabel, "model", b.Model,
		"billing_url", nullable(b.BillingURL), "is_portal", b.IsPortal, "message", b.Message).MarshalJSON()
}

type billingProvider struct {
	label string
	link  string
	slugs []string
	hosts []string
}

var billingProviders = []billingProvider{
	{"OpenAI", "https://platform.openai.com/settings/organization/billing", []string{"openai"}, []string{"api.openai.com"}},
	{"Anthropic", "https://console.anthropic.com/settings/billing", []string{"anthropic"}, []string{"api.anthropic.com"}},
	{"OpenRouter", "https://openrouter.ai/settings/credits", []string{"openrouter"}, []string{"openrouter.ai"}},
	{"xAI", "https://console.x.ai/team/default/billing", []string{"xai", "xai-oauth"}, []string{"api.x.ai"}},
	{"DeepSeek", "https://platform.deepseek.com/top_up", []string{"deepseek"}, []string{"api.deepseek.com"}},
	{"Groq", "https://console.groq.com/settings/billing", []string{"groq"}, []string{"api.groq.com"}},
	{"Mistral", "https://console.mistral.ai/billing", []string{"mistral"}, []string{"api.mistral.ai"}},
	{"Together AI", "https://api.together.ai/settings/billing", []string{"together"}, []string{"api.together.ai", "api.together.xyz"}},
	{"Fireworks AI", "https://fireworks.ai/account/billing", []string{"fireworks"}, []string{"fireworks.ai"}},
	{"Perplexity", "https://www.perplexity.ai/settings/api", []string{"perplexity"}, []string{"perplexity.ai"}},
	{"Google AI", "https://aistudio.google.com/app/billing", []string{"google", "gemini"}, []string{"generativelanguage.googleapis.com"}},
	{"Cohere", "https://dashboard.cohere.com/billing", []string{"cohere"}, nil},
	{"Moonshot AI", "https://platform.moonshot.ai/console/pay", []string{"moonshot"}, nil},
	{"NVIDIA", "https://build.nvidia.com/settings/billing", []string{"nvidia"}, nil},
}

func baseHostMatches(baseURL, domain string) bool {
	raw := strings.TrimSpace(baseURL)
	if raw != "" && !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	host := strings.TrimRight(hostOf(raw), ".")
	domain = strings.TrimRight(strings.ToLower(strings.TrimSpace(domain)), ".")
	return host != "" && domain != "" && (host == domain || strings.HasSuffix(host, "."+domain))
}

func IsPortalInferenceRoute(provider, baseURL string) bool {
	return strings.ToLower(strings.TrimSpace(provider)) == Provider || baseHostMatches(baseURL, hostOf(auth.DefaultPortalInferenceURL))
}

func titleWords(text string) string {
	runes := []rune(text)
	inWord := false
	for i, r := range runes {
		switch {
		case !unicode.IsLetter(r):
			inWord = false
		case inWord:
			runes[i] = unicode.ToLower(r)
		default:
			runes[i], inWord = unicode.ToTitle(r), true
		}
	}
	return string(runes)
}

func BuildBillingBlock(provider, baseURL, model, message string) BillingBlock {
	slug := strings.ToLower(strings.TrimSpace(provider))
	model = strings.TrimSpace(model)
	if IsPortalInferenceRoute(slug, baseURL) {
		if slug == "" {
			slug = Provider
		}
		return BillingBlock{Provider: slug, ProviderLabel: portalDisplayName, Model: model, BillingURL: BillingURL(nil), IsPortal: true, Message: message}
	}
	for _, p := range billingProviders {
		if slices.Contains(p.slugs, slug) {
			return BillingBlock{Provider: slug, ProviderLabel: p.label, Model: model, BillingURL: p.link, Message: message}
		}
	}
	for _, p := range billingProviders {
		if slices.ContainsFunc(p.hosts, func(host string) bool { return baseHostMatches(baseURL, host) }) {
			return BillingBlock{Provider: slug, ProviderLabel: p.label, Model: model, BillingURL: p.link, Message: message}
		}
	}
	label := titleWords(strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(slug)))
	if label == "" {
		label = "your provider"
	}
	return BillingBlock{Provider: slug, ProviderLabel: label, Model: model, Message: message}
}
