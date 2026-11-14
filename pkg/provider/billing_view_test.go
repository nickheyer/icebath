package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func bvJSON(t *testing.T, text string) map[string]any {
	t.Helper()
	v, err := decodeExactJSON(text)
	if err != nil {
		t.Fatalf("decode %s: %v", text, err)
	}
	return v.(map[string]any)
}

func bvDecEq(t *testing.T, label string, got *PyDecimal, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %s", label, want)
	}
	eq, err := got.Equal(MustDecimal(want))
	if err != nil || !eq {
		t.Fatalf("%s = %s, want %s (%v)", label, got, want, err)
	}
}

const bvMemberPayload = `{"org": {"id": "o1", "slug": "acme", "name": "Acme", "role": "MEMBER"}, "balanceUsd": "142.5",
 "cliBillingEnabled": true, "chargePresets": ["100", "250", "500"], "bounds": {"minUsd": "10", "maxUsd": "10000"},
 "card": null, "monthlyCap": null, "autoReload": null}`

func bvOwnerPayload(t *testing.T) map[string]any {
	p := bvJSON(t, bvMemberPayload)
	p["org"].(map[string]any)["role"] = "OWNER"
	p["card"] = map[string]any{"brand": "visa", "last4": "4242"}
	p["monthlyCap"] = bvJSON(t, `{"limitUsd": "1000", "spentThisMonthUsd": "180", "isDefaultCeiling": true}`)
	p["autoReload"] = bvJSON(t, `{"enabled": true, "thresholdUsd": "20", "reloadToUsd": "100"}`)
	return p
}

func bvMustState(t *testing.T, payload map[string]any) *BillingState {
	t.Helper()
	s, err := BillingStateFromPayload(payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bvOK(t *testing.T) func(bool, error) bool {
	return func(got bool, err error) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
}

func TestBillingViewKeepGoingOnCreditsParsesBoolsOnly(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		raw  any
		want *bool
	}{{map[string]any{"enabled": true}, &yes}, {map[string]any{"enabled": false}, &no}, {nil, nil},
		{map[string]any{"enabled": "true"}, nil}, {map[string]any{}, nil}, {true, nil}} {
		p := bvOwnerPayload(t)
		p["keepGoingOnCredits"] = c.raw
		got := bvMustState(t, p).KeepGoingOnCredits
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Fatalf("%v: got %v want %v", c.raw, got, c.want)
		}
	}
}

func TestBillingViewMemberTierParse(t *testing.T) {
	s := bvMustState(t, bvJSON(t, bvMemberPayload))
	if s.KeepGoingOnCredits != nil || !s.LoggedIn || s.Role != "MEMBER" || !s.CLIBillingEnabled {
		t.Fatalf("state %+v", s)
	}
	bvDecEq(t, "balance", s.BalanceUSD, "142.5")
	if len(s.ChargePresets) != 3 {
		t.Fatalf("presets %v", s.ChargePresets)
	}
	for i, want := range []string{"100", "250", "500"} {
		bvDecEq(t, "preset", &s.ChargePresets[i], want)
	}
	bvDecEq(t, "min", s.MinUSD, "10")
	bvDecEq(t, "max", s.MaxUSD, "10000")
	if s.Card != nil || s.MonthlyCap != nil || s.AutoReload != nil {
		t.Fatalf("sub structs %+v", s)
	}
	if bvOK(t)(s.IsAdmin()) || bvOK(t)(s.CanCharge()) {
		t.Fatal("member must not be admin or charge")
	}
}

