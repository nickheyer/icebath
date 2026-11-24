package gateway

import (
	"math"
	"strings"

	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Epoch seconds a message stamp names, a positive finite number or an ISO date, false for anything else
func undoEpoch(value any) (float64, bool) {
	number := math.NaN()
	switch v := value.(type) {
	case float64:
		number = v
	case int64:
		number = float64(v)
	case int:
		number = float64(v)
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return 0, false
		}
		parsed, err := fromISOFormat(text)
		if err != nil {
			return 0, false
		}
		return timeutil.Epoch(parsed.T), true
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		return 0, false
	}
	return number, true
}

// Start of an undone span, the undone user message's stamp or the newest kept message's when that is later, false when neither is known
func UndoSpanStart(target any, kept []map[string]any) (float64, bool) {
	start, known := undoEpoch(target)
	for _, message := range kept {
		if message == nil || state.IsCompactionSummaryMessage(message) {
			continue
		}
		if when, stamped := undoEpoch(message["timestamp"]); stamped && (!known || when > start) {
			start, known = when, true
		}
	}
	return start, known
}

// Wire hint of an undone span that started at a known time, nil when the span changed no file
func UndoHint(mgr *tools.CheckpointManager, cwd string, since float64, known bool) *tuigateway.UndoRollbackHint {
	if !known {
		return nil
	}
	files := mgr.ChangedFilesSince(cwd, since)
	if len(files) == 0 {
		return nil
	}
	return &tuigateway.UndoRollbackHint{Files: files}
}
