package tui

import "github.com/nickheyer/icebath/pkg/tuigateway"

// Attach answer of either route as the lifecycle applies it
type attachment struct {
	SessionID      string
	StoredSID      string
	SubscriptionID string
	Status         string
	Info           *tuigateway.SessionInfo
	Running        bool
	StartedAt      float64
	Messages       []tuigateway.TranscriptMessage
	Inflight       *tuigateway.InflightTurn
	Connection     *tuigateway.ConnectionRequestPayload
	Snapshot       *tuigateway.CanonicalSnapshot
	// Server requests the session still waits on, delivered again once the view shows it
	OpenRequests []tuigateway.OpenRequestEntry
}

// Attachment of a session authority snapshot, its rows the display projection the resume asked for
func snapshotAttachment(s *tuigateway.CanonicalSnapshot) attachment {
	info := s.Info
	stored, epoch, generation, running := s.StoredSessionID, s.AuthorityEpoch, s.ExecutionGeneration, s.Running
	info.StoredSessionID, info.AuthorityEpoch, info.ExecutionGeneration, info.Running = &stored, &epoch, &generation, &running
	info.Pending = s.Pending
	if info.Pending == nil {
		info.Pending = []tuigateway.CanonicalPendingAdmission{}
	}
	return attachment{SessionID: s.SessionID, StoredSID: stored, SubscriptionID: s.SubscriptionID, Info: &info, Running: s.Running,
		Messages: s.DisplayMessages, Connection: s.PendingConnection, Snapshot: s, OpenRequests: s.OpenRequests}
}

// First non empty text
func firstText(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
