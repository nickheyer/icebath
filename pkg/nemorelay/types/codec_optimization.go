package types

import (
	"strings"

	"github.com/google/uuid"
)

// LLMOptimizationKind names a kind of request optimization
type LLMOptimizationKind string

const (
	LLMOptimizationKindInputCompression LLMOptimizationKind = "input_compression"
	LLMOptimizationKindModelRouting     LLMOptimizationKind = "model_routing"
)

// AsStr returns the kind text
func (k LLMOptimizationKind) AsStr() string { return string(k) }

func (k LLMOptimizationKind) rustDebug(b *strings.Builder) {
	b.WriteString("LlmOptimizationKind(" + RustStrDebug(string(k)) + ")")
}

// LLMOptimizationModel names a model and optionally its provider
type LLMOptimizationModel struct {
	Model    string  `json:"model"`
	Provider *string `json:"provider" serde:"default,skip_none"`
}

func (LLMOptimizationModel) rustName() string { return "LlmOptimizationModel" }

// NewLLMOptimizationModel names a model without provider
func NewLLMOptimizationModel(model string) LLMOptimizationModel {
	return LLMOptimizationModel{Model: model}
}

// WithProvider returns a copy naming the provider
func (m LLMOptimizationModel) WithProvider(provider string) LLMOptimizationModel {
	m.Provider = &provider
	return m
}

// MarshalJSON writes the serde form
func (m LLMOptimizationModel) MarshalJSON() ([]byte, error) { return EncodeJSON(m) }

// UnmarshalJSON reads the serde form
func (m *LLMOptimizationModel) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, m) }

// LLMOptimizationModelTransition records the baseline and effective models
type LLMOptimizationModelTransition struct {
	Baseline  *LLMOptimizationModel `json:"baseline" serde:"default,skip_none"`
	Effective *LLMOptimizationModel `json:"effective" serde:"default,skip_none"`
}

func (LLMOptimizationModelTransition) rustName() string { return "LlmOptimizationModelTransition" }

// MarshalJSON writes the serde form
func (t LLMOptimizationModelTransition) MarshalJSON() ([]byte, error) { return EncodeJSON(t) }

// UnmarshalJSON reads the serde form
func (t *LLMOptimizationModelTransition) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, t) }

// LLMOptimizationTokens holds token counts of one side of an optimization
type LLMOptimizationTokens struct {
	PromptTokens     *uint64 `json:"prompt_tokens" serde:"default,skip_none"`
	CompletionTokens *uint64 `json:"completion_tokens" serde:"default,skip_none"`
	CacheReadTokens  *uint64 `json:"cache_read_tokens" serde:"default,skip_none"`
	CacheWriteTokens *uint64 `json:"cache_write_tokens" serde:"default,skip_none"`
	TotalTokens      *uint64 `json:"total_tokens" serde:"default,skip_none"`
}

func (LLMOptimizationTokens) rustName() string { return "LlmOptimizationTokens" }

// SavedPromptTokens records saved prompt tokens as both prompt and total
func SavedPromptTokens(promptTokens uint64) LLMOptimizationTokens {
	total := promptTokens
	return LLMOptimizationTokens{PromptTokens: &promptTokens, TotalTokens: &total}
}

// MarshalJSON writes the serde form
func (t LLMOptimizationTokens) MarshalJSON() ([]byte, error) { return EncodeJSON(t) }

// UnmarshalJSON reads the serde form
func (t *LLMOptimizationTokens) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, t) }

// LLMOptimizationEvidenceQuality says whether token impact was observed or estimated
type LLMOptimizationEvidenceQuality uint8

const (
	LLMOptimizationEvidenceQualityObserved LLMOptimizationEvidenceQuality = iota + 1
	LLMOptimizationEvidenceQualityEstimated
)

var evidenceQualityNames = []string{"observed", "estimated"}

func (q LLMOptimizationEvidenceQuality) serdeSerialize() (any, error) {
	if q < 1 || int(q) > len(evidenceQualityNames) {
		return nil, nil
	}
	return evidenceQualityNames[q-1], nil
}

func (q *LLMOptimizationEvidenceQuality) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, evidenceQualityNames)
	if err != nil {
		return err
	}
	*q = LLMOptimizationEvidenceQuality(idx + 1)
	return nil
}

func (q LLMOptimizationEvidenceQuality) rustDebug(b *strings.Builder) {
	switch q {
	case LLMOptimizationEvidenceQualityObserved:
		b.WriteString("Observed")
	case LLMOptimizationEvidenceQualityEstimated:
		b.WriteString("Estimated")
	}
}

// MarshalJSON writes the serde form
func (q LLMOptimizationEvidenceQuality) MarshalJSON() ([]byte, error) { return EncodeJSON(q) }

// UnmarshalJSON reads the serde form
func (q *LLMOptimizationEvidenceQuality) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, q) }

// LLMOptimizationTokenImpact compares baseline and effective token counts
type LLMOptimizationTokenImpact struct {
	Baseline         *LLMOptimizationTokens          `json:"baseline" serde:"default,skip_none"`
	Effective        *LLMOptimizationTokens          `json:"effective" serde:"default,skip_none"`
	Saved            *LLMOptimizationTokens          `json:"saved" serde:"default,skip_none"`
	Quality          *LLMOptimizationEvidenceQuality `json:"quality" serde:"default,skip_none"`
	EstimationMethod *string                         `json:"estimation_method" serde:"default,skip_none"`
}

func (LLMOptimizationTokenImpact) rustName() string { return "LlmOptimizationTokenImpact" }

