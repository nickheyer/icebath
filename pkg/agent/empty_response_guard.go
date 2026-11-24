package agent

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

const (
	defaultEmptyRetryBudget = 3
	reducedEmptyRetryBudget = 1
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
	pyDecimalLiteral           = regexp.MustCompile(`^([+-]?)(?:(\d(?:_?\d)*)(?:\.(\d(?:_?\d)*)?)?|\.(\d(?:_?\d)*))(?:[eE]([+-]?\d(?:_?\d)*))?$`)
	pyDecimalSpecial           = regexp.MustCompile(`(?i)^([+-]?)(inf|infinity|nan|snan)(\d*)$`)
	errDecimalNaNCompare       = errors.New("[<class 'decimal.InvalidOperation'>]")
	estimateEmptyAttemptCostFn = estimateEmptyAttemptCost
)

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

// Decimal of a cost in dollars, nil when it is no positive finite or infinite amount
func guardDecimalOf(usd float64) *guardDecimal {
	d, err := parseGuardDecimal(strconv.FormatFloat(usd, 'g', -1, 64))
	if err != nil {
		return nil
	}
	if sign, err := d.sign(); err != nil || sign <= 0 {
		return nil
	}
	return &d
}

// Cost threshold the registry gives the guard when none is set
func defaultEmptyGuardCostThresholdUSD() guardDecimal {
	return *guardDecimalOf(settings.AgentEmptyResponseGuardCostThresholdUsd.Default)
}

// Enabled flag and cost threshold of the empty response guard in effect for ctx, a threshold that is not above zero giving the default
func resolveGuardSettings(ctx context.Context) (bool, guardDecimal) {
	threshold := defaultEmptyGuardCostThresholdUSD()
	usd := settings.AgentEmptyResponseGuardCostThresholdUsd.Get(ctx)
	if d := guardDecimalOf(usd); d != nil {
		threshold = *d
	} else {
		pkgLog().Debug(fmt.Sprintf("empty-guard: cost_threshold_usd %s is not above zero, using default", strconv.FormatFloat(usd, 'g', -1, 64)))
	}
	return settings.AgentEmptyResponseGuardEnabled.Get(ctx), threshold
}

// Config resolved enabled flag, agents built without config taking the registry default
func (a *AIAgent) emptyGuardIsEnabled() bool {
	if a.emptyGuardEnabled == nil {
		return settings.AgentEmptyResponseGuardEnabled.Default
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
	return defaultEmptyGuardCostThresholdUSD()
}

// Attempt history of the current empty streak, the list the agent itself holds
func (a *AIAgent) emptyAttempts() *[]emptyAttempt { return &a.emptyAttemptHistory }

// Canonical usage of the response, false when it carries none or its normalization fails
func (a *AIAgent) emptyGuardUsage(response any, what string) (usage provider.CanonicalUsage, ok bool) {
	rawUsage := state.ToolCallField(response, "usage")
	if !anyutil.IsTrue(rawUsage) {
		return provider.CanonicalUsage{}, false
	}
	defer func() {
		if rec := recover(); rec != nil {
			pkgLog().Debug(fmt.Sprintf("empty-guard: %s failed", what), "error", rec)
			usage, ok = provider.CanonicalUsage{}, false
		}
	}()
	return provider.NormalizeUsage(rawUsage, a.provider, a.apiMode), true
}

// Estimated USD input cost of one attempt, nil when pricing or usage is unknown or the estimate fails
func estimateEmptyAttemptCost(ctx context.Context, a *AIAgent, response any) (cost *big.Rat) {
	canonical, ok := a.emptyGuardUsage(response, "cost estimation")
	if !ok {
		return nil
	}
	defer func() {
		if rec := recover(); rec != nil {
			pkgLog().Debug("empty-guard: cost estimation failed", "error", rec)
			cost = nil
		}
	}()
	return provider.EstimateUsageCost(ctx, a.model, canonical, a.provider, a.baseURL, a.apiKey).AmountUSD
}

// Usage presence and zero generation of a response, both false without evidence
func (a *AIAgent) emptyZeroOutput(response any) (usagePresent, zeroOutput bool) {
	canonical, ok := a.emptyGuardUsage(response, "usage normalization")
	if !ok || canonical.PromptTokens() <= 0 {
		return false, false
	}
	return true, canonical.OutputTokens+canonical.ReasoningTokens == 0
}

// Records one empty completion, a zero retry counter starting a new streak with cleared history
func (a *AIAgent) recordEmptyAttempt(ctx context.Context, finishReason string, response any, observedGeneration bool) {
	attempts := a.emptyAttempts()
	if a.emptyContentRetries == 0 {
		*attempts = (*attempts)[:0]
		a.emptyStreakCostUSD = new(big.Rat)
	}
	usagePresent, zeroOutput := a.emptyZeroOutput(response)
	*attempts = append(*attempts, emptyAttempt{
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
