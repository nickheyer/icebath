package models

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
)

// Note of a cost a subscription covers
const IncludedNote = "subscription-included; no provider invoice for usage"

// Where a price came from
const (
	FromPriceList = "official_docs_snapshot"
	FromProvider  = "provider_models_api"
	FromNowhere   = "none"
)

// What a request cost, or why that is not known
type Cost struct {
	// USD, nil when not known
	USD *big.Rat
	// estimated, included or unknown
	Status  string
	Source  string
	Label   string
	Version string
	Fetched time.Time
	Notes   []string
}

// USD of the cost, 0 when not known
func (c Cost) Amount() float64 {
	if c.USD == nil {
		return 0
	}
	f, _ := c.USD.Float64()
	return f
}

// Price of a route's model with where it came from
type pricing struct {
	price   provider.Price
	source  string
	version string
	fetched time.Time
	notes   []string
}

// Kind whose price list a route bills by: its own when it publishes one, else the one claiming its address
func (m *Models) priceSpec(route provider.Route) provider.Spec {
	spec := m.Spec(route)
	if len(spec.Prices) > 0 || spec.ListPrices != "" {
		return spec
	}
	if claimed, ok := m.providers.Spec(m.Kind(route.BaseURL)); ok && route.BaseURL != "" {
		return claimed
	}
	return spec
}

// Spellings of a model a price list may use: as it is, and without a vendor or region before it, with dashed versions and without a
// revision or date after it
func priceKeys(model string) []string {
	lower := strings.ToLower(strings.TrimSpace(model))
	keys := []string{lower}
	bare := lower
	for _, prefix := range []string{"anthropic/", "global.", "us.", "eu.", "apac.", "ap.", "au.", "jp.", "ca.", "sa.", "me.", "af."} {
		if strings.HasPrefix(bare, prefix) {
			bare = bare[len(prefix):]
			break
		}
	}
	bare = dotted.ReplaceAllString(bare, "$1-$2")
	for _, trailer := range revisions {
		bare = trailer.ReplaceAllString(bare, "")
	}
	if bare != lower {
		keys = append(keys, bare)
	}
	return keys
}

var (
	dotted    = regexp.MustCompile(`(\d+)\.(\d+)`)
	revisions = []*regexp.Regexp{regexp.MustCompile(`:\d+$`), regexp.MustCompile(`-v\d+$`), regexp.MustCompile(`-\d{8}$`)}
)

func lookupPrice(prices map[string]provider.Price, model string) (provider.Price, bool) {
	for _, key := range priceKeys(model) {
		if price, ok := prices[key]; ok {
			return price, true
		}
	}
	return provider.Price{}, false
}

// Name of the model after its vendor's
func bare(model string) string {
	return model[strings.LastIndex(model, "/")+1:]
}

// Price of the route's model: none for a subscription, an aggregator's own catalog, the published price list of the kind billing it, the
// prices its endpoint lists, else the list prices models.dev keeps for a first party API
func (m *Models) pricing(ctx context.Context, cfg *config.Config, route provider.Route) (pricing, bool) {
	spec := m.Spec(route)
	if spec.Included {
		return pricing{source: FromNowhere, version: "included-route"}, true
	}
	if spec.Aggregator {
		if prices, ok := m.Registry.prices(ctx, m.env(cfg, route, false)); ok {
			if price, ok := prices[route.Model]; ok {
				return pricing{price: price, source: FromProvider, version: route.Spec + "-models-api", fetched: time.Now(),
					notes: []string{spec.Label() + " cost is estimated from the models API until reconciled."}}, true
			}
		}
	}
	billing := m.priceSpec(route)
	if price, ok := lookupPrice(billing.Prices, bare(route.Model)); ok {
		return pricing{price: price, source: FromPriceList, version: price.Version}, true
	}
	if route.BaseURL != "" {
		if r, ok := m.Endpoint(ctx, cfg, route.BaseURL, route.APIKey)[route.Model]; ok {
			if price, ok := r.Price(); ok {
				return pricing{price: price, source: FromProvider, version: "openai-compatible-models-api", fetched: time.Now()}, true
			}
		}
	}
	if domain := billing.ListPrices; domain != "" && route.Model != "" {
		if route.BaseURL != "" {
			parsed, err := url.Parse(route.BaseURL)
			host := ""
			if err == nil {
				host = strings.ToLower(parsed.Hostname())
			}
			if err != nil || parsed.Scheme != "https" || parsed.Port() != "" && parsed.Port() != "443" || host != domain && !strings.HasSuffix(host, "."+domain) {
				return pricing{}, false
			}
		}
		lookup := route
		lookup.Spec, lookup.Model = billing.Name, bare(route.Model)
		if info, ok := m.Info(cfg, lookup); ok && info.Priced() {
			return pricing{price: devPrice(info), source: FromProvider, version: "models.dev", fetched: time.Now()}, true
		}
	}
	return pricing{}, false
}

