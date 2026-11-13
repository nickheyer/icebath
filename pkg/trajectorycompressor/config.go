package trajectorycompressor

import (
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// OpenRouterBaseURL is the default summarization endpoint
const OpenRouterBaseURL = "https://openrouter.ai/api/v1"

// CompressionConfig holds tokenizer, targets, protected turns, summarizer, output, processing and metrics settings
type CompressionConfig struct {
	TokenizerName         string
	TargetMaxTokens       int
	SummaryTargetTokens   int
	ProtectFirstSystem    bool
	ProtectFirstHuman     bool
	ProtectFirstGPT       bool
	ProtectFirstTool      bool
	ProtectLastNTurns     int
	SummarizationModel    string
	BaseURL               string
	APIKeyEnv             string
	Temperature           float64
	MaxRetries            int
	RetryDelay            float64
	AddSummaryNotice      bool
	SummaryNoticeText     string
	OutputSuffix          string
	NumWorkers            int
	MaxConcurrentRequests int
	SkipUnderTarget       bool
	SaveOverLimit         bool
	PerTrajectoryTimeout  int
	MetricsEnabled        bool
	MetricsPerTrajectory  bool
	MetricsOutputFile     string
}

// Configuration with the hermes dataclass defaults
func DefaultConfig() *CompressionConfig {
	return &CompressionConfig{
		TokenizerName: "moonshotai/Kimi-K2-Thinking", TargetMaxTokens: 15250, SummaryTargetTokens: 750,
		ProtectFirstSystem: true, ProtectFirstHuman: true, ProtectFirstGPT: true, ProtectFirstTool: true, ProtectLastNTurns: 4,
		SummarizationModel: "google/gemini-3-flash-preview", BaseURL: OpenRouterBaseURL, APIKeyEnv: "OPENROUTER_API_KEY",
		Temperature: 0.3, MaxRetries: 3, RetryDelay: 2, AddSummaryNotice: true,
		SummaryNoticeText: "\n\nSome of your previous tool responses may be summarized to preserve context.",
		OutputSuffix:      "_compressed", NumWorkers: 4, MaxConcurrentRequests: 50, SkipUnderTarget: true, SaveOverLimit: true,
		PerTrajectoryTimeout: 300, MetricsEnabled: true, MetricsPerTrajectory: true, MetricsOutputFile: "compression_metrics.json",
	}
}

// YAML section to keys, a yaml key paired with its attribute name where the two differ
var yamlSections = []struct {
	section string
	keys    []string
}{
	{"tokenizer", []string{"name:tokenizer_name"}},
	{"compression", []string{"target_max_tokens", "summary_target_tokens"}},
	{"protected_turns", []string{"first_system:protect_first_system", "first_human:protect_first_human",
		"first_gpt:protect_first_gpt", "first_tool:protect_first_tool", "last_n_turns:protect_last_n_turns"}},
	{"summarization", []string{"model:summarization_model", "base_url", "api_key_env", "temperature", "max_retries", "retry_delay"}},
	{"output", []string{"add_summary_notice", "summary_notice_text", "output_suffix"}},
	{"processing", []string{"num_workers", "max_concurrent_requests", "skip_under_target", "save_over_limit"}},
	{"metrics", []string{"enabled:metrics_enabled", "per_trajectory:metrics_per_trajectory", "output_file:metrics_output_file"}},
}

// Pointer to the config field an attribute name addresses
func (c *CompressionConfig) field(attr string) any {
	switch attr {
	case "tokenizer_name":
		return &c.TokenizerName
	case "target_max_tokens":
		return &c.TargetMaxTokens
	case "summary_target_tokens":
		return &c.SummaryTargetTokens
	case "protect_first_system":
		return &c.ProtectFirstSystem
	case "protect_first_human":
		return &c.ProtectFirstHuman
	case "protect_first_gpt":
		return &c.ProtectFirstGPT
	case "protect_first_tool":
		return &c.ProtectFirstTool
	case "protect_last_n_turns":
		return &c.ProtectLastNTurns
	case "summarization_model":
		return &c.SummarizationModel
	case "base_url":
		return &c.BaseURL
	case "api_key_env":
		return &c.APIKeyEnv
	case "temperature":
		return &c.Temperature
	case "max_retries":
		return &c.MaxRetries
	case "retry_delay":
		return &c.RetryDelay
	case "add_summary_notice":
		return &c.AddSummaryNotice
	case "summary_notice_text":
		return &c.SummaryNoticeText
	case "output_suffix":
		return &c.OutputSuffix
	case "num_workers":
		return &c.NumWorkers
	case "max_concurrent_requests":
		return &c.MaxConcurrentRequests
	case "skip_under_target":
		return &c.SkipUnderTarget
	case "save_over_limit":
		return &c.SaveOverLimit
	case "metrics_enabled":
		return &c.MetricsEnabled
	case "metrics_per_trajectory":
		return &c.MetricsPerTrajectory
	case "metrics_output_file":
		return &c.MetricsOutputFile
	}
	panic("unknown compression config attribute " + attr)
}

// Stores a YAML value into a typed field, flags taking Python truthiness and numbers their integer or float value
func assignYAML(dst any, attr string, v any) error {
	switch d := dst.(type) {
	case *bool:
		*d = pyrepr.Truthy(v)
		return nil
	case *string:
		if s, ok := v.(string); ok {
			*d = s
			return nil
		}
		return fmt.Errorf("TypeError: %s must be a string, got '%s'", attr, pyrepr.TypeName(v))
	case *int:
		if f, ok := yamlNumber(v); ok && f == math.Trunc(f) {
			*d = int(f)
			return nil
		}
		return fmt.Errorf("TypeError: %s must be an integer, got '%s'", attr, pyrepr.TypeName(v))
	case *float64:
		if f, ok := yamlNumber(v); ok {
			*d = f
			return nil
		}
		return fmt.Errorf("TypeError: %s must be a number, got '%s'", attr, pyrepr.TypeName(v))
	}
	panic("unsupported compression config field type")
}

// Numeric value of a YAML scalar, booleans counting as 0 and 1 the way Python int subclasses them
func yamlNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case bool:
		if n {
			return 1, true
		}
		return 0, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case string, nil:
		return 0, false
	}
	f, err := pyint.Float(v)
	return f, err == nil
}

