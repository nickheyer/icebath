package nemorelay

import (
	"context"
	"math/bits"
	"slices"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/nemorelay/codec"
	"github.com/nickheyer/icebath/pkg/nemorelay/types"
)

const (
	MaxLLMOptimizationContributions          = 64
	MaxLLMOptimizationContributionBytes      = 16 * 1024
	MaxLLMOptimizationTotalContributionBytes = 256 * 1024
	MaxLLMOptimizationContributionAttempts   = 64
)

// Bounded per-call accumulator of LLM optimization contributions
type LLMOptimizationRecorder struct {
	mu                        sync.Mutex
	settled                   *sync.Cond
	contributions             []types.LLMOptimizationContribution
	recordedAt                []time.Time
	totalContributionBytes    int
	attemptedContributions    int
	inFlightRecords           int
	emitted                   int
	closed                    bool
	finished                  bool
	limitations               map[string]struct{}
	contributionLimitExceeded bool
	invalidPayloadSchema      bool
}

func NewLLMOptimizationRecorder() *LLMOptimizationRecorder {
	recorder := &LLMOptimizationRecorder{limitations: map[string]struct{}{}}
	recorder.settled = sync.NewCond(&recorder.mu)
	return recorder
}

func (r *LLMOptimizationRecorder) sealForContributionLimit() {
	r.contributionLimitExceeded = true
	r.closed = true
}

// Records one contribution with an id and sequence, rejecting it once bounds are reached
func (r *LLMOptimizationRecorder) Record(contribution types.LLMOptimizationContribution) bool {
	if !r.reserveRecordAttempt() {
		return false
	}
	defer r.releaseRecordAttempt()
	if contributionHasPayload(contribution) && contribution.PayloadSchema == nil {
		r.noteInvalidPayloadSchema()
		return false
	}
	id := newUUID()
	contribution.ID = &id
	for {
		sequence, ok := r.nextContributionSequence()
		if !ok {
			return false
		}
		contribution.Sequence = &sequence
		size, ok := r.serializedContributionSize(contribution)
		if !ok {
			return false
		}
		switch r.commitContribution(contribution, size, sequence) {
		case contributionCommitted:
			return true
		case contributionRejected:
			return false
		}
	}
}

func (r *LLMOptimizationRecorder) reserveRecordAttempt() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.attemptedContributions >= MaxLLMOptimizationContributionAttempts {
		r.sealForContributionLimit()
		return false
	}
	r.attemptedContributions++
	r.inFlightRecords++
	return true
}

func (r *LLMOptimizationRecorder) releaseRecordAttempt() {
	r.mu.Lock()
	if r.inFlightRecords > 0 {
		r.inFlightRecords--
	}
	r.mu.Unlock()
	r.settled.Broadcast()
}

func (r *LLMOptimizationRecorder) nextContributionSequence() (uint64, bool) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, false
	}
	if len(r.contributions) >= MaxLLMOptimizationContributions {
		r.mu.Unlock()
		r.noteContributionLimitExceeded()
		return 0, false
	}
	sequence := uint64(len(r.contributions))
	r.mu.Unlock()
	return sequence, true
}

func (r *LLMOptimizationRecorder) serializedContributionSize(contribution types.LLMOptimizationContribution) (int, bool) {
	encoded, err := types.EncodeJSON(contribution)
	if err != nil {
		r.noteInvalidPayloadSchema()
		return 0, false
	}
	if len(encoded) > MaxLLMOptimizationContributionBytes {
		r.noteContributionLimitExceeded()
		return 0, false
	}
	return len(encoded), true
}

type contributionCommit int

const (
	contributionCommitted contributionCommit = iota
	contributionRetry
	contributionRejected
)

