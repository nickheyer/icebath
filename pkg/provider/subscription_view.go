package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// DevSubscriptionFixtureEnv names the offline subscription screen fixture
const DevSubscriptionFixtureEnv = "ICEBATH_DEV_SUBSCRIPTION_FIXTURE"

const devSubscriptionFixturePortal = "https://portal.nousresearch.com/billing"

// CurrentSubscription is the active plan, raw server values kept as Python keeps them
type CurrentSubscription struct {
	TierID                   any
	TierName                 any
	MonthlyCredits           *PyDecimal
	CreditsRemaining         *PyDecimal
	CycleEndsAt              any
	PendingDowngradeTierName any
	PendingDowngradeAt       any
	CancelAtPeriodEnd        bool
	CancellationEffectiveAt  any
}

// SubscriptionTier is a plan picker row, the current plan shown but not selectable
type SubscriptionTier struct {
	TierID          any
	Name            any
	TierOrder       int64
	DollarsPerMonth *PyDecimal
	MonthlyCredits  *PyDecimal
	IsCurrent       bool
	IsEnabled       bool
}

// SubscriptionChangePreview is a parsed plan change quote with effect charge_now, scheduled, no_op or blocked
type SubscriptionChangePreview struct {
	Effect              string
	Reason              any
	CurrentTierID       any
	CurrentTierName     any
	TargetTierID        any
	TargetTierName      any
	MonthlyCreditsDelta *PyDecimal
	AmountDueNowCents   *int64
	EffectiveAt         any
}

// SubscriptionState is the parsed subscription overview, LoggedIn false with empty fields when unavailable
type SubscriptionState struct {
	OrgRoleCapability
	LoggedIn  bool
	OrgName   any
	OrgID     any
	Context   string
	Current   *CurrentSubscription
	Tiers     []SubscriptionTier
	PortalURL any
	Error     *string
}

// Python a or b, the first value when truthy and the second otherwise
func pyOrValue(a, b any) any {
	if auTruthy(a) {
		return a
	}
	return b
}

// Python str of a raw JSON value as an f-string or urlencode renders it
func viewPyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	}
	if s, ok := moneyText(v); ok {
		return s
	}
	return leftCorePyRepr(v)
}

// Python int of a value, the parse, overflow and type errors int raises returned as errors
func pyIntOf(v any) (int64, error) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, nil
		}
		return 0, nil
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			n, ok := new(big.Int).SetString(s, 10)
			if !ok || !n.IsInt64() {
				return 0, fmt.Errorf("Python int too large to convert to C long")
			}
			return n.Int64(), nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return 0, err
		}
		return pyIntOf(f)
	case float64:
		switch {
		case math.IsInf(t, 0):
			return 0, fmt.Errorf("cannot convert float infinity to integer")
		case math.IsNaN(t):
			return 0, fmt.Errorf("cannot convert float NaN to integer")
		case math.Trunc(t) >= 9223372036854775808.0 || math.Trunc(t) < -9223372036854775808.0:
			return 0, fmt.Errorf("Python int too large to convert to C long")
		}
		return int64(math.Trunc(t)), nil
	case string:
		if n, ok := pyIntText(t); ok {
			return n, nil
		}
		return 0, fmt.Errorf("invalid literal for int() with base 10: %s", pyStringRepr(t))
	}
	return 0, fmt.Errorf("int() argument must be a string, a bytes-like object or a real number, not '%s'", auTypeName(v))
}

// Python dict key identity of a value, numbers equal across int float and bool, containers unhashable
func pyHashKey(v any) (string, error) {
	switch t := v.(type) {
	case nil:
		return "N", nil
	case string:
		return "s:" + t, nil
	case bool:
		if t {
			return "n:1", nil
		}
		return "n:0", nil
	case int:
		return "n:" + strconv.Itoa(t), nil
	case int64:
		return "n:" + strconv.FormatInt(t, 10), nil
	case json.Number:
		s := string(t)
		if !strings.ContainsAny(s, ".eE") {
			n, _ := new(big.Int).SetString(s, 10)
			return "n:" + n.String(), nil
		}
		f, _ := strconv.ParseFloat(s, 64)
		return pyHashKey(f)
	case float64:
		if !math.IsInf(t, 0) && !math.IsNaN(t) && t == math.Trunc(t) {
			n, _ := new(big.Float).SetFloat64(t).Int(nil)
			return "n:" + n.String(), nil
		}
		return "f:" + pyFloatRepr(t), nil
	}
	return "", fmt.Errorf("unhashable type: '%s'", auTypeName(v))
}