// Loads a YAML config file, missing sections and keys keeping the defaults and a null base_url keeping its default
func ConfigFromYAML(path string) (*CompressionConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data, err := config.SafeLoad(raw)
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()
	if !pyrepr.Truthy(data) {
		return cfg, nil
	}
	top, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("TypeError: '%s' object is not subscriptable", pyrepr.TypeName(data))
	}
	for _, sec := range yamlSections {
		sectionData, present := top[sec.section]
		if !present {
			continue
		}
		section, ok := sectionData.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(sectionData))
		}
		for _, key := range sec.keys {
			yamlKey, attr, _ := strings.Cut(key, ":")
			if attr == "" {
				attr = yamlKey
			}
			value, given := section[yamlKey]
			if !given {
				continue
			}
			if attr == "base_url" && !pyrepr.Truthy(value) {
				continue
			}
			if err := assignYAML(cfg.field(attr), attr, value); err != nil {
				return nil, err
			}
		}
	}
	return cfg, nil
}

// TrajectoryMetrics are the counts of one trajectory compression
type TrajectoryMetrics struct {
	OriginalTokens          int
	CompressedTokens        int
	TokensSaved             int
	CompressionRatio        float64
	OriginalTurns           int
	CompressedTurns         int
	TurnsRemoved            int
	TurnsCompressedStartIdx int
	TurnsCompressedEndIdx   int
	TurnsInCompressedRegion int
	WasCompressed           bool
	StillOverLimit          bool
	SkippedUnderTarget      bool
	SummarizationAPICalls   int
	SummarizationErrors     int
}

// Metrics with the dataclass defaults
func NewTrajectoryMetrics() *TrajectoryMetrics {
	return &TrajectoryMetrics{CompressionRatio: 1.0, TurnsCompressedStartIdx: -1, TurnsCompressedEndIdx: -1}
}

// Ordered dict of the metrics with the compression region nested after turns_removed
func (m *TrajectoryMetrics) ToDict() *pyjson.Object {
	region := pyjson.NewObject()
	region.Set("start_idx", int64(m.TurnsCompressedStartIdx))
	region.Set("end_idx", int64(m.TurnsCompressedEndIdx))
	region.Set("turns_count", int64(m.TurnsInCompressedRegion))
	d := pyjson.NewObject()
	d.Set("original_tokens", int64(m.OriginalTokens))
	d.Set("compressed_tokens", int64(m.CompressedTokens))
	d.Set("tokens_saved", int64(m.TokensSaved))
	d.Set("compression_ratio", pyint.Round(m.CompressionRatio, 4))
	d.Set("original_turns", int64(m.OriginalTurns))
	d.Set("compressed_turns", int64(m.CompressedTurns))
	d.Set("turns_removed", int64(m.TurnsRemoved))
	d.Set("compression_region", region)
	d.Set("was_compressed", m.WasCompressed)
	d.Set("still_over_limit", m.StillOverLimit)
	d.Set("skipped_under_target", m.SkippedUnderTarget)
	d.Set("summarization_api_calls", int64(m.SummarizationAPICalls))
	d.Set("summarization_errors", int64(m.SummarizationErrors))
	return d
}