// Price of a model as models.dev lists it
func devPrice(info Info) provider.Price {
	text := func(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
	price := provider.Price{Input: text(info.CostInput), Output: text(info.CostOutput), URL: "https://models.dev", Version: "models.dev"}
	if info.CostCacheRead != nil {
		price.CacheRead = text(*info.CostCacheRead)
	}
	if info.CostCacheWrite != nil {
		price.CacheWrite = text(*info.CostCacheWrite)
	}
	return price
}

// Prices of the models of the route's provider in the models.dev registry as held, by id
func (m *Models) DevPrices(cfg *config.Config, route provider.Route) map[string]provider.Price {
	out := map[string]provider.Price{}
	for id, raw := range m.devModels(cfg, route, false) {
		entry, _ := raw.(map[string]any)
		cost, ok := entry["cost"].(map[string]any)
		if !ok || cost["input"] == nil && cost["output"] == nil {
			continue
		}
		info := Info{CostInput: floatOf(cost["input"]), CostOutput: floatOf(cost["output"])}
		if config.Truthy(cost["cache_read"]) {
			info.CostCacheRead = new(floatOf(cost["cache_read"]))
		}
		out[id] = devPrice(info)
	}
	return out
}

func floatOf(v any) float64 {
	f, _ := v.(float64)
	return f
}

// Prices of every model of the route's provider for showing beside them, as its own catalog publishes them; empty when it publishes none
func (m *Models) Prices(ctx context.Context, cfg *config.Config, route provider.Route, refresh bool) map[string]provider.Price {
	prices, _ := m.Registry.prices(ctx, m.env(cfg, route, refresh))
	return prices
}

// Price of one model for showing beside it: the provider's own catalog, else its kind's price list or the list prices models.dev keeps;
// nothing is fetched
func (m *Models) ShownPrice(cfg *config.Config, route provider.Route, catalog map[string]provider.Price) (provider.Price, bool) {
	if price, ok := catalog[route.Model]; ok {
		return price, true
	}
	billing := m.priceSpec(route)
	if price, ok := lookupPrice(billing.Prices, bare(route.Model)); ok {
		return price, true
	}
	if billing.ListPrices != "" {
		lookup := route
		lookup.Spec, lookup.Model = billing.Name, bare(route.Model)
		if info, ok := m.Info(cfg, lookup); ok && info.Priced() {
			return devPrice(info), true
		}
	}
	return provider.Price{}, false
}

func unknownCost(source string, notes ...string) Cost {
	return Cost{Status: "unknown", Source: source, Label: "n/a", Notes: notes}
}

// What one request on the route cost: its tokens at the model's prices, those of the tier that served it when it has its own, those past
// the price's threshold at the higher ones
func (m *Models) Cost(ctx context.Context, cfg *config.Config, route provider.Route, usage provider.Usage) Cost {
	spec := m.Spec(route)
	if spec.Included {
		return Cost{USD: new(big.Rat), Status: "included", Source: FromNowhere, Label: "included", Version: "included-route", Notes: []string{IncludedNote}}
	}
	found, ok := m.pricing(ctx, cfg, route)
	if tiers, has := m.priceSpec(route).TierPrices[usage.Tier]; usage.Tier != "" && has {
		price, known := lookupPrice(tiers, bare(route.Model))
		if !known {
			tier := usage.Tier
			if tier == "fast" {
				tier = "fast-mode"
			}
			return unknownCost(FromPriceList, tier+" pricing unavailable for model")
		}
		found, ok = pricing{price: price, source: FromPriceList, version: price.Version}, true
	}
	if !ok {
		return unknownCost(FromNowhere)
	}
	price := found.price
	above := price.Above != nil && price.Threshold > 0 && usage.Prompt() > int64(price.Threshold)
	amount := new(big.Rat)
	for _, bucket := range []struct {
		tokens      int64
		rate, upper string
		note        string
	}{
		{usage.Input, price.Input, aboveOf(price, func(p *provider.Price) string { return p.Input }), ""},
		{usage.Output, price.Output, aboveOf(price, func(p *provider.Price) string { return p.Output }), ""},
		{usage.CacheRead, price.CacheRead, aboveOf(price, func(p *provider.Price) string { return p.CacheRead }), "cache-read pricing unavailable for route"},
		{usage.CacheWrite, price.CacheWrite, aboveOf(price, func(p *provider.Price) string { return p.CacheWrite }), "cache-write pricing unavailable for route"},
	} {
		rate := bucket.rate
		if above && bucket.upper != "" {
			rate = bucket.upper
		}
		value, known := new(big.Rat).SetString(strings.TrimSpace(rate))
		if rate == "" || !known {
			if bucket.tokens != 0 {
				if bucket.note == "" {
					return unknownCost(found.source)
				}
				return unknownCost(found.source, bucket.note)
			}
			continue
		}
		part := new(big.Rat).Mul(big.NewRat(bucket.tokens, 1), value)
		amount.Add(amount, part.Quo(part, big.NewRat(1_000_000, 1)))
	}
	if request, known := new(big.Rat).SetString(strings.TrimSpace(price.Request)); price.Request != "" && known {
		amount.Add(amount, request)
	}
	cost := Cost{USD: amount, Status: "estimated", Source: found.source, Label: CostLabel(amount), Version: cmp.Or(found.version, price.Version),
		Fetched: found.fetched, Notes: found.notes}
	if found.source == FromNowhere && amount.Sign() == 0 {
		cost.Status, cost.Label = "included", "included"
		cost.Notes = append(cost.Notes, IncludedNote)
	}
	return cost
}

func aboveOf(price provider.Price, field func(*provider.Price) string) string {
	if price.Above == nil {
		return ""
	}
	return field(price.Above)
}

// Amount as $0.00 for nothing, at four places below a cent and two above
func CostLabel(amount *big.Rat) string {
	if amount == nil || amount.Sign() == 0 {
		return "$0.00"
	}
	if amount.Cmp(big.NewRat(1, 100)) < 0 {
		if label := "~$" + fixed(amount, 4); label != "~$0.0000" {
			return label
		}
		return "~$<0.0001"
	}
	return "~$" + fixed(amount, 2)
}

// Decimal of an amount at the places, halves rounded to even
func fixed(r *big.Rat, places int) string {
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil)
	scaled := new(big.Rat).Mul(r, new(big.Rat).SetInt(scale))
	num := new(big.Int).Abs(scaled.Num())
	quo, rem := new(big.Int).QuoRem(num, scaled.Denom(), new(big.Int))
	switch new(big.Int).Mul(rem, big.NewInt(2)).Cmp(scaled.Denom()) {
	case 1:
		quo.Add(quo, big.NewInt(1))
	case 0:
		if quo.Bit(0) == 1 {
			quo.Add(quo, big.NewInt(1))
		}
	}
	digits := quo.String()
	for len(digits) <= places {
		digits = "0" + digits
	}
	digits = digits[:len(digits)-places] + "." + digits[len(digits)-places:]
	if scaled.Sign() < 0 && quo.Sign() != 0 {
		digits = "-" + digits
	}
	return digits
}

// Tokens shortened as 200K or 1.05M, empty when not known
func Tokens(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n >= 1_000_000 && n%1_000_000 == 0:
		return strconv.FormatInt(n/1_000_000, 10) + "M"
	case n >= 1_000_000:
		return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(float64(n)/1_000_000, 'f', 2, 64), "0"), ".") + "M"
	case n >= 1_000:
		return strconv.FormatInt(n/1_000, 10) + "K"
	}
	return strconv.FormatInt(n, 10)
}

// Price for a million tokens as dollars, free for nothing and enough places to show a tiny one; ? when it is not a number
func PerMillion(price string) string {
	value, err := strconv.ParseFloat(strings.TrimSpace(price), 64)
	if err != nil {
		return "?"
	}
	if value == 0 {
		return "free"
	}
	text := fmt.Sprintf("%.2f", value)
	if value < 0.01 {
		places := 3
		for places < 12 && math.RoundToEven(value*math.Pow(10, float64(places))) == 0 {
			places++
		}
		text = strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.*f", min(places+1, 12), value), "0"), ".")
	}
	return "$" + text
}