// Real tier id of a server object, nil for a missing id or a non object
func subscriptionTierID(raw any) any {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	id := pyOrValue(m["tierId"], m["id"])
	if !auTruthy(id) {
		return nil
	}
	return id
}

func parseCurrentSubscription(raw any) *CurrentSubscription {
	tierID := subscriptionTierID(raw)
	if tierID == nil {
		return nil
	}
	m := raw.(map[string]any)
	cur := &CurrentSubscription{
		TierID:                   tierID,
		TierName:                 pyOrValue(m["tierName"], m["name"]),
		MonthlyCredits:           ParseMoney(m["monthlyCredits"]),
		CreditsRemaining:         ParseMoney(m["creditsRemaining"]),
		CycleEndsAt:              m["cycleEndsAt"],
		PendingDowngradeTierName: m["pendingDowngradeTierName"],
		PendingDowngradeAt:       m["pendingDowngradeAt"],
		CancelAtPeriodEnd:        auTruthy(m["cancelAtPeriodEnd"]),
	}
	if auTruthy(m["cancellationEffectiveAt"]) {
		cur.CancellationEffectiveAt = m["cancellationEffectiveAt"]
	}
	return cur
}

// Tier picker row of a server object, nil without a tier id, an error where Python int raises
func parseSubscriptionTier(raw any) (*SubscriptionTier, error) {
	tierID := subscriptionTierID(raw)
	if tierID == nil {
		return nil, nil
	}
	m := raw.(map[string]any)
	name := pyOrValue(m["name"], "")
	order := m["tierOrder"]
	if order == nil {
		order = int64(0)
	}
	tierOrder, err := pyIntOf(order)
	if err != nil {
		return nil, err
	}
	enabled := true
	if m["isEnabled"] != nil {
		enabled = auTruthy(m["isEnabled"])
	}
	return &SubscriptionTier{TierID: tierID, Name: name, TierOrder: tierOrder, DollarsPerMonth: ParseMoney(m["dollarsPerMonthDisplay"]),
		MonthlyCredits: ParseMoney(m["monthlyCredits"]), IsCurrent: auTruthy(m["isCurrent"]), IsEnabled: enabled}, nil
}

// SubscriptionChangePreviewFromPayload maps a plan change quote, a missing or unknown effect becoming blocked
func SubscriptionChangePreviewFromPayload(payload map[string]any) (*SubscriptionChangePreview, error) {
	effect, ok := payload["effect"].(string)
	if !ok {
		effect = "blocked"
	}
	p := &SubscriptionChangePreview{
		Effect:              effect,
		CurrentTierID:       payload["currentTierId"],
		CurrentTierName:     payload["currentTierName"],
		TargetTierID:        payload["targetTierId"],
		TargetTierName:      payload["targetTierName"],
		MonthlyCreditsDelta: ParseMoney(payload["monthlyCreditsDelta"]),
	}
	if auTruthy(payload["reason"]) {
		p.Reason = payload["reason"]
	}
	switch cents := payload["amountDueNowCents"].(type) {
	case bool, json.Number, float64, int, int64:
		n, err := pyIntOf(cents)
		if err != nil {
			return nil, err
		}
		p.AmountDueNowCents = &n
	}
	if auTruthy(payload["effectiveAt"]) {
		p.EffectiveAt = payload["effectiveAt"]
	}
	return p, nil
}

// SubscriptionStateFromPayload maps the subscription overview JSON object into a SubscriptionState
func SubscriptionStateFromPayload(payload map[string]any, portalURL any) (*SubscriptionState, error) {
	org, canChangePlanRaw := ParseOrgFields(payload)
	s := &SubscriptionState{
		OrgRoleCapability: OrgRoleCapability{Role: org["role"], CanChangePlanRaw: canChangePlanRaw},
		LoggedIn:          true,
		OrgName:           org["name"],
		Context:           "personal",
		Current:           parseCurrentSubscription(payload["current"]),
		PortalURL:         portalURL,
	}
	if auTruthy(org["id"]) {
		s.OrgID = org["id"]
	}
	if c, ok := payload["context"].(string); ok && (c == "personal" || c == "team") {
		s.Context = c
	}
	if raw, ok := payload["tiers"].([]any); ok {
		for _, item := range raw {
			tier, err := parseSubscriptionTier(item)
			if err != nil {
				return nil, err
			}
			if tier != nil {
				s.Tiers = append(s.Tiers, *tier)
			}
		}
	}
	return s, nil
}

