package langfuse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Langfuse usage key, canonical usage attribute and the rate card column billing it
type usageField struct {
	key  string
	attr string
	rate string
}

var usageFields = []usageField{
	{"input", "input_tokens", "input"},
	{"output", "output_tokens", "output"},
	{"cache_read_input_tokens", "cache_read_tokens", "cache_read"},
	{"cache_creation_input_tokens", "cache_write_tokens", "cache_write"},
	{"output_reasoning_tokens", "reasoning_tokens", "output"},
}

// Whole number of a token count that may be absent, a float, numeric text or a bool, counts written as integers held within the int32 range
func pyIntOf(v any) int {
	if !anyutil.IsTrue(v) {
		return 0
	}
	switch t := v.(type) {
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			panic(&errorutil.Error{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + strconv.Quote(t)})
		}
		return clampInt(n)
	case float64:
		return floatToInt(t)
	case float32:
		return floatToInt(float64(t))
	case json.Number:
		if n, err := strconv.ParseInt(t.String(), 10, 64); err == nil || errors.Is(err, strconv.ErrRange) {
			return clampInt(n)
		}
		f, err := t.Float64()
		if err != nil {
			panic(&errorutil.Error{Kind: "ValueError", Msg: "invalid literal for int() with base 10: " + strconv.Quote(t.String())})
		}
		return floatToInt(f)
	case *big.Int:
		if !t.IsInt64() {
			return clampInt(int64(t.Sign()) * math.MaxInt64)
		}
		return clampInt(t.Int64())
	}
	if n, ok := pyIntValue(v); ok {
		return toInt(n)
	}
	panic(fmt.Sprintf("a usage count is %s, expected a whole number", errchain.Kind(v)))
}

// Python int() of a float, raising on NaN and infinity
func floatToInt(f float64) int {
	if math.IsNaN(f) {
		panic(&errorutil.Error{Kind: "ValueError", Msg: "cannot convert float NaN to integer"})
	}
	if math.IsInf(f, 0) {
		panic(&errorutil.Error{Kind: "OverflowError", Msg: "cannot convert float infinity to integer"})
	}
	return int(math.Trunc(f))
}

// Langfuse token buckets from canonical counts, reasoning split out of output so the buckets sum to the total
func usageBuckets(read func(attr string) any) *jsonutil.Object {
	tokens := map[string]int{}
	for _, f := range usageFields {
		tokens[f.key] = max(pyIntOf(read(f.attr)), 0)
	}
	reasoning := min(tokens["output_reasoning_tokens"], tokens["output"])
	tokens["output_reasoning_tokens"], tokens["output"] = reasoning, tokens["output"]-reasoning
	out := jsonutil.NewObject()
	for _, f := range usageFields {
		out.Set(f.key, tokens[f.key])
	}
	return out
}

// Reader of the canonical usage attributes
func canonicalReader(u provider.CanonicalUsage) func(string) any {
	return func(attr string) any {
		switch attr {
		case "input_tokens":
			return u.InputTokens
		case "output_tokens":
			return u.OutputTokens
		case "cache_read_tokens":
			return u.CacheReadTokens
		case "cache_write_tokens":
			return u.CacheWriteTokens
		case "reasoning_tokens":
			return u.ReasoningTokens
		}
		return nil
	}
}

// Rate of one rate card column, the above tier rate when the request crossed the tier and has one
func entryRate(e provider.PricingEntry, column string, above bool) *big.Rat {
	var base, tier *big.Rat
	switch column {
	case "input":
		base, tier = e.InputCostPerMillion, e.InputCostPerMillionAbove
	case "output":
		base, tier = e.OutputCostPerMillion, e.OutputCostPerMillionAbove
	case "cache_read":
		base, tier = e.CacheReadCostPerMillion, e.CacheReadCostPerMillionAbove
	case "cache_write":
		base, tier = e.CacheWriteCostPerMillion, e.CacheWriteCostPerMillionAbove
	}
	if above && tier != nil {
		return tier
	}
	return base
}