// MarshalJSON writes the serde form
func (t LLMOptimizationTokenImpact) MarshalJSON() ([]byte, error) { return EncodeJSON(t) }

// UnmarshalJSON reads the serde form
func (t *LLMOptimizationTokenImpact) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, t) }

// LLMOptimizationPayload is a typed optimization payload with a schema name and version
type LLMOptimizationPayload interface {
	SchemaName() string
	SchemaVersion() string
}

// LLMOptimizationContribution records what one optimizer did to a request
type LLMOptimizationContribution struct {
	ID              *uuid.UUID                      `json:"id" serde:"default,skip_none"`
	Sequence        *uint64                         `json:"sequence" serde:"default,skip_none"`
	Producer        string                          `json:"producer"`
	Kind            LLMOptimizationKind             `json:"kind"`
	Applied         bool                            `json:"applied" serde:"default"`
	ModelTransition *LLMOptimizationModelTransition `json:"model_transition" serde:"default,skip_none"`
	TokenImpact     *LLMOptimizationTokenImpact     `json:"token_impact" serde:"default,skip_none"`
	PayloadSchema   *DataSchema                     `json:"payload_schema" serde:"default,skip_none"`
	Payload         any                             `json:"payload" serde:"default,option,skip_none"`
	PayloadPresent  bool                            `json:"-"`
	Extra           map[string]any                  `json:"extra" serde:"flatten"`
}

func (LLMOptimizationContribution) rustName() string { return "LlmOptimizationContribution" }

// NewLLMOptimizationContribution builds an applied contribution for a producer and kind
func NewLLMOptimizationContribution(producer string, kind LLMOptimizationKind) LLMOptimizationContribution {
	return LLMOptimizationContribution{Producer: producer, Kind: kind, Applied: true, Extra: map[string]any{}}
}

// WithPayload attaches a typed payload and its schema
func (c LLMOptimizationContribution) WithPayload(payload LLMOptimizationPayload) (LLMOptimizationContribution, error) {
	c.PayloadSchema = &DataSchema{Name: payload.SchemaName(), Version: payload.SchemaVersion()}
	value, err := ToJSONValue(payload)
	if err != nil {
		return c, err
	}
	c.Payload = value
	c.PayloadPresent = true
	return c, nil
}

// MarshalJSON writes the serde form
func (c LLMOptimizationContribution) MarshalJSON() ([]byte, error) { return EncodeJSON(c) }

// UnmarshalJSON reads the serde form
func (c *LLMOptimizationContribution) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, c) }

// LLMOptimizationSummaryStatus says whether a summary covers every contribution
type LLMOptimizationSummaryStatus uint8

const (
	LLMOptimizationSummaryStatusComplete LLMOptimizationSummaryStatus = iota + 1
	LLMOptimizationSummaryStatusPartial
)

var summaryStatusNames = []string{"complete", "partial"}

func (s LLMOptimizationSummaryStatus) serdeSerialize() (any, error) {
	if s < 1 || int(s) > len(summaryStatusNames) {
		return nil, nil
	}
	return summaryStatusNames[s-1], nil
}

func (s *LLMOptimizationSummaryStatus) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, summaryStatusNames)
	if err != nil {
		return err
	}
	*s = LLMOptimizationSummaryStatus(idx + 1)
	return nil
}

func (s LLMOptimizationSummaryStatus) rustDebug(b *strings.Builder) {
	switch s {
	case LLMOptimizationSummaryStatusComplete:
		b.WriteString("Complete")
	case LLMOptimizationSummaryStatusPartial:
		b.WriteString("Partial")
	}
}

// MarshalJSON writes the serde form
func (s LLMOptimizationSummaryStatus) MarshalJSON() ([]byte, error) { return EncodeJSON(s) }

// UnmarshalJSON reads the serde form
func (s *LLMOptimizationSummaryStatus) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, s) }

// LLMOptimizationSummary totals the effect of every optimization on one call
type LLMOptimizationSummary struct {
	SchemaVersion      string                        `json:"schema_version"`
	CalculationVersion string                        `json:"calculation_version"`
	Status             LLMOptimizationSummaryStatus  `json:"status"`
	Limitations        []string                      `json:"limitations" serde:"default,skip_empty"`
	BaselineModel      *LLMOptimizationModel         `json:"baseline_model" serde:"default,skip_none"`
	EffectiveModel     *LLMOptimizationModel         `json:"effective_model" serde:"default,skip_none"`
	EffectiveUsage     *Usage                        `json:"effective_usage" serde:"default,skip_none"`
	BaselineUsage      *Usage                        `json:"baseline_usage" serde:"default,skip_none"`
	TokensSaved        LLMOptimizationTokens         `json:"tokens_saved"`
	BaselineCost       *CostEstimate                 `json:"baseline_cost" serde:"default,skip_none"`
	ActualCost         *CostEstimate                 `json:"actual_cost" serde:"default,skip_none"`
	EstimatedCostSaved *float64                      `json:"estimated_cost_saved" serde:"default,skip_none"`
	Currency           *string                       `json:"currency" serde:"default,skip_none"`
	Contributions      []LLMOptimizationContribution `json:"contributions"`
}

func (LLMOptimizationSummary) rustName() string { return "LlmOptimizationSummary" }

// MarshalJSON writes the serde form
func (s LLMOptimizationSummary) MarshalJSON() ([]byte, error) { return EncodeJSON(s) }

// UnmarshalJSON reads the serde form
func (s *LLMOptimizationSummary) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, s) }