// BuildSubscriptionState fetches and parses the subscription overview fail open, the dev fixture short circuiting
func BuildSubscriptionState(ctx context.Context, timeout time.Duration) (*SubscriptionState, error) {
	if fixture := DevFixtureSubscriptionState(); fixture != nil {
		return fixture, nil
	}
	return fetchPortalState(ctx, nousSubscriptionStateFetch, "subscription",
		func(errText *string) *SubscriptionState {
			return &SubscriptionState{Context: "personal", Error: errText}
		},
		SubscriptionStateFromPayload, func(base string) string { return base }, timeout)
}

// Python unquote with UTF-8 errors replaced, each maximal invalid subpart one replacement character
func pyUnquoteReplace(s string) string {
	var out, pending strings.Builder
	flush := func() {
		p := pending.String()
		for i := 0; i < len(p); {
			r, size := utf8.DecodeRuneInString(p[i:])
			if r == utf8.RuneError && size <= 1 {
				out.WriteString("�")
				i += pystr.UTF8MaximalSubpart([]byte(p[i:]))
				continue
			}
			out.WriteString(p[i : i+size])
			i += size
		}
		pending.Reset()
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHexByte(s[i+1]) && isHexByte(s[i+2]) {
			v, _ := strconv.ParseUint(s[i+1:i+3], 16, 8)
			pending.WriteByte(byte(v))
			i += 2
			continue
		}
		flush()
		out.WriteByte(s[i])
	}
	flush()
	return out.String()
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// Python urllib quote_plus with no extra safe characters
func pyQuotePlusAll(s string) string {
	const safe = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_.-~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case strings.IndexByte(safe, c) >= 0:
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Ordered query pairs like dict of parse_qsl with blank values kept, a repeated key keeping its first slot
type manageQuery struct {
	keys   []string
	values map[string]string
}

func (q *manageQuery) set(k, v string) {
	if _, ok := q.values[k]; !ok {
		q.keys = append(q.keys, k)
	}
	q.values[k] = v
}

func (q *manageQuery) pop(k string) {
	if _, ok := q.values[k]; !ok {
		return
	}
	delete(q.values, k)
	for i, key := range q.keys {
		if key == k {
			q.keys = append(q.keys[:i], q.keys[i+1:]...)
			return
		}
	}
}

func parseQueryKeepBlank(query string) *manageQuery {
	q := &manageQuery{values: map[string]string{}}
	if query == "" {
		return q
	}
	for _, part := range strings.Split(query, "&") {
		if part == "" {
			continue
		}
		name, value, _ := strings.Cut(part, "=")
		q.set(pyUnquoteReplace(strings.ReplaceAll(name, "+", " ")), pyUnquoteReplace(strings.ReplaceAll(value, "+", " ")))
	}
	return q
}

// SubscriptionManageURL is the portal manage page pinned to the org and optionally the plan, false when unresolvable
func SubscriptionManageURL(state *SubscriptionState, tierID string) (string, bool) {
	raw := state.PortalURL
	if !auTruthy(raw) {
		raw = ""
	}
	portal, ok := raw.(string)
	if !ok {
		return "", false
	}
	parts, err := pyURLParse(portal, "")
	if err != nil || (parts.scheme != "http" && parts.scheme != "https") || parts.netloc == "" {
		return "", false
	}
	q := parseQueryKeepBlank(parts.query)
	q.pop("org_id")
	q.pop("plan")
	if auTruthy(state.OrgID) {
		q.set("org_id", viewPyStr(state.OrgID))
	}
	if tierID != "" {
		q.set("plan", tierID)
	}
	pairs := make([]string, 0, len(q.keys))
	for _, k := range q.keys {
		pairs = append(pairs, pyQuotePlusAll(k)+"="+pyQuotePlusAll(q.values[k]))
	}
	return pyURLUnparse(pyURLParts{scheme: parts.scheme, netloc: parts.netloc, path: "/manage-subscription", query: strings.Join(pairs, "&")}), true
}

// SelectableTiers lists enabled paid tiers other than the current plan, cheapest first
func SelectableTiers(state *SubscriptionState) []SubscriptionTier {
	var out []SubscriptionTier
	for _, t := range state.Tiers {
		if t.IsEnabled && !t.IsCurrent && t.TierOrder > 0 {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].TierOrder < out[j].TierOrder })
	return out
}

// FormatTierRow renders name, grouped monthly price and a credits suffix only when credits exceed zero
func FormatTierRow(tier SubscriptionTier) (string, error) {
	price, err := FormatMoney(tier.DollarsPerMonth, true)
	if err != nil {
		return "", err
	}
	row := viewPyStr(tier.Name) + " · " + price + "/mo"
	if mc := tier.MonthlyCredits; mc != nil {
		c, err := mc.Cmp(DecimalFromInt(0))
		if err != nil {
			return "", err
		}
		if c > 0 {
			credits, err := FormatMoney(mc, true)
			if err != nil {
				return "", err
			}
			row += " · " + credits + " credits/mo"
		}
	}
	return row, nil
}

// IsUpgrade reports the tier ranking above the current plan, by active subscription then the current marker
func IsUpgrade(state *SubscriptionState, tierID string) (bool, error) {
	orders := map[string]int64{}
	for _, t := range state.Tiers {
		key, err := pyHashKey(t.TierID)
		if err != nil {
			return false, err
		}
		orders[key] = t.TierOrder
	}
	var curID any
	if state.Current != nil {
		curID = state.Current.TierID
	}
	curKey, err := pyHashKey(curID)
	if err != nil {
		return false, err
	}
	curOrder, found := orders[curKey]
	if !found {
		for _, t := range state.Tiers {
			if t.IsCurrent {
				curOrder = t.TierOrder
				break
			}
		}
	}
	return orders["s:"+tierID] > curOrder, nil
}

var devTierSpecs = []struct {
	id, name string
	order    int64
	dpm, mc  string
}{{"free", "Free", 0, "0", "0"}, {"plus", "Plus", 1, "20", "1000"}, {"super", "Super", 2, "40", "3000"}, {"ultra", "Ultra", 3, "80", "7000"}}

var devSubscriptionFixtureAliases = map[string]string{"logged_out": "logged-out", "loggedout": "logged-out", "mid-tier": "mid",
	"top-tier": "top", "member": "not-admin"}

// Sample plan catalog with the given tier marked current
func devTiers(currentID string) []SubscriptionTier {
	out := make([]SubscriptionTier, 0, len(devTierSpecs))
	for _, spec := range devTierSpecs {
		out = append(out, SubscriptionTier{TierID: spec.id, Name: spec.name, TierOrder: spec.order, DollarsPerMonth: ParseMoney(spec.dpm),
			MonthlyCredits: ParseMoney(spec.mc), IsCurrent: spec.id == currentID, IsEnabled: true})
	}
	return out
}

// Fixture state on the tier with the remaining credits, the catalog marking it current
func devPlan(s *SubscriptionState, tierID, remaining string) *CurrentSubscription {
	for _, spec := range devTierSpecs {
		if spec.id == tierID {
			s.Current = &CurrentSubscription{TierID: spec.id, TierName: spec.name, MonthlyCredits: decPtr(MustDecimal(spec.mc)),
				CreditsRemaining: decPtr(MustDecimal(remaining)), CycleEndsAt: "2026-07-01"}
		}
	}
	s.Tiers = devTiers(tierID)
	return s.Current
}

// DevFixtureSubscriptionState is the named offline fixture, nil when unset and logged out with an error when unknown
func DevFixtureSubscriptionState() *SubscriptionState {
	name := pyLower(pyStripSpace(os.Getenv(DevSubscriptionFixtureEnv)))
	if name == "" {
		return nil
	}
	if alias, ok := devSubscriptionFixtureAliases[name]; ok {
		name = alias
	}
	if name == "logged-out" {
		return &SubscriptionState{Context: "personal"}
	}
	s := &SubscriptionState{OrgRoleCapability: OrgRoleCapability{Role: "OWNER"}, LoggedIn: true, OrgName: "Acme Inc",
		OrgID: "org_acme", Context: "personal", PortalURL: devSubscriptionFixturePortal}
	switch name {
	case "free":
		s.Tiers = devTiers("")
	case "mid":
		devPlan(s, "plus", "420")
	case "top":
		devPlan(s, "ultra", "5000")
	case "not-admin":
		devPlan(s, "plus", "420")
		s.Role = "MEMBER"
	case "downgrade":
		cur := devPlan(s, "super", "1500")
		cur.PendingDowngradeTierName, cur.PendingDowngradeAt = "Plus", "2026-07-15"
	case "cancel":
		cur := devPlan(s, "plus", "420")
		cur.CancelAtPeriodEnd, cur.CancellationEffectiveAt = true, "2026-07-01"
	case "team":
		s.Context, s.OrgName, s.OrgID = "team", "Acme Engineering", "org_eng"
	default:
		text := "unknown " + DevSubscriptionFixtureEnv + ": " + name
		return &SubscriptionState{Context: "personal", Error: &text}
	}
	return s
}