// Langfuse usage and cost maps of canonical usage, costs left out where Langfuse should estimate or the total is unknown
func canonicalUsageAndCost(ctx context.Context, canonical provider.CanonicalUsage, providerName, model, baseURL string) (*jsonutil.Object, *jsonutil.Object) {
	buckets := usageBuckets(canonicalReader(canonical))
	usageDetails := jsonutil.NewObject()
	for _, k := range buckets.Keys() {
		if n := buckets.Value(k).(int); n != 0 || k == "input" || k == "output" {
			usageDetails.Set(k, n)
		}
	}
	costDetails := jsonutil.NewObject()
	var cost provider.CostResult
	priced := func() (ok bool) {
		defer func() {
			if r := recover(); r != nil {
				debugLog(fmt.Sprintf("usage pricing failed: %v", r))
				ok = false
			}
		}()
		if provider.ResolveBillingRoute(model, providerName, baseURL).BillingMode == "subscription_included" {
			return false
		}
		cost = provider.EstimateUsageCost(ctx, model, canonical, providerName, baseURL, "")
		return true
	}()
	if !priced || cost.AmountUSD == nil {
		return usageDetails, costDetails
	}
	if total, _ := cost.AmountUSD.Float64(); cost.Status != "included" && total > 0 {
		costDetails.Set("total", total)
	}
	func() {
		defer func() { _ = recover() }()
		entry, ok := provider.GetPricingEntry(ctx, model, providerName, baseURL, "")
		if !ok {
			return
		}
		above := entry.HasTier && canonical.PromptTokens() > entry.TierThresholdTokens
		for _, f := range usageFields {
			rate := entryRate(entry, f.rate, above)
			tokens := buckets.Value(f.key).(int)
			if rate != nil && tokens != 0 {
				part := new(big.Rat).Mul(big.NewRat(int64(tokens), 1), rate)
				part.Quo(part, big.NewRat(1000000, 1))
				value, _ := part.Float64()
				costDetails.Set(f.key, value)
			}
		}
	}()
	return usageDetails, costDetails
}

// Usage and cost maps from a response object's usage, or from the post_api_request usage summary when one is given
func usageAndCost(ctx context.Context, response any, providerName, model, baseURL, apiMode string, usage any) (usageDetails, costDetails *jsonutil.Object) {
	rawUsage := objectAttr(response, "usage")
	if usage == nil && !anyutil.IsTrue(rawUsage) {
		return jsonutil.NewObject(), jsonutil.NewObject()
	}
	defer func() {
		if r := recover(); r != nil {
			if usage == nil {
				debugLog(fmt.Sprintf("usage normalization failed: %v", r))
			}
			usageDetails, costDetails = jsonutil.NewObject(), jsonutil.NewObject()
		}
	}()
	var canonical provider.CanonicalUsage
	if usage == nil {
		canonical = provider.NormalizeUsage(rawUsage, providerName, apiMode)
	} else {
		output := dget(usage, "output_tokens")
		if !anyutil.IsTrue(output) {
			output = dget(usage, "completion_tokens")
		}
		requestCount := 1
		if v, ok := dictGet(usage, "request_count"); ok {
			requestCount = pyIntOf(v)
		}
		canonical = provider.CanonicalUsage{
			InputTokens: pyIntOf(dget(usage, "input_tokens")), OutputTokens: pyIntOf(output),
			CacheReadTokens: pyIntOf(dget(usage, "cache_read_tokens")), CacheWriteTokens: pyIntOf(dget(usage, "cache_write_tokens")),
			ReasoningTokens: pyIntOf(dget(usage, "reasoning_tokens")), RequestCount: requestCount,
		}
	}
	return canonicalUsageAndCost(ctx, canonical, providerName, model, baseURL)
}
