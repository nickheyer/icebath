package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// DevBillingFixtureEnv names the offline billing screen fixture
const DevBillingFixtureEnv = "ICEBATH_DEV_BILLING_FIXTURE"

// PortalStateTimeout is the default wait for the billing and subscription overview fetches
const PortalStateTimeout = 15 * time.Second

// Billing and subscription overview fetchers, swappable so tests can script the portal
var (
	nousBillingStateFetch      = GetNousBillingState
	nousSubscriptionStateFetch = GetNousSubscriptionState
)

// Python str of a server money value, false where str would never read as a Decimal
func moneyText(value any) (string, bool) {
	switch t := value.(type) {
	case string:
		return t, true
	case json.Number:
		s := string(t)
		if strings.ContainsAny(s, ".eE") {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil && !errors.Is(err, strconv.ErrRange) {
				return "", false
			}
			return pyFloatRepr(f), true
		}
		if n, ok := new(big.Int).SetString(s, 10); ok {
			return n.String(), true
		}
		return s, true
	case float64:
		return pyFloatRepr(t), true
	case int:
		return strconv.Itoa(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case PyDecimal:
		return t.String(), true
	case *PyDecimal:
		if t != nil {
			return t.String(), true
		}
	}
	return "", false
}

// ParseMoney reads a server money value as an exact Decimal, nil when absent or unreadable
func ParseMoney(value any) *PyDecimal {
	text, ok := moneyText(value)
	if !ok {
		return nil
	}
	d, err := ParseDecimal(pyStripSpace(text))
	if err != nil {
		return nil
	}
	return &d
}

// MustDecimal is the Decimal of literal text, panicking on malformed text
func MustDecimal(text string) PyDecimal {
	d, err := ParseDecimal(text)
	if err != nil {
		panic(fmt.Sprintf("decimal literal %q: %v", text, err))
	}
	return d
}

// FormatMoney renders whole dollars as $X and others to exactly two places, an em dash for nil
func FormatMoney(value *PyDecimal, grouped bool) (string, error) {
	if value == nil {
		return "—", nil
	}
	integral, err := value.ToIntegralValue()
	if err != nil {
		return "", err
	}
	whole, err := value.Equal(integral)
	if err != nil {
		return "", err
	}
	if !whole {
		if integral, err = value.Quantize(-2); err != nil {
			return "", err
		}
	}
	text, err := integral.FormatFixed(grouped)
	if err != nil {
		return "", err
	}
	return "$" + text, nil
}

// Value of a string field, nil when the field is missing or not a string
func optionalStr(raw map[string]any, key string) *string {
	if s, ok := raw[key].(string); ok {
		return &s
	}
	return nil
}

var cardProvenanceLabels = map[string]string{
	"subPin":          "the card on your subscription",
	"customerDefault": "your default card saved on the portal",
	"autoRefill":      "your auto-reload card",
}

// CardInfo is the card on file with the server ladder rung that picked it
type CardInfo struct {
	Brand       string
	Last4       string
	ResolvedVia *string
}

// Masked is brand and masked last four, the brand alone for a Link method with no number
func (c CardInfo) Masked() string {
	if c.Last4 != "" {
		return c.Brand + " ····" + c.Last4
	}
	return c.Brand
}

// Provenance labels why this card was picked, empty for an unknown rung or an old server
func (c CardInfo) Provenance() string {
	if c.ResolvedVia == nil {
		return ""
	}
	return cardProvenanceLabels[*c.ResolvedVia]
}

// Display is the masked card followed by its provenance when known
func (c CardInfo) Display() string {
	if label := c.Provenance(); label != "" {
		return c.Masked() + " — " + label
	}
	return c.Masked()
}

// PaymentMethodInfo is the payment method on file settled to card, link or unknown at parse time
type PaymentMethodInfo struct {
	Kind        string
	Brand       *string
	Last4       *string
	Wallet      *string
	Email       *string
	ResolvedVia *string
	RawKind     *string
}

// MonthlyCap is the monthly spend ceiling and this month's spend
type MonthlyCap struct {
	LimitUSD          *PyDecimal
	SpentThisMonthUSD *PyDecimal
	IsDefaultCeiling  bool
}

// AutoReloadCard is the auto reload card kind canonical, distinct or none
type AutoReloadCard struct {
	Kind            string
	PaymentMethodID *string
	Brand           *string
	Last4           *string
}

// AutoReload is the auto reload setting with its threshold, target and card
type AutoReload struct {
	Enabled      bool
	ThresholdUSD *PyDecimal
	ReloadToUSD  *PyDecimal
	Card         *AutoReloadCard
}

// OrgRoleCapability is the org role and server plan change capability shared by billing and subscription states
type OrgRoleCapability struct {
	Role             any
	CanChangePlanRaw *bool
}

// IsAdmin is the legacy OWNER or ADMIN display check, an AttributeError for a truthy non string role
func (o OrgRoleCapability) IsAdmin() (bool, error) {
	if !auTruthy(o.Role) {
		return false, nil
	}
	role, ok := o.Role.(string)
	if !ok {
		return false, fmt.Errorf("'%s' object has no attribute 'upper'", auTypeName(o.Role))
	}
	upper := strings.ToUpper(role)
	return upper == "OWNER" || upper == "ADMIN", nil
}

// CanChangePlan is the server capability when supplied and the legacy role check otherwise
func (o OrgRoleCapability) CanChangePlan() (bool, error) {
	if o.CanChangePlanRaw != nil {
		return *o.CanChangePlanRaw, nil
	}
	return o.IsAdmin()
}

// BillingState is the parsed billing overview, LoggedIn false with empty fields when unavailable
type BillingState struct {
	OrgRoleCapability
	LoggedIn           bool
	OrgID              any
	OrgSlug            any
	OrgName            any
	BalanceUSD         *PyDecimal
	CLIBillingEnabled  bool
	ChargePresets      []PyDecimal
	MinUSD             *PyDecimal
	MaxUSD             *PyDecimal
	Card               *CardInfo
	PaymentMethod      *PaymentMethodInfo
	MonthlyCap         *MonthlyCap
	AutoReload         *AutoReload
	KeepGoingOnCredits *bool
	PortalURL          any
	Error              *string
}

// CanCharge offers charge and auto reload actions when the plan may change and the org kill switch is on
func (s *BillingState) CanCharge() (bool, error) {
	can, err := s.CanChangePlan()
	if err != nil || !can {
		return false, err
	}
	return s.CLIBillingEnabled, nil
}

func parseCard(value any) *CardInfo {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	brand, okBrand := raw["brand"].(string)
	last4, okLast4 := raw["last4"].(string)
	if !okBrand || !okLast4 {
		return nil
	}
	return &CardInfo{Brand: brand, Last4: last4, ResolvedVia: optionalStr(raw, "resolvedVia")}
}

func parsePaymentMethod(value any) *PaymentMethodInfo {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	kind, ok := raw["kind"].(string)
	if !ok {
		return nil
	}
	resolvedVia, brand, last4 := optionalStr(raw, "resolvedVia"), optionalStr(raw, "brand"), optionalStr(raw, "last4")
	if kind == "card" && brand != nil && *brand != "" && last4 != nil && *last4 != "" {
		return &PaymentMethodInfo{Kind: "card", Brand: brand, Last4: last4, Wallet: optionalStr(raw, "wallet"), ResolvedVia: resolvedVia}
	}
	if kind == "link" {
		return &PaymentMethodInfo{Kind: "link", Email: optionalStr(raw, "email"), ResolvedVia: resolvedVia}
	}
	return &PaymentMethodInfo{Kind: "unknown", RawKind: &kind, ResolvedVia: resolvedVia}
}

func parseMonthlyCap(value any) *MonthlyCap {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &MonthlyCap{LimitUSD: ParseMoney(raw["limitUsd"]), SpentThisMonthUSD: ParseMoney(raw["spentThisMonthUsd"]),
		IsDefaultCeiling: auTruthy(raw["isDefaultCeiling"])}
}

func parseAutoReload(value any) *AutoReload {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return &AutoReload{Enabled: auTruthy(raw["enabled"]), ThresholdUSD: ParseMoney(raw["thresholdUsd"]),
		ReloadToUSD: ParseMoney(raw["reloadToUsd"]), Card: parseAutoReloadCard(raw["card"])}
}

func parseAutoReloadCard(value any) *AutoReloadCard {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	kind, _ := raw["kind"].(string)
	switch kind {
	case "canonical", "none":
		return &AutoReloadCard{Kind: kind}
	case "distinct":
		return &AutoReloadCard{Kind: kind, PaymentMethodID: optionalStr(raw, "paymentMethodId"),
			Brand: optionalStr(raw, "brand"), Last4: optionalStr(raw, "last4")}
	}
	return nil
}

// ParseOrgFields returns the org object or an empty map and canChangePlan when it is a bool
func ParseOrgFields(payload map[string]any) (map[string]any, *bool) {
	org, ok := payload["org"].(map[string]any)
	if !ok {
		org = map[string]any{}
	}
	if ccp, ok := payload["canChangePlan"].(bool); ok {
		return org, &ccp
	}
	return org, nil
}

// Items Python iteration yields for a value, characters of a string and keys of an object in sorted order
func pyIterItems(value any) ([]any, error) {
	switch t := value.(type) {
	case []any:
		return t, nil
	case string:
		out := make([]any, 0, utf8.RuneCountInString(t))
		for _, r := range t {
			out = append(out, string(r))
		}
		return out, nil
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]any, len(keys))
		for i, k := range keys {
			out[i] = k
		}
		return out, nil
	}
	return nil, fmt.Errorf("'%s' object is not iterable", auTypeName(value))
}