// Mean of values, the default when there are none
func mean[T int | float64](values []T, def any) any {
	if len(values) == 0 {
		return def
	}
	sum := 0.0
	for _, v := range values {
		sum += float64(v)
	}
	return sum / float64(len(values))
}

// round(x, n) of a mean, an integer default staying an integer as Python round keeps int
func roundAny(v any, n int) any {
	if f, ok := v.(float64); ok {
		return pyint.Round(f, n)
	}
	return v
}

// AggregateMetrics are the totals across every trajectory of a run
type AggregateMetrics struct {
	TotalTrajectories              int
	TrajectoriesCompressed         int
	TrajectoriesSkippedUnderTarget int
	TrajectoriesStillOverLimit     int
	TrajectoriesFailed             int
	TotalTokensBefore              int
	TotalTokensAfter               int
	TotalTokensSaved               int
	TotalTurnsBefore               int
	TotalTurnsAfter                int
	TotalTurnsRemoved              int
	TotalSummarizationCalls        int
	TotalSummarizationErrors       int
	CompressionRatios              []float64
	TokensSavedList                []int
	TurnsRemovedList               []int
	ProcessingStartTime            string
	ProcessingEndTime              string
	ProcessingDurationSeconds      float64
}

// Adds one trajectory's metrics to the totals
func (a *AggregateMetrics) AddTrajectoryMetrics(m *TrajectoryMetrics) {
	a.TotalTrajectories++
	a.TotalTokensBefore += m.OriginalTokens
	a.TotalTokensAfter += m.CompressedTokens
	a.TotalTokensSaved += m.TokensSaved
	a.TotalTurnsBefore += m.OriginalTurns
	a.TotalTurnsAfter += m.CompressedTurns
	a.TotalTurnsRemoved += m.TurnsRemoved
	a.TotalSummarizationCalls += m.SummarizationAPICalls
	a.TotalSummarizationErrors += m.SummarizationErrors
	if m.WasCompressed {
		a.TrajectoriesCompressed++
		a.CompressionRatios = append(a.CompressionRatios, m.CompressionRatio)
		a.TokensSavedList = append(a.TokensSavedList, m.TokensSaved)
		a.TurnsRemovedList = append(a.TurnsRemovedList, m.TurnsRemoved)
	}
	if m.SkippedUnderTarget {
		a.TrajectoriesSkippedUnderTarget++
	}
	if m.StillOverLimit {
		a.TrajectoriesStillOverLimit++
	}
}

// Ordered dict of summary, tokens, turns, averages, summarization and processing sections
func (a *AggregateMetrics) ToDict() *pyjson.Object {
	obj := func(kv ...any) *pyjson.Object {
		o := pyjson.NewObject()
		for i := 0; i < len(kv); i += 2 {
			o.Set(kv[i].(string), kv[i+1])
		}
		return o
	}
	ratio := func(n, d int) float64 { return pyint.Round(float64(n)/float64(max(d, 1)), 4) }
	return obj(
		"summary", obj("total_trajectories", int64(a.TotalTrajectories), "trajectories_compressed", int64(a.TrajectoriesCompressed),
			"trajectories_skipped_under_target", int64(a.TrajectoriesSkippedUnderTarget),
			"trajectories_still_over_limit", int64(a.TrajectoriesStillOverLimit), "trajectories_failed", int64(a.TrajectoriesFailed),
			"compression_rate", ratio(a.TrajectoriesCompressed, a.TotalTrajectories)),
		"tokens", obj("total_before", int64(a.TotalTokensBefore), "total_after", int64(a.TotalTokensAfter), "total_saved", int64(a.TotalTokensSaved),
			"overall_compression_ratio", ratio(a.TotalTokensAfter, a.TotalTokensBefore)),
		"turns", obj("total_before", int64(a.TotalTurnsBefore), "total_after", int64(a.TotalTurnsAfter), "total_removed", int64(a.TotalTurnsRemoved)),
		"averages", obj("avg_compression_ratio", roundAny(mean(a.CompressionRatios, 1.0), 4),
			"avg_tokens_saved_per_compressed", roundAny(mean(a.TokensSavedList, int64(0)), 1),
			"avg_turns_removed_per_compressed", roundAny(mean(a.TurnsRemovedList, int64(0)), 2)),
		"summarization", obj("total_api_calls", int64(a.TotalSummarizationCalls), "total_errors", int64(a.TotalSummarizationErrors),
			"success_rate", pyint.Round(1-float64(a.TotalSummarizationErrors)/float64(max(a.TotalSummarizationCalls, 1)), 4)),
		"processing", obj("start_time", a.ProcessingStartTime, "end_time", a.ProcessingEndTime,
			"duration_seconds", pyint.Round(a.ProcessingDurationSeconds, 2)),
	)
}
