package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	defaultEmptyRetryBudget  = 3
	reducedEmptyRetryBudget  = 1
	defaultEmptyGuardEnabled = true
)

// One observed empty completion within the current streak
type emptyAttempt struct {
	model, provider, finishReason string
	usagePresent, zeroOutput      bool
	observedGeneration            bool
}

func (e emptyAttempt) signature() [3]string { return [3]string{e.model, e.provider, e.finishReason} }

// Decimal value the guard settings read, finite as a rational or a signed infinity or NaN
type guardDecimal struct {
	amount *big.Rat
	inf    int
	nan    bool
}

var (
	defaultEmptyGuardCostThresholdUSD = guardDecimal{amount: big.NewRat(1, 4)}
	pyDecimalLiteral                  = regexp.MustCompile(`^([+-]?)(?:(\d(?:_?\d)*)(?:\.(\d(?:_?\d)*)?)?|\.(\d(?:_?\d)*))(?:[eE]([+-]?\d(?:_?\d)*))?$`)
	pyDecimalSpecial                  = regexp.MustCompile(`(?i)^([+-]?)(inf|infinity|nan|snan)(\d*)$`)
	errDecimalNaNCompare              = errors.New("[<class 'decimal.InvalidOperation'>]")
	estimateEmptyAttemptCostFn        = estimateEmptyAttemptCost
)

func emptyGuardLog() *slog.Logger { return slog.Default().With("logger", "agent.empty_response_guard") }

// Decimal of a str() form with the Python grammar, an error where Decimal raises InvalidOperation
func parseGuardDecimal(s string) (guardDecimal, error) {
	s = strings.TrimSpace(s)
	if m := pyDecimalSpecial.FindStringSubmatch(s); m != nil {
		word := strings.ToLower(m[2])
		if strings.HasSuffix(word, "nan") {
			return guardDecimal{nan: true}, nil
		}
		if m[3] != "" {
			return guardDecimal{}, errDecimalNaNCompare
		}
		if m[1] == "-" {
			return guardDecimal{inf: -1}, nil
		}
		return guardDecimal{inf: 1}, nil
	}
	if !pyDecimalLiteral.MatchString(s) {
		return guardDecimal{}, errDecimalNaNCompare
	}
	r, ok := new(big.Rat).SetString(strings.ReplaceAll(s, "_", ""))
	if !ok {
		return guardDecimal{}, errDecimalNaNCompare
	}
	return guardDecimal{amount: r}, nil
}

// Sign of the value against zero, the InvalidOperation of a NaN comparison as an error
func (d guardDecimal) sign() (int, error) {
	if d.nan {
		return 0, errDecimalNaNCompare
	}
	if d.inf != 0 {
		return d.inf, nil
	}
	return d.amount.Sign(), nil
}

// True when a finite attempt cost reaches this threshold
func (d guardDecimal) reachedBy(cost *big.Rat) bool {
	switch {
	case d.nan:
		return false
	case d.inf > 0:
		return false
	case d.inf < 0:
		return true
	}
	return cost.Cmp(d.amount) >= 0
}

// Enabled flag and cost threshold of agent.empty_response_guard, malformed input giving the schema defaults
func resolveGuardSettings(raw any) (bool, guardDecimal) {
	section, ok := raw.(map[string]any)
	if !ok {
		return defaultEmptyGuardEnabled, defaultEmptyGuardCostThresholdUSD
	}
	enabled := defaultEmptyGuardEnabled
	switch v := section["enabled"].(type) {
	case string:
		switch pystr.Lower(pystr.Strip(v)) {
		case "0", "false", "no", "off":
			enabled = false
		}
	case bool:
		enabled = v
	}
	threshold := defaultEmptyGuardCostThresholdUSD
	if thresholdRaw := section["cost_threshold_usd"]; thresholdRaw != nil {
		if _, isBool := thresholdRaw.(bool); !isBool {
			candidate, err := parseGuardDecimal(pyrepr.Str(thresholdRaw))
			sign := 0
			if err == nil {
				sign, err = candidate.sign()
			}
			if err != nil {
				emptyGuardLog().Debug(fmt.Sprintf("empty-guard: invalid cost_threshold_usd %s, using default", pyrepr.Repr(thresholdRaw)))
			} else if sign > 0 {
				threshold = candidate
			}
		}
	}
	return enabled, threshold
}