// BillingStateFromPayload maps the billing state JSON object into a BillingState
func BillingStateFromPayload(payload map[string]any, portalURL any) (*BillingState, error) {
	org, canChangePlanRaw := ParseOrgFields(payload)
	bounds, ok := payload["bounds"].(map[string]any)
	if !ok {
		bounds = map[string]any{}
	}
	var presets []PyDecimal
	if raw := payload["chargePresets"]; auTruthy(raw) {
		items, err := pyIterItems(raw)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if p := ParseMoney(item); p != nil {
				presets = append(presets, *p)
			}
		}
	}
	return &BillingState{
		OrgRoleCapability:  OrgRoleCapability{Role: org["role"], CanChangePlanRaw: canChangePlanRaw},
		LoggedIn:           true,
		OrgID:              org["id"],
		OrgSlug:            org["slug"],
		OrgName:            org["name"],
		BalanceUSD:         ParseMoney(payload["balanceUsd"]),
		CLIBillingEnabled:  auTruthy(payload["cliBillingEnabled"]),
		ChargePresets:      presets,
		MinUSD:             ParseMoney(bounds["minUsd"]),
		MaxUSD:             ParseMoney(bounds["maxUsd"]),
		Card:               parseCard(payload["card"]),
		PaymentMethod:      parsePaymentMethod(payload["paymentMethod"]),
		MonthlyCap:         parseMonthlyCap(payload["monthlyCap"]),
		AutoReload:         parseAutoReload(payload["autoReload"]),
		KeepGoingOnCredits: parseKeepGoingOnCredits(payload["keepGoingOnCredits"]),
		PortalURL:          portalURL,
	}, nil
}