func (r *LLMOptimizationRecorder) commitContribution(contribution types.LLMOptimizationContribution, size int, sequence uint64) contributionCommit {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return contributionRejected
	}
	if uint64(len(r.contributions)) != sequence {
		return contributionRetry
	}
	total := r.totalContributionBytes + size
	if total < r.totalContributionBytes || total > MaxLLMOptimizationTotalContributionBytes {
		r.sealForContributionLimit()
		return contributionRejected
	}
	r.totalContributionBytes = total
	r.contributions = append(r.contributions, contribution)
	r.recordedAt = append(r.recordedAt, nowUTC())
	return contributionCommitted
}

func (r *LLMOptimizationRecorder) noteInvalidPayloadSchema() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.invalidPayloadSchema = true
	}
}

func (r *LLMOptimizationRecorder) noteContributionLimitExceeded() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.finished {
		r.sealForContributionLimit()
	}
}

// Records contributions in order and stops once the recorder closes
func (r *LLMOptimizationRecorder) recordAll(contributions []types.LLMOptimizationContribution) {
	for _, contribution := range contributions {
		if !r.Record(contribution) && r.isClosed() {
			return
		}
	}
}

func (r *LLMOptimizationRecorder) isClosed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

type timedContribution struct {
	contribution types.LLMOptimizationContribution
	recordedAt   time.Time
}

func (r *LLMOptimizationRecorder) unemittedWithTimestamps() []timedContribution {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := min(r.emitted, len(r.contributions))
	unemitted := make([]timedContribution, 0, len(r.contributions)-start)
	for index := start; index < len(r.contributions) && index < len(r.recordedAt); index++ {
		unemitted = append(unemitted, timedContribution{contribution: r.contributions[index], recordedAt: r.recordedAt[index]})
	}
	return unemitted
}

func (r *LLMOptimizationRecorder) markEmitted(count int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emitted = min(r.emitted+count, len(r.contributions))
}

func (r *LLMOptimizationRecorder) hasEvidenceLocked() bool {
	return len(r.contributions) > 0 || len(r.limitations) > 0 || r.contributionLimitExceeded || r.invalidPayloadSchema
}

// Closes the recorder for finalization and notes a limitation when evidence exists
func (r *LLMOptimizationRecorder) closeForFinalization(conditionalLimitation *string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return false
	}
	evidence := r.hasEvidenceLocked()
	if evidence && conditionalLimitation != nil {
		r.limitations[*conditionalLimitation] = struct{}{}
	}
	r.closed = true
	return evidence
}

type finishedContributions struct {
	contributions []types.LLMOptimizationContribution
	limitations   []string
}

// Waits for in-flight records and takes the contributions exactly once
func (r *LLMOptimizationRecorder) finish() finishedContributions {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished {
		return finishedContributions{}
	}
	r.closed = true
	for r.inFlightRecords > 0 {
		r.settled.Wait()
		if r.finished {
			return finishedContributions{}
		}
	}
	r.finished = true
	limitations := make([]string, 0, len(r.limitations)+2)
	for limitation := range r.limitations {
		limitations = append(limitations, limitation)
	}
	slices.Sort(limitations)
	r.limitations = map[string]struct{}{}
	if r.contributionLimitExceeded {
		limitations = append(limitations, "contribution_limit_exceeded")
		r.contributionLimitExceeded = false
	}
	if r.invalidPayloadSchema {
		limitations = append(limitations, "invalid_contribution_payload_schema")
		r.invalidPayloadSchema = false
	}
	r.recordedAt = nil
	contributions := r.contributions
	r.contributions = nil
	return finishedContributions{contributions: contributions, limitations: limitations}
}

func withLLMOptimizationRecorder(ctx context.Context, recorder *LLMOptimizationRecorder) context.Context {
	return context.WithValue(ctx, optimizationRecorderKey, recorder)
}

// Returns the optimization recorder of the LLM call the caller runs inside
func CurrentLLMOptimizationRecorder(ctx context.Context) *LLMOptimizationRecorder {
	if ctx == nil {
		return nil
	}
	recorder, _ := ctx.Value(optimizationRecorderKey).(*LLMOptimizationRecorder)
	return recorder
}

