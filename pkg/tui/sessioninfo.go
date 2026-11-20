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

// One prompt the owner holds for a session, an admission of its queue or a submission a forwarding owner queued
type pendingRow struct {
	AdmissionID         string
	InputID             string
	Status              string
	User                string
	ExecutionGeneration *int64
	TargetProfileHome   string
	TargetSessionID     string
	DisplayKind         *string
	DisplayMetadata     *tuigateway.DisplayMetadata
}

// Pending rows of a session, the owner's admissions then the forwarded submissions, nil when the info lists none
func pendingRowsOf(info *tuigateway.SessionInfo) []pendingRow {
	if info == nil || info.Pending == nil && info.PendingSubmissions == nil {
		return nil
	}
	rows := []pendingRow{}
	for _, a := range info.Pending {
		rows = append(rows, pendingRow{AdmissionID: a.AdmissionID, InputID: a.InputID, Status: string(a.Status), User: a.Text,
			ExecutionGeneration: a.ExecutionGeneration, TargetProfileHome: a.Ref.ProfileID, TargetSessionID: a.Ref.SessionID,
			DisplayKind: a.DisplayKind, DisplayMetadata: a.DisplayMetadata})
	}
	for _, s := range info.PendingSubmissions {
		rows = append(rows, pendingRow{AdmissionID: s.AdmissionID, InputID: s.InputID, Status: s.Status, User: s.User})
	}
	return rows
}

// Whether a pending row still waits on the owner's queue
func (r pendingRow) serverQueued() bool { return r.Status == "queued" || r.Status == "unknown" }

// Id the client gave a pending row, the admission id when it named none
func (r pendingRow) submissionID() string { return firstText(r.InputID, r.AdmissionID) }