// Enabled flag of the keep going object, nil for anything but a bool inside an object
func parseKeepGoingOnCredits(value any) *bool {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if enabled, ok := raw["enabled"].(bool); ok {
		return &enabled
	}
	return nil
}

// Fail open overview fetch, logged out on auth failure, logged out with an error on any other failure
func fetchPortalState[T any](ctx context.Context, fetch func(PoolEnv, time.Duration) (any, error), label string,
	failed func(errText *string) T, parse func(payload map[string]any, portalURL any) (T, error),
	portalFallback func(base string) string, timeout time.Duration) (T, error) {
	env := modelsPoolEnv(ctx)
	payload, err := fetch(env, timeout)
	if err != nil {
		var be *BillingError
		if errors.As(err, &be) {
			if be.Kind.IsAuth() {
				return failed(nil), nil
			}
			env.log().Debug(label+" ▸ /state fetch failed (fail-open)", "error", err)
			text := be.Error()
			return failed(&text), nil
		}
		env.log().Debug(label+" ▸ /state unexpected error (fail-open)", "error", err)
		text := "could not load " + label + " state"
		return failed(&text), nil
	}
	obj, isObj := payload.(map[string]any)
	var portalURL any
	if isObj && auTruthy(obj["portalUrl"]) {
		if portalURL, err = AbsolutizeNousBillingPortalURL(env, obj["portalUrl"]); err != nil {
			var zero T
			return zero, err
		}
	}
	if !auTruthy(portalURL) {
		portalURL = portalFallback(ResolveNousBillingPortalBaseURL(env, nil))
	}
	if !isObj {
		var zero T
		return zero, fmt.Errorf("'%s' object has no attribute 'get'", auTypeName(payload))
	}
	return parse(obj, portalURL)
}

// BuildBillingState fetches and parses the billing overview fail open, the dev fixture short circuiting
func BuildBillingState(ctx context.Context, timeout time.Duration) (*BillingState, error) {
	if fixture := devFixtureBillingState(); fixture != nil {
		return fixture, nil
	}
	return fetchPortalState(ctx, nousBillingStateFetch, "billing",
		func(errText *string) *BillingState { return &BillingState{Error: errText} },
		BillingStateFromPayload,
		func(base string) string { return strings.TrimRight(base, "/") + "/billing?topup=open" }, timeout)
}

