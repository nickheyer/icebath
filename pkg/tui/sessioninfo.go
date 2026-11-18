package tui

import (
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Session info or an empty one, so every field reads as unset without a session
func infoOf(info *tuigateway.SessionInfo) *tuigateway.SessionInfo {
	if info == nil {
		return &tuigateway.SessionInfo{}
	}
	return info
}

// Trimmed text of an optional info field, empty when unset
func infoText(p *string) string { return trimJS(val(p)) }

// Copy of a session info with a patch laid over it
func patchedInfo(info *tuigateway.SessionInfo, patch tuigateway.SessionInfo) *tuigateway.SessionInfo {
	next := *infoOf(info)
	tuigateway.Merge(&next, patch)
	return &next
}

// Whether two optional stamps agree, both absent or both the same number
func sameStamp(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Pending rows of a session as the queue reads them, the owner's admissions or the forwarded submissions, nil when the info lists none
func pendingRowsOf(info *tuigateway.SessionInfo) []map[string]any {
	if info == nil || info.Pending == nil && info.PendingSubmissions == nil {
		return nil
	}
	rows := []map[string]any{}
	for _, admission := range info.Pending {
		row := wireParams(admission)
		row["user"], row["target_profile_home"], row["target_session_id"] = admission.Text, admission.Ref.ProfileID, admission.Ref.SessionID
		rows = append(rows, row)
	}
	for _, submission := range info.PendingSubmissions {
		rows = append(rows, wireParams(submission))
	}
	return rows
}