func TestBillingViewFiveRoles(t *testing.T) {
	yes := true
	for _, c := range []struct {
		role                     string
		raw                      *bool
		isAdmin, canChangePlanOK bool
	}{{"OWNER", nil, true, true}, {"ADMIN", nil, true, true}, {"FINANCE_ADMIN", &yes, false, true},
		{"SECURITY_ADMIN", nil, false, false}, {"MEMBER", nil, false, false}} {
		p := bvJSON(t, bvMemberPayload)
		p["org"].(map[string]any)["role"] = c.role
		if c.raw != nil {
			p["canChangePlan"] = *c.raw
		}
		s := bvMustState(t, p)
		if bvOK(t)(s.IsAdmin()) != c.isAdmin || bvOK(t)(s.CanChangePlan()) != c.canChangePlanOK {
			t.Fatalf("%s: admin/change mismatch", c.role)
		}
		if (s.CanChangePlanRaw == nil) != (c.raw == nil) || (c.raw != nil && *s.CanChangePlanRaw != *c.raw) {
			t.Fatalf("%s: raw %v", c.role, s.CanChangePlanRaw)
		}
		if c.role == "SECURITY_ADMIN" && (s.Card != nil || s.MonthlyCap != nil || s.AutoReload != nil) {
			t.Fatal("security admin sees no card, cap or reload")
		}
	}
}

func TestBillingViewOwnerTierParse(t *testing.T) {
	s := bvMustState(t, bvOwnerPayload(t))
	if !bvOK(t)(s.IsAdmin()) || !bvOK(t)(s.CanCharge()) {
		t.Fatal("owner admin and can charge")
	}
	if s.Card == nil || s.Card.Brand != "visa" || s.Card.Last4 != "4242" || s.Card.ResolvedVia != nil || s.Card.Masked() != "visa ····4242" {
		t.Fatalf("card %+v", s.Card)
	}
	bvDecEq(t, "limit", s.MonthlyCap.LimitUSD, "1000")
	bvDecEq(t, "spent", s.MonthlyCap.SpentThisMonthUSD, "180")
	if !s.MonthlyCap.IsDefaultCeiling {
		t.Fatal("default ceiling")
	}
	if !s.AutoReload.Enabled || s.AutoReload.Card != nil {
		t.Fatalf("auto reload %+v", s.AutoReload)
	}
	bvDecEq(t, "threshold", s.AutoReload.ThresholdUSD, "20")
	bvDecEq(t, "reload", s.AutoReload.ReloadToUSD, "100")
}

func bvBillingErr(t *testing.T, status int, payload string, h http.Header) *BillingError {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "")
	t.Setenv("NOUS_PORTAL_BASE_URL", "")
	var be *BillingError
	if err := nousBillingErrorFor(nousProcessEnv(), status, bvJSON(t, payload), h); !errors.As(err, &be) {
		t.Fatalf("not a billing error: %v", err)
	}
	return be
}

func TestBillingView401MapsToAuthError(t *testing.T) {
	be := bvBillingErr(t, 401, `{"error": "invalid_token"}`, nil)
	if !be.Kind.IsAuth() || be.Status != 401 {
		t.Fatalf("%+v", be)
	}
}

func TestBillingView403InsufficientScopeMapsToScopeRequired(t *testing.T) {
	be := bvBillingErr(t, 403, `{"error": "insufficient_scope", "portalUrl": "/billing"}`, nil)
	url, _ := be.PortalURL.(string)
	if be.Kind != BillingErrorScopeRequired || be.ErrorCode != "insufficient_scope" || !strings.HasPrefix(url, "http") || !strings.HasSuffix(url, "/billing") {
		t.Fatalf("%+v", be)
	}
}

func TestBillingViewSpecificThrottleErrorsRemainDistinguishable(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
		kind   BillingErrorKind
	}{{503, "stripe_unavailable", BillingErrorStripeUnavailable}, {429, "upgrade_cap_exceeded", BillingErrorUpgradeCapExceeded}} {
		be := bvBillingErr(t, c.status, `{"error": "`+c.code+`"}`, http.Header{"Retry-After": {"90"}})
		if be.Kind != c.kind || be.ErrorCode != c.code || be.RetryAfter == nil || *be.RetryAfter != 90 ||
			!be.Kind.IsTransient() || be.Kind == BillingErrorRateLimited {
			t.Fatalf("%s: %+v", c.code, be)
		}
	}
}