var billingFixtureAliases = map[string]string{
	"logged_out": "logged-out", "loggedout": "logged-out", "card_sub": "card-sub", "card_autoreload": "card-autoreload",
	"autoreload": "card-autoreload", "not-admin": "notadmin", "member": "notadmin", "billing_off": "billing-off", "off": "billing-off",
}

func decPtr(d PyDecimal) *PyDecimal { return &d }

// Billing state of the named offline fixture, nil when unset and logged out with an error for an unknown name
func devFixtureBillingState() *BillingState {
	name := pyLower(pyStripSpace(os.Getenv(DevBillingFixtureEnv)))
	if name == "" {
		return nil
	}
	if alias, ok := billingFixtureAliases[name]; ok {
		name = alias
	}
	if name == "logged-out" {
		return &BillingState{}
	}
	s := &BillingState{
		OrgRoleCapability: OrgRoleCapability{Role: "OWNER"},
		LoggedIn:          true, OrgID: "org_acme", OrgSlug: "acme", OrgName: "Acme Inc",
		BalanceUSD: decPtr(MustDecimal("3.40")), CLIBillingEnabled: true, MinUSD: decPtr(DecimalFromInt(5)),
		MaxUSD:        decPtr(DecimalFromInt(500)),
		ChargePresets: []PyDecimal{DecimalFromInt(10), DecimalFromInt(25), DecimalFromInt(50)},
		PortalURL:     "https://portal.nousresearch.com/billing?topup=open",
	}
	card := &CardInfo{Brand: "Visa", Last4: "4242"}
	switch name {
	case "nocard":
	case "card":
		s.Card = card
	case "card-sub":
		via := "subPin"
		s.Card = &CardInfo{Brand: "Visa", Last4: "4242", ResolvedVia: &via}
	case "card-autoreload":
		s.Card = card
		s.AutoReload = &AutoReload{Enabled: true, ThresholdUSD: decPtr(DecimalFromInt(5)), ReloadToUSD: decPtr(DecimalFromInt(25))}
	case "notadmin":
		s.Card, s.Role = card, "MEMBER"
	case "billing-off":
		s.CLIBillingEnabled = false
	default:
		text := "unknown " + DevBillingFixtureEnv + ": " + name
		return &BillingState{Error: &text}
	}
	return s
}

// NewIdempotencyKey is a fresh UUID4 key reused across retries of the same purchase
func NewIdempotencyKey() string { return uuid.NewString() }

// AmountValidation is the outcome of checking a custom charge amount
type AmountValidation struct {
	OK     bool
	Amount *PyDecimal
	Error  string
}

// ValidateChargeAmount mirrors the server bounds and whole cent rule, an error where Python decimal raises
func ValidateChargeAmount(raw string, minUSD, maxUSD *PyDecimal) (AmountValidation, error) {
	amount := ParseMoney(pyStripSpace(strings.TrimLeft(pyStripSpace(raw), "$")))
	reject := func(msg string) (AmountValidation, error) { return AmountValidation{Error: msg}, nil }
	if amount == nil {
		return reject("Enter a dollar amount, e.g. 100")
	}
	sign, err := amount.Cmp(DecimalFromInt(0))
	if err != nil {
		return AmountValidation{}, err
	}
	if sign <= 0 {
		return reject("Amount must be greater than $0")
	}
	cents, err := amount.Quantize(-2)
	if err != nil {
		return AmountValidation{}, err
	}
	if same, err := amount.Equal(cents); err != nil || !same {
		if err != nil {
			return AmountValidation{}, err
		}
		return reject("Amount can't be smaller than a cent")
	}
	for _, bound := range []struct {
		limit *PyDecimal
		below bool
		label string
	}{{minUSD, true, "Minimum"}, {maxUSD, false, "Maximum"}} {
		if bound.limit == nil {
			continue
		}
		c, err := amount.Cmp(*bound.limit)
		if err != nil {
			return AmountValidation{}, err
		}
		if (bound.below && c < 0) || (!bound.below && c > 0) {
			text, err := FormatMoney(bound.limit, false)
			if err != nil {
				return AmountValidation{}, err
			}
			return reject(bound.label + " is " + text)
		}
	}
	return AmountValidation{OK: true, Amount: amount}, nil
}