// Config resolved enabled flag, agents built without config enabled
func (a *AIAgent) emptyGuardIsEnabled() bool {
	if a.emptyGuardEnabled == nil {
		return defaultEmptyGuardEnabled
	}
	return *a.emptyGuardEnabled
}

// Resolved positive threshold, the default for an absent or non positive one
func (a *AIAgent) emptyGuardThreshold() guardDecimal {
	if t := a.emptyGuardCostThresholdUSD; t != nil {
		if sign, err := t.sign(); err == nil && sign > 0 {
			return *t
		}
	}
	return defaultEmptyGuardCostThresholdUSD
}

// Canonical usage of the response, false when it carries none
func (a *AIAgent) emptyGuardUsage(response any) (provider.CanonicalUsage, bool) {
	rawUsage := pyAttr(response, "usage")
	if !pyrepr.Truthy(rawUsage) {
		return provider.CanonicalUsage{}, false
	}
	return provider.NormalizeUsage(rawUsage, a.provider, a.apiMode), true
}

// Estimated USD input cost of one attempt, nil when pricing or usage is unknown
func estimateEmptyAttemptCost(ctx context.Context, a *AIAgent, response any) *big.Rat {
	canonical, ok := a.emptyGuardUsage(response)
	if !ok {
		return nil
	}
	return provider.EstimateUsageCost(ctx, a.model, canonical, a.provider, a.baseURL, a.apiKey).AmountUSD
}

// Usage presence and zero generation of a response, both false without evidence
func (a *AIAgent) emptyZeroOutput(response any) (usagePresent, zeroOutput bool) {
	canonical, ok := a.emptyGuardUsage(response)
	if !ok || canonical.PromptTokens() <= 0 {
		return false, false
	}
	return true, canonical.OutputTokens+canonical.ReasoningTokens == 0
}

// Records one empty completion, a zero retry counter starting a new streak with cleared history
func (a *AIAgent) recordEmptyAttempt(ctx context.Context, finishReason string, response any, observedGeneration bool) {
	if a.emptyContentRetries == 0 {
		a.emptyAttemptHistory = a.emptyAttemptHistory[:0]
		a.emptyStreakCostUSD = new(big.Rat)
	}
	usagePresent, zeroOutput := a.emptyZeroOutput(response)
	a.emptyAttemptHistory = append(a.emptyAttemptHistory, emptyAttempt{
		model: a.model, provider: a.provider, finishReason: finishReason,
		usagePresent: usagePresent, zeroOutput: zeroOutput, observedGeneration: observedGeneration,
	})
	if cost := estimateEmptyAttemptCostFn(ctx, a, response); cost != nil && cost.Sign() > 0 {
		prior := a.emptyStreakCostUSD
		if prior == nil {
			prior = new(big.Rat)
		}
		a.emptyStreakCostUSD = new(big.Rat).Add(prior, cost)
	}
}

// True when two or more same signature attempts all prove an empty by usage or by no observed generation
func (a *AIAgent) deterministicEmpty() bool {
	if !a.emptyGuardIsEnabled() || len(a.emptyAttemptHistory) < 2 {
		return false
	}
	first := a.emptyAttemptHistory[0].signature()
	sameSignature, usageProvesEmpty, responseProvesEmpty := true, true, true
	for _, e := range a.emptyAttemptHistory {
		sameSignature = sameSignature && e.signature() == first
		usageProvesEmpty = usageProvesEmpty && e.usagePresent && e.zeroOutput
		responseProvesEmpty = responseProvesEmpty && !e.usagePresent && !e.observedGeneration
	}
	return sameSignature && (usageProvesEmpty || responseProvesEmpty)
}

// Retry budget of the streak, one when a single attempt costs at least the threshold, three otherwise
func (a *AIAgent) emptyRetryBudget(ctx context.Context, response any) int {
	if !a.emptyGuardIsEnabled() {
		return defaultEmptyRetryBudget
	}
	if cost := estimateEmptyAttemptCostFn(ctx, a, response); cost != nil && a.emptyGuardThreshold().reachedBy(cost) {
		return reducedEmptyRetryBudget
	}
	return defaultEmptyRetryBudget
}

// Accumulated estimated cost of the current empty streak, nil when unknown or zero
func (a *AIAgent) streakCostUSD() *big.Rat {
	if a.emptyStreakCostUSD == nil || a.emptyStreakCostUSD.Sign() <= 0 {
		return nil
	}
	return a.emptyStreakCostUSD
}