// Records a contribution on the current LLM call and reports whether it was kept
func RecordLLMOptimizationContribution(ctx context.Context, contribution types.LLMOptimizationContribution) bool {
	recorder := CurrentLLMOptimizationRecorder(ctx)
	return recorder != nil && recorder.Record(contribution)
}

type contributionAnalysis struct {
	limitations               []string
	tokenTotals               checkedTokenTotals
	baselineModel             *types.LLMOptimizationModel
	contributedEffectiveModel *types.LLMOptimizationModel
	useEffectiveAsBaseline    bool
}

func isModelRouting(contribution types.LLMOptimizationContribution) bool {
	return contribution.Kind.String() == types.LLMOptimizationKindModelRouting
}

func analyzeContributions(finished finishedContributions) contributionAnalysis {
	appliedRouting := make([]types.LLMOptimizationContribution, 0)
	for _, contribution := range finished.contributions {
		if contribution.Applied && isModelRouting(contribution) {
			appliedRouting = append(appliedRouting, contribution)
		}
	}
	routingAmbiguous := len(appliedRouting) > 1
	limitations := slices.Clone(finished.limitations)
	if routingAmbiguous {
		limitations = append(limitations, "multiple_routing_contributions")
	}
	var totals checkedTokenTotals
	for _, contribution := range finished.contributions {
		if !contribution.Applied {
			continue
		}
		if routingAmbiguous && isModelRouting(contribution) {
			continue
		}
		if contribution.TokenImpact != nil && contribution.TokenImpact.Saved != nil {
			totals.addContribution(*contribution.TokenImpact.Saved)
		}
	}
	if totals.missingTotal {
		limitations = append(limitations, "missing_token_savings_total")
	}
	if totals.inconsistentTotal {
		limitations = append(limitations, "inconsistent_token_savings_total")
	}
	analysis := contributionAnalysis{
		limitations:            limitations,
		tokenTotals:            totals,
		useEffectiveAsBaseline: len(appliedRouting) == 0 || routingAmbiguous,
	}
	if len(appliedRouting) == 1 && appliedRouting[0].ModelTransition != nil {
		analysis.baselineModel = cloneOptimizationModel(appliedRouting[0].ModelTransition.Baseline)
		analysis.contributedEffectiveModel = cloneOptimizationModel(appliedRouting[0].ModelTransition.Effective)
	}
	return analysis
}

func cloneOptimizationModel(model *types.LLMOptimizationModel) *types.LLMOptimizationModel {
	if model == nil {
		return nil
	}
	return &types.LLMOptimizationModel{Model: model.Model, Provider: cloneStringPtr(model.Provider)}
}

func resolveEffectiveModel(contributed *types.LLMOptimizationModel, response *types.AnnotatedLLMResponse, requestedModel *string) *types.LLMOptimizationModel {
	if contributed != nil {
		return contributed
	}
	if response != nil && response.Model != nil {
		return &types.LLMOptimizationModel{Model: *response.Model}
	}
	if requestedModel != nil {
		return &types.LLMOptimizationModel{Model: *requestedModel}
	}
	return nil
}

type usageAnalysis struct {
	effective                    *types.Usage
	baseline                     *types.Usage
	tokenCountOverflow           bool
	baselineDerivationIncomplete bool
}

func cloneUint64Ptr(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}

func uint64Ptr(value uint64) *uint64 {
	return &value
}

func checkedAdd(left, right uint64) (uint64, bool) {
	sum, carry := bits.Add64(left, right, 0)
	return sum, carry == 0
}

