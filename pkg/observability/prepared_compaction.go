package observability

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	PreparedCompactionMark   = "icebath.prepared_compaction.count"
	PreparedCompactionMetric = PreparedCompactionMark
)

var (
	PreparedCompactionEvents = Set("auto_disabled", "compaction_failed", "discarded", "internal_error",
		"pass_cancelled", "pass_completed", "pass_failed", "pass_started", "skipped_busy", "used")
	PreparedCompactionOrigins = Set("none", "tool_batch", "turn_end")
	PreparedCompactionReasons = Set("agent_released", "aux_fallback_main", "compaction_aborted", "fresh_required",
		"head_moved", "host_cancelled", "none", "other_session", "pass_failed_while_waiting", "provider_context",
		"rewound", "running_not_fit", "session_boundary", "shape_changed", "superseded", "text_changed",
		"transcript_shorter", "wait_timeout")
	PreparedCompactionWaitBuckets = Set("none", "lt_1s", "1s_to_5s", "5s_to_30s", "30s_to_120s", "gte_120s")
	PreparedCompactionDimensions  = dims{
		"event": PreparedCompactionEvents, "origin": PreparedCompactionOrigins,
		"reason": PreparedCompactionReasons, "wait_bucket": PreparedCompactionWaitBuckets,
	}
	PreparedCompactionSnapshotDimensions = dims{"compression_prepare_ahead": Set("off", "on")}
)

var (
	preparedWaitThresholds = []threshold{{1, "lt_1s"}, {5, "1s_to_5s"}, {30, "5s_to_30s"}, {120, "30s_to_120s"}}
	truthyStrings          = Set("1", "true", "yes", "on")
)

// Reads a config value the way the agent reads boolean switches, None giving the default
func isTruthyValue(value any, def bool) bool {
	if value == nil {
		return def
	}
	if s, ok := value.(string); ok {
		return truthyStrings.Has(pystr.Lower(pystr.Strip(s)))
	}
	return pyTruthy(value)
}

// Reports on when the profile config turns compression prepare_ahead on
func PrepareAheadState(cfg any) string {
	var value any
	if compression, ok := asDict(installSub(cfg, "compression")); ok {
		value = compression["prepare_ahead"]
	}
	if isTruthyValue(value, false) {
		return "on"
	}
	return "off"
}

// Bucket of how long a turn waited on a prepared pass, none for a missing or negative wait
func PreparedCompactionWaitBucket(seconds any) string {
	value, ok := pyNumber(seconds)
	if !ok || value < 0 {
		return "none"
	}
	return bucket(value, preparedWaitThresholds, "gte_120s")
}

func closedValue(value any, allowed ValueSet, fallback string) string {
	if text := strings.ReplaceAll(norm(value), "-", "_"); allowed.Has(text) {
		return text
	}
	return fallback
}

// Closed dimensions of one prepared compaction event, nil for an event outside the set
func PreparedCompactionFields(event, origin, reason, failureClass, waitSeconds any) map[string]string {
	name := closedValue(event, PreparedCompactionEvents, "")
	if name == "" {
		return nil
	}
	failure := "none"
	if failureClass != nil {
		failure = CompressionFailureClass("failed", failureClass)
	}
	return map[string]string{
		"event": name, "failure_class": failure,
		"origin":      closedValue(origin, PreparedCompactionOrigins, "none"),
		"reason":      closedValue(reason, PreparedCompactionReasons, "none"),
		"wait_bucket": PreparedCompactionWaitBucket(waitSeconds),
	}
}

func noneIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// Counts one prepared compaction event while shared metrics are on, empty strings and nil reading as None
func RecordPreparedCompaction(ctx context.Context, event, origin, reason, failureClass string, waitSeconds *float64) {
	var wait any
	if waitSeconds != nil {
		wait = *waitSeconds
	}
	emit(ctx, PreparedCompactionMark, func() map[string]string {
		return PreparedCompactionFields(event, noneIfEmpty(origin), noneIfEmpty(reason), noneIfEmpty(failureClass), wait)
	})
}
