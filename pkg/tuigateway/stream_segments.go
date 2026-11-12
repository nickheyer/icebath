package tuigateway

import (
	"sync"
	"unicode/utf16"
)

// Session record key holding the segment cursor of the running prompt turn
const SessionSegmentsKey = "_turn_segments"

// Length of a text in UTF-16 code units, the unit a JavaScript string indexes in
func utf16Len(text string) int {
	return len(utf16.Encode([]rune(text)))
}

// Segment cursor of one turn, created at message start and advanced by every delta and seal
type TurnSegments struct {
	mu      sync.Mutex
	TurnID  string
	Segment int
	Offset  int
}

func NewTurnSegments() *TurnSegments { return &TurnSegments{TurnID: uuidHex()} }

// Identity fields of the turn start frame
func (s *TurnSegments) Start() map[string]any { return map[string]any{"turn_id": s.TurnID} }

// Identity fields of one delta, the offset then advanced past a text
func (s *TurnSegments) Delta(text any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := map[string]any{"turn_id": s.TurnID, "segment": s.Segment, "offset": s.Offset}
	if str, ok := text.(string); ok {
		s.Offset += utf16Len(str)
	}
	return stamp
}

// Identity fields of the interim that seals the current segment, the cursor then moved to the next one
func (s *TurnSegments) Seal() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp := map[string]any{"turn_id": s.TurnID, "segment": s.Segment}
	s.Segment++
	s.Offset = 0
	return stamp
}

// Copy of payload carrying the stamp fields over its own
func stampedPayload(payload, stamp map[string]any) map[string]any {
	out := make(map[string]any, len(payload)+len(stamp))
	for k, v := range payload {
		out[k] = v
	}
	for k, v := range stamp {
		out[k] = v
	}
	return out
}

// Payload with the turn identity when the event is a reply frame of a running turn, unchanged otherwise
func StampReplyFrame(segments *TurnSegments, event string, payload map[string]any) map[string]any {
	if segments == nil {
		return payload
	}
	switch {
	case event == "message.start":
		return stampedPayload(payload, segments.Start())
	case event == "message.interim" && payload != nil:
		return stampedPayload(payload, segments.Seal())
	case event == "message.delta" && payload != nil:
		if text, ok := payload["text"].(string); ok {
			return stampedPayload(payload, segments.Delta(text))
		}
	}
	return payload
}

// Reply frame stamping with the cursor of the prompt turn the session record is running, if any
func StampSessionFrame(session *Session, event string, payload map[string]any) map[string]any {
	segments, _ := session.Get(SessionSegmentsKey).(*TurnSegments)
	return StampReplyFrame(segments, event, payload)
}