func deriveOptimizationUsage(response *types.AnnotatedLLMResponse, tokensSaved types.LLMOptimizationTokens, totals checkedTokenTotals, limitations *[]string) usageAnalysis {
	var effective *types.Usage
	if response != nil && response.Usage != nil {
		effective = cloneUsage(response.Usage)
	}
	overflow := totals.overflow.any()
	incomplete := totals.missingTotal || totals.inconsistentTotal
	if effective != nil {
		noteMissingCoreUsage(effective, limitations, &overflow)
	}
	if effective != nil && effective.TotalTokens != nil && response != nil && response.Usage != nil && response.Usage.TotalTokens == nil {
		response.Usage.TotalTokens = cloneUint64Ptr(effective.TotalTokens)
	}
	var apiSplit *codec.InputSplit
	if response != nil && response.APISpecific != nil {
		if split, ok := codec.InputSplitOfAPI(response.APISpecific); ok {
			apiSplit = &split
		}
	}
	var baseline *types.Usage
	if effective != nil {
		baseline = deriveBaselineUsage(effective, apiSplit, tokensSaved, totals, limitations, &overflow, &incomplete)
	}
	if overflow {
		*limitations = append(*limitations, "token_count_overflow")
	}
	return usageAnalysis{effective: effective, baseline: baseline, tokenCountOverflow: overflow, baselineDerivationIncomplete: incomplete}
}

func noteMissingCoreUsage(usage *types.Usage, limitations *[]string, overflow *bool) {
	if usage.PromptTokens == nil {
		*limitations = append(*limitations, "missing_effective_prompt_tokens")
	}
	if usage.CompletionTokens == nil {
		*limitations = append(*limitations, "missing_effective_completion_tokens")
	}
	if usage.TotalTokens == nil {
		if usage.PromptTokens != nil && usage.CompletionTokens != nil {
			if total, ok := checkedAdd(*usage.PromptTokens, *usage.CompletionTokens); ok {
				usage.TotalTokens = uint64Ptr(total)
			} else {
				*overflow = true
			}
		} else {
			*limitations = append(*limitations, "missing_effective_total_tokens")
		}
	}
}

func deriveBaselineUsage(usage *types.Usage, apiSplit *codec.InputSplit, saved types.LLMOptimizationTokens, totals checkedTokenTotals, limitations *[]string, overflow *bool, incomplete *bool) *types.Usage {
	baseline := cloneUsage(usage)
	baseline.Cost = nil
	fields := []struct {
		observed   **uint64
		saved      *uint64
		overflowed bool
		missing    string
	}{
		{&baseline.PromptTokens, saved.PromptTokens, totals.overflow.prompt, "missing_effective_prompt_tokens"},
		{&baseline.CompletionTokens, saved.CompletionTokens, totals.overflow.completion, "missing_effective_completion_tokens"},
		{&baseline.CacheReadTokens, saved.CacheReadTokens, totals.overflow.cacheRead, "missing_effective_cache_read_tokens"},
		{&baseline.CacheWriteTokens, saved.CacheWriteTokens, totals.overflow.cacheWrite, "missing_effective_cache_write_tokens"},
		{&baseline.TotalTokens, saved.TotalTokens, totals.overflow.total, "missing_effective_total_tokens"},
	}
	for _, field := range fields {
		if checkedAddObservedTokens(field.observed, field.saved, field.overflowed, field.missing, limitations, incomplete) {
			*overflow = true
		}
	}
	uncached, outcome := baselineUncachedInputTokens(usage, apiSplit, totals)
	switch outcome {
	case baselineInputOverflow:
		*overflow = true
		baseline.UncachedInputTokens = nil
	case baselineInputInconsistent:
		*limitations = append(*limitations, "inconsistent_baseline_input_split")
		*incomplete = true
		baseline.UncachedInputTokens = nil
	case baselineInputUndetermined:
		*limitations = append(*limitations, "undetermined_baseline_input_split")
		*incomplete = true
		baseline.UncachedInputTokens = nil
	default:
		baseline.UncachedInputTokens = uncached
	}
	return baseline
}

type baselineInputOutcome int

const (
	baselineInputOK baselineInputOutcome = iota
	baselineInputOverflow
	baselineInputInconsistent
	baselineInputUndetermined
)

