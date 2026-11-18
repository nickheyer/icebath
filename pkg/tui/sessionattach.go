package tui

import (
	"strings"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

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
}

// Attachment of a session authority snapshot, stored rows given their display text when no display projection came
func snapshotAttachment(s *tuigateway.CanonicalSnapshot) attachment {
	info := s.Info
	stored, epoch, generation, running := s.StoredSessionID, s.AuthorityEpoch, s.ExecutionGeneration, s.Running
	info.StoredSessionID, info.AuthorityEpoch, info.ExecutionGeneration, info.Running = &stored, &epoch, &generation, &running
	info.Pending = s.Pending
	if info.Pending == nil {
		info.Pending = []tuigateway.CanonicalPendingAdmission{}
	}
	messages := s.DisplayMessages
	if messages == nil {
		messages = make([]tuigateway.TranscriptMessage, len(s.Messages))
		for i, row := range s.Messages {
			row.Text, row.Name = storedRowText(row.Content), row.ToolName
			messages[i] = row
		}
	}
	return attachment{SessionID: s.SessionID, StoredSID: stored, SubscriptionID: s.SubscriptionID, Info: &info, Running: s.Running,
		Messages: messages, Connection: s.PendingConnection, Snapshot: s}
}

// Attachment of a resume or activation on the method table route under its stored id
func resumedAttachment(r *tuigateway.SessionResumeResult, stored string) attachment {
	info := r.Info
	info.StoredSessionID = &stored
	status := val(r.Status)
	return attachment{SessionID: r.SessionID, StoredSID: stored, Info: &info, Running: val(r.Running) || status == "working" || status == "waiting",
		Status: status, StartedAt: val(r.StartedAt), Messages: r.Messages, Inflight: r.Inflight, Connection: r.PendingConnection}
}

// Display text of stored message content, a string as it is, text parts joined and media parts named
func storedRowText(content any) *string {
	switch c := content.(type) {
	case string:
		return &c
	case []any:
		var out []string
		for _, part := range c {
			var text string
			switch p := part.(type) {
			case string:
				text = p
			case map[string]any:
				if s, isString := p["text"].(string); isString {
					text = s
				} else if kind, _ := p["type"].(string); kind != "" {
					text = "[" + kind + "]"
					if strings.Contains(kind, "image") {
						text = "[image]"
					}
				}
			}
			if text != "" {
				out = append(out, text)
			}
		}
		joined := strings.Join(out, "\n")
		return &joined
	}
	return nil
}

// Transcript rows of typed history as the transcript builder reads them
func historyRows(rows []tuigateway.TranscriptMessage) []any {
	out := make([]any, len(rows))
	for i, row := range rows {
		out[i] = wireParams(row)
	}
	return out
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
