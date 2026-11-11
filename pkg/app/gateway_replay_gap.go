package app

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Marker printed before a whole reply whose painted segment is not its prefix
const replayGapResumed = "[output resumed]"

// Status of the admission in the snapshot pending rows, nil when it ended in the hole
func replayGapPendingStatus(pending []any, admission any) any {
	for _, raw := range pending {
		row, _ := raw.(map[string]any)
		if pyrepr.Equal(row["admission_id"], admission) {
			return row["status"]
		}
	}
	return nil
}

// True when terminal with its reply, false while running, nil when the receipt is unreadable
func replayGapSettled(receipt map[string]any) *bool {
	yes, no := true, false
	if !pyrepr.Equal(receipt["status"], "terminal") {
		return &no
	}
	switch receipt["outcome"] {
	case "cancelled", "interrupted", "failed":
		return &yes
	}
	if _, ok := receipt["result"]; ok {
		return &yes
	}
	return nil
}

// Rest of the final reply after the painted exact prefix, else the resumed marker and the whole reply
func replayGapOwedReply(shown, final string) string {
	head := strings.TrimPrefix(shown, "\n\n")
	for _, prefix := range []string{head, pystr.Rstrip(head)} {
		if strings.HasPrefix(final, prefix) {
			return final[len(prefix):]
		}
	}
	return "\n" + replayGapResumed + "\n" + final
}