func baselineUncachedInputTokens(usage *types.Usage, apiSplit *codec.InputSplit, totals checkedTokenTotals) (*uint64, baselineInputOutcome) {
	if usage.UncachedInputTokens == nil {
		return nil, baselineInputOK
	}
	uncached := *usage.UncachedInputTokens
	split, ok := codec.InputSplitFor(usage)
	if !ok {
		return nil, baselineInputInconsistent
	}
	if totals.overflow.prompt || totals.overflow.cacheRead || totals.overflow.cacheWrite {
		return nil, baselineInputOverflow
	}
	savedPrompt := uint64(0)
	if totals.values.PromptTokens != nil {
		savedPrompt = *totals.values.PromptTokens
	}
	withSavedPrompt, ok := checkedAdd(uncached, savedPrompt)
	if !ok {
		return nil, baselineInputOverflow
	}
	savedCache := uint64(0)
	for _, value := range []*uint64{totals.values.CacheReadTokens, totals.values.CacheWriteTokens} {
		if value == nil {
			continue
		}
		next, ok := checkedAdd(savedCache, *value)
		if !ok {
			return nil, baselineInputOverflow
		}
		savedCache = next
	}
	if savedCache == 0 {
		return uint64Ptr(withSavedPrompt), baselineInputOK
	}
	var inclusive bool
	switch split {
	case codec.InputSplitInclusive:
		inclusive = true
	case codec.InputSplitSeparate:
		inclusive = false
	default:
		if apiSplit == nil {
			return nil, baselineInputUndetermined
		}
		inclusive = *apiSplit == codec.InputSplitInclusive
	}
	if !inclusive {
		return uint64Ptr(withSavedPrompt), baselineInputOK
	}
	if withSavedPrompt < savedCache {
		return nil, baselineInputInconsistent
	}
	return uint64Ptr(withSavedPrompt - savedCache), baselineInputOK
}

type pricingAnalysis struct {
	baselineCost      *types.CostEstimate
	actualCost        *types.CostEstimate
	completeCoreUsage bool
}

func priceOptimizationUsage(effectiveUsage *types.Usage, response *types.AnnotatedLLMResponse, effectiveModel, baselineModel *types.LLMOptimizationModel, baselineUsage *types.Usage, overflow, incomplete bool, pricing *codec.PricingResolver) pricingAnalysis {
	var providerReported *types.CostEstimate
	if effectiveUsage != nil && effectiveUsage.Cost != nil && effectiveUsage.Cost.Source == types.CostSourceProviderReported {
		providerReported = cloneCostEstimate(effectiveUsage.Cost)
	}
	complete := effectiveUsage != nil && effectiveUsage.PromptTokens != nil && effectiveUsage.CompletionTokens != nil
	actual := providerReported
	if actual == nil && complete && effectiveModel != nil {
		actual = pricing.EstimateCostForProvider(effectiveModel.Provider, effectiveModel.Model, effectiveUsage)
	}
	if effectiveUsage != nil {
		effectiveUsage.Cost = cloneCostEstimate(actual)
	}
	if response != nil && response.Usage != nil {
		response.Usage.Cost = cloneCostEstimate(actual)
	}
	var baselineCost *types.CostEstimate
	if !overflow && !incomplete && complete && baselineModel != nil && baselineUsage != nil {
		baselineCost = pricing.EstimateCostForProvider(baselineModel.Provider, baselineModel.Model, baselineUsage)
	}
	return pricingAnalysis{baselineCost: baselineCost, actualCost: actual, completeCoreUsage: complete}
}