func TestBillingView400AmountOutOfBoundsIsBaseError(t *testing.T) {
	be := bvBillingErr(t, 400, `{"error": "amount_out_of_bounds", "message": "too big"}`, nil)
	if be.Status != 400 || !strings.Contains(be.Error(), "too big") {
		t.Fatalf("%+v", be)
	}
}

func TestBillingViewPortalBaseURLDefault(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "")
	t.Setenv("NOUS_PORTAL_BASE_URL", "")
	if got := ResolveNousBillingPortalBaseURL(nousProcessEnv(), nil); got != DefaultNousPortalURL {
		t.Fatalf("got %q", got)
	}
}

func bvSwapFetch(t *testing.T, fn func(PoolEnv, time.Duration) (any, error)) {
	old := nousBillingStateFetch
	nousBillingStateFetch = fn
	t.Cleanup(func() { nousBillingStateFetch = old })
}

func TestBillingViewBuildFailOpenOnHTTPError(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(DevBillingFixtureEnv, "")
	bvSwapFetch(t, func(PoolEnv, time.Duration) (any, error) {
		return nil, &BillingError{Message: "portal exploded", Status: 500}
	})
	s, err := BuildBillingState(t.Context(), PortalStateTimeout)
	if err != nil || s.LoggedIn || s.Error == nil || !strings.Contains(*s.Error, "portal exploded") {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestBillingViewBuildFailOpenOutcomes(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	t.Setenv(DevBillingFixtureEnv, "")
	t.Setenv("ICEBATH_PORTAL_BASE_URL", "")
	t.Setenv("NOUS_PORTAL_BASE_URL", "")
	bvSwapFetch(t, func(PoolEnv, time.Duration) (any, error) {
		return nil, &BillingError{Kind: BillingErrorSessionRevoked, Message: "x"}
	})
	if s, err := BuildBillingState(t.Context(), PortalStateTimeout); err != nil || s.LoggedIn || s.Error != nil {
		t.Fatalf("auth failure is a bare logged out state: %+v %v", s, err)
	}
	bvSwapFetch(t, func(PoolEnv, time.Duration) (any, error) { return nil, errors.New("boom") })
	if s, _ := BuildBillingState(t.Context(), PortalStateTimeout); s.Error == nil || *s.Error != "could not load billing state" {
		t.Fatalf("unexpected error text: %+v", s)
	}
	bvSwapFetch(t, func(PoolEnv, time.Duration) (any, error) { return map[string]any{"portalUrl": "/billing?x=1"}, nil })
	if s, _ := BuildBillingState(t.Context(), PortalStateTimeout); s.PortalURL != "https://portal.nousresearch.com/billing?x=1" || !s.LoggedIn {
		t.Fatalf("relative portal url: %+v", s.PortalURL)
	}
	bvSwapFetch(t, func(PoolEnv, time.Duration) (any, error) { return map[string]any{}, nil })
	if s, _ := BuildBillingState(t.Context(), PortalStateTimeout); s.PortalURL != "https://portal.nousresearch.com/billing?topup=open" {
		t.Fatalf("fallback portal url: %+v", s.PortalURL)
	}
	bvSwapFetch(t, func(PoolEnv, time.Duration) (any, error) { return []any{}, nil })
	if _, err := BuildBillingState(t.Context(), PortalStateTimeout); err == nil || err.Error() != "'list' object has no attribute 'get'" {
		t.Fatalf("non object payload: %v", err)
	}
}

func TestBillingViewNewIdempotencyKeyUniqueAndUUIDShaped(t *testing.T) {
	a, b := NewIdempotencyKey(), NewIdempotencyKey()
	if a == b || len(a) != 36 || strings.Count(a, "-") != 4 {
		t.Fatalf("%q %q", a, b)
	}
}

func TestBillingViewValidateAmountRejections(t *testing.T) {
	lo, hi := DecimalFromInt(10), DecimalFromInt(10000)
	for _, c := range [][2]string{{"", "dollar amount"}, {"0", "greater than"}, {"-5", "greater than"}, {"10.005", "cent"},
		{"5", "Minimum"}, {"99999", "Maximum"}} {
		v, err := ValidateChargeAmount(c[0], &lo, &hi)
		if err != nil || v.OK || !strings.Contains(strings.ToLower(v.Error), strings.ToLower(c[1])) {
			t.Fatalf("%q: %+v %v", c[0], v, err)
		}
	}
	v, err := ValidateChargeAmount(" $$ 1_000.50 ", &lo, &hi)
	if err != nil || !v.OK {
		t.Fatalf("accepted amount: %+v %v", v, err)
	}
	bvDecEq(t, "amount", v.Amount, "1000.5")
	if v, _ := ValidateChargeAmount("5", &lo, &hi); v.Error != "Minimum is $10" {
		t.Fatalf("minimum text %q", v.Error)
	}
	for _, raw := range []string{"nan", "inf", "1e30"} {
		if _, err := ValidateChargeAmount(raw, &lo, &hi); !errors.Is(err, ErrDecimalInvalidOperation) {
			t.Fatalf("%s must raise InvalidOperation, got %v", raw, err)
		}
	}
	if v, err := ValidateChargeAmount("-inf", &lo, &hi); err != nil || v.Error != "Amount must be greater than $0" {
		t.Fatalf("-inf: %+v %v", v, err)
	}
}

func TestBillingViewFixtureCardAndGateInvariants(t *testing.T) {
	for _, c := range []struct {
		name                     string
		hasCard, isAdmin, billOn bool
	}{{"nocard", false, true, true}, {"card", true, true, true}, {"card-autoreload", true, true, true},
		{"notadmin", true, false, true}, {"billing-off", false, true, false}} {
		t.Setenv(DevBillingFixtureEnv, c.name)
		s, err := BuildBillingState(t.Context(), PortalStateTimeout)
		if err != nil || !s.LoggedIn || (s.Card != nil) != c.hasCard || bvOK(t)(s.IsAdmin()) != c.isAdmin || s.CLIBillingEnabled != c.billOn {
			t.Fatalf("%s: %+v %v", c.name, s, err)
		}
	}
	t.Setenv(DevBillingFixtureEnv, " Card_Sub ")
	if s, _ := BuildBillingState(t.Context(), PortalStateTimeout); s.Card.Display() != "Visa ····4242 — the card on your subscription" {
		t.Fatalf("card sub display %q", s.Card.Display())
	}
	t.Setenv(DevBillingFixtureEnv, "bogus")
	if s, _ := BuildBillingState(t.Context(), PortalStateTimeout); s.LoggedIn || s.Error == nil || *s.Error != "unknown ICEBATH_DEV_BILLING_FIXTURE: bogus" {
		t.Fatalf("unknown fixture %+v", s)
	}
}

func TestBillingViewMoneyParityWithPythonDecimal(t *testing.T) {
	for _, c := range [][2]string{{"1_000", "1000"}, {"_1", "1"}, {"1._5", "1.5"}, {"1.5_0", "1.50"}, {" 1 ", "1"},
		{"١٢", "12"}, {"1e+3", "1E+3"}, {".5", "0.5"}, {"5.", "5"}, {"-0", "-0"}, {"NaN0012", "NaN12"},
		{"-inf", "-Infinity"}, {"snan", "sNaN"}, {" 1 ", "1"}, {"1e_3", "1E+3"}, {"0.0000001", "1E-7"},
		{"0.000001", "0.000001"}, {"-0E-8", "-0E-8"}, {"123E-2", "1.23"}, {"1e999999999999999999", "1E+999999999999999999"}} {
		got := ParseMoney(c[0])
		if got == nil || got.String() != c[1] {
			t.Fatalf("parse %q = %v, want %s", c[0], got, c[1])
		}
	}
	for _, bad := range []any{"infinit", "1 2", "0x1", "", "1e1000000000000000000", true, map[string]any{}, nil} {
		if got := ParseMoney(bad); got != nil {
			t.Fatalf("parse %v = %s, want nil", bad, got)
		}
	}
	if got := ParseMoney(bvJSON(t, `{"v": 1e16}`)["v"]); got == nil || got.String() != "1E+16" {
		t.Fatalf("float money %v", got)
	}
	for _, c := range [][2]string{{"NaN", "$NaN"}, {"Infinity", "$Infinity"}, {"-Infinity", "$-Infinity"}, {"1E+3", "$1,000"},
		{"-5", "$-5"}, {"2.675", "$2.68"}, {"2.665", "$2.66"}, {"1e30", "$1,000,000,000,000,000,000,000,000,000,000"},
		{"-0.001", "$-0.00"}, {"1E-30", "$0.00"}, {"-1234567.5", "$-1,234,567.50"}, {"142.5", "$142.50"}} {
		got, err := FormatMoney(ParseMoney(c[0]), true)
		if err != nil || got != c[1] {
			t.Fatalf("format %s = %q %v, want %s", c[0], got, err, c[1])
		}
	}
	if got, _ := FormatMoney(ParseMoney("1000"), false); got != "$1000" {
		t.Fatalf("ungrouped %q", got)
	}
	if got, _ := FormatMoney(nil, false); got != "—" {
		t.Fatalf("none %q", got)
	}
	for _, bad := range []string{"sNaN", "1234567890123456789012345678.5"} {
		if _, err := FormatMoney(ParseMoney(bad), true); !errors.Is(err, ErrDecimalInvalidOperation) {
			t.Fatalf("format %s must raise, got %v", bad, err)
		}
	}
}

func TestBillingViewPaymentMethodAndAutoReloadCardKinds(t *testing.T) {
	p := bvJSON(t, `{"paymentMethod": {"kind": "card", "brand": "visa", "last4": "1", "wallet": "apple_pay", "resolvedVia": "subPin"},
	 "autoReload": {"enabled": 1, "card": {"kind": "distinct", "paymentMethodId": "pm_1", "brand": "amex", "last4": 9}}}`)
	s := bvMustState(t, p)
	if pm := s.PaymentMethod; pm.Kind != "card" || *pm.Wallet != "apple_pay" || *pm.ResolvedVia != "subPin" || pm.Email != nil {
		t.Fatalf("card method %+v", pm)
	}
	if c := s.AutoReload.Card; !s.AutoReload.Enabled || c.Kind != "distinct" || *c.PaymentMethodID != "pm_1" || c.Last4 != nil {
		t.Fatalf("auto reload card %+v", c)
	}
	p = bvJSON(t, `{"paymentMethod": {"kind": "card", "brand": "visa", "last4": ""}, "autoReload": {"card": {"kind": "other"}},
	 "card": {"brand": "Link", "last4": "", "resolvedVia": "nope"}, "chargePresets": "15"}`)
	s = bvMustState(t, p)
	if pm := s.PaymentMethod; pm.Kind != "unknown" || *pm.RawKind != "card" {
		t.Fatalf("incomplete card is unknown %+v", pm)
	}
	if s.AutoReload.Card != nil || s.Card.Display() != "Link" || len(s.ChargePresets) != 2 {
		t.Fatalf("card %q presets %v", s.Card.Display(), s.ChargePresets)
	}
	if _, err := BillingStateFromPayload(bvJSON(t, `{"chargePresets": 5}`), nil); err == nil || err.Error() != "'int' object is not iterable" {
		t.Fatalf("non iterable presets: %v", err)
	}
	s = bvMustState(t, bvJSON(t, `{"paymentMethod": {"kind": "link", "email": "a@b"}, "org": {"role": 5}}`))
	if s.PaymentMethod.Kind != "link" || *s.PaymentMethod.Email != "a@b" {
		t.Fatalf("link %+v", s.PaymentMethod)
	}
	if _, err := s.IsAdmin(); err == nil || err.Error() != "'int' object has no attribute 'upper'" {
		t.Fatalf("non string role: %v", err)
	}
}