func addSummaryLimitations(limitations *[]string, effectiveUsage *types.Usage, effectiveModel, baselineModel *types.LLMOptimizationModel, baselineCost, actualCost *types.CostEstimate, overflow, incomplete, complete bool) {
	if effectiveUsage == nil {
		*limitations = append(*limitations, "missing_effective_usage")
	}
	if effectiveModel == nil {
		*limitations = append(*limitations, "missing_effective_model")
	}
	if baselineModel == nil {
		*limitations = append(*limitations, "missing_baseline_model")
	}
	if baselineCost == nil && baselineModel != nil && !overflow && !incomplete && complete {
		*limitations = append(*limitations, "missing_baseline_pricing")
	}
	if actualCost == nil {
		*limitations = append(*limitations, "missing_actual_cost")
	}
}

// Builds the optimization summary of a finished LLM call and stores it on the response
func finalizeOptimizationSummary(recorder *LLMOptimizationRecorder, response *types.AnnotatedLLMResponse, requestedModel *string, pricing *codec.PricingResolver) *types.LLMOptimizationSummary {
	finished := recorder.finish()
	if len(finished.contributions) == 0 && len(finished.limitations) == 0 {
		return nil
	}
	analysis := analyzeContributions(finished)
	effectiveModel := resolveEffectiveModel(analysis.contributedEffectiveModel, response, requestedModel)
	if analysis.useEffectiveAsBaseline && analysis.baselineModel == nil {
		analysis.baselineModel = cloneOptimizationModel(effectiveModel)
	}
	baselineModel := analysis.baselineModel
	tokensSaved := analysis.tokenTotals.values
	limitations := analysis.limitations
	totals := analysis.tokenTotals
	overflow := totals.overflow.any()
	usage := deriveOptimizationUsage(response, tokensSaved, totals, &limitations)
	effectiveUsage := usage.effective
	overflow = overflow || usage.tokenCountOverflow
	pricingResult := priceOptimizationUsage(effectiveUsage, response, effectiveModel, baselineModel, usage.baseline, overflow, usage.baselineDerivationIncomplete, pricing)
	addSummaryLimitations(&limitations, effectiveUsage, effectiveModel, baselineModel, pricingResult.baselineCost, pricingResult.actualCost, overflow, usage.baselineDerivationIncomplete, pricingResult.completeCoreUsage)
	saved, currency := calculateEstimatedCostSaved(pricingResult.baselineCost, pricingResult.actualCost, &limitations)
	slices.Sort(limitations)
	limitations = slices.Compact(limitations)
	status := types.LLMOptimizationSummaryStatusPartial
	if len(limitations) == 0 {
		status = types.LLMOptimizationSummaryStatusComplete
	}
	contributions := finished.contributions
	if contributions == nil {
		contributions = []types.LLMOptimizationContribution{}
	}
	summary := &types.LLMOptimizationSummary{
		SchemaVersion:      "1",
		CalculationVersion: "1",
		Status:             status,
		Limitations:        limitations,
		BaselineModel:      baselineModel,
		EffectiveModel:     effectiveModel,
		EffectiveUsage:     effectiveUsage,
		BaselineUsage:      usage.baseline,
		TokensSaved:        tokensSaved,
		BaselineCost:       pricingResult.baselineCost,
		ActualCost:         pricingResult.actualCost,
		EstimatedCostSaved: saved,
		Currency:           currency,
		Contributions:      contributions,
	}
	if response != nil {
		response.OptimizationSummary = summary
	}
	return summary
}

func calculateEstimatedCostSaved(baselineCost, actualCost *types.CostEstimate, limitations *[]string) (*float64, *string) {
	var baselineTotal, actualTotal *float64
	if baselineCost != nil {
		baselineTotal = baselineCost.TotalOrComponentSum()
		if baselineTotal == nil {
			*limitations = append(*limitations, "missing_baseline_cost_total")
		}
	}
	if actualCost != nil {
		actualTotal = actualCost.TotalOrComponentSum()
		if actualTotal == nil {
			*limitations = append(*limitations, "missing_actual_cost_total")
		}
	}
	if baselineCost == nil || actualCost == nil {
		return nil, nil
	}
	if !asciiEqualFold(baselineCost.Currency, actualCost.Currency) {
		*limitations = append(*limitations, "cost_currency_mismatch")
		return nil, nil
	}
	if baselineTotal == nil || actualTotal == nil {
		return nil, nil
	}
	saved := *baselineTotal - *actualTotal
	currency := baselineCost.Currency
	return &saved, &currency
}

type tokenOverflow struct {
	prompt, completion, cacheRead, cacheWrite, total bool
}

func (o tokenOverflow) any() bool {
	return o.prompt || o.completion || o.cacheRead || o.cacheWrite || o.total
}

type checkedTokenTotals struct {
	values            types.LLMOptimizationTokens
	overflow          tokenOverflow
	missingTotal      bool
	inconsistentTotal bool
}

func (t *checkedTokenTotals) addContribution(other types.LLMOptimizationTokens) {
	checkedAccumulate(&t.values.PromptTokens, other.PromptTokens, &t.overflow.prompt)
	checkedAccumulate(&t.values.CompletionTokens, other.CompletionTokens, &t.overflow.completion)
	checkedAccumulate(&t.values.CacheReadTokens, other.CacheReadTokens, &t.overflow.cacheRead)
	checkedAccumulate(&t.values.CacheWriteTokens, other.CacheWriteTokens, &t.overflow.cacheWrite)
	derivedTotal, derivedOverflow := checkedOptionSum(other.PromptTokens, other.CompletionTokens)
	t.overflow.total = t.overflow.total || derivedOverflow
	if derivedOverflow {
		t.values.TotalTokens = nil
	}
	if other.TotalTokens != nil && other.PromptTokens != nil && other.CompletionTokens != nil {
		if derived, ok := checkedAdd(*other.PromptTokens, *other.CompletionTokens); ok && derived != *other.TotalTokens {
			t.inconsistentTotal = true
		}
	}
	contributionTotal := other.TotalTokens
	if contributionTotal == nil {
		contributionTotal = derivedTotal
	}
	if contributionTotal == nil {
		t.missingTotal = true
	}
	checkedAccumulate(&t.values.TotalTokens, contributionTotal, &t.overflow.total)
}

func checkedAccumulate(target **uint64, value *uint64, overflowed *bool) {
	if *overflowed || value == nil {
		return
	}
	current := uint64(0)
	if *target != nil {
		current = **target
	}
	total, ok := checkedAdd(current, *value)
	if !ok {
		*target = nil
		*overflowed = true
		return
	}
	*target = uint64Ptr(total)
}

func checkedAddObservedTokens(target **uint64, value *uint64, valueOverflowed bool, missing string, limitations *[]string, incomplete *bool) bool {
	if valueOverflowed {
		*target = nil
		*incomplete = true
		return true
	}
	if value == nil {
		return false
	}
	if *target == nil {
		*limitations = append(*limitations, missing)
		*incomplete = true
		return false
	}
	total, ok := checkedAdd(**target, *value)
	if !ok {
		*target = nil
		return true
	}
	*target = uint64Ptr(total)
	return false
}

func checkedOptionSum(values ...*uint64) (*uint64, bool) {
	present := false
	total := uint64(0)
	for _, value := range values {
		if value == nil {
			continue
		}
		present = true
		next, ok := checkedAdd(total, *value)
		if !ok {
			return nil, true
		}
		total = next
	}
	if !present {
		return nil, false
	}
	return uint64Ptr(total), false
}

// Reports whether a contribution carries a payload, null included
func contributionHasPayload(contribution types.LLMOptimizationContribution) bool {
	return contribution.Payload != nil || contribution.PayloadPresent
}

func cloneCostEstimate(cost *types.CostEstimate) *types.CostEstimate {
	if cost == nil {
		return nil
	}
	copied := *cost
	return &copied
}

func cloneUsage(usage *types.Usage) *types.Usage {
	if usage == nil {
		return nil
	}
	copied := *usage
	copied.Cost = cloneCostEstimate(usage.Cost)
	return &copied
}
