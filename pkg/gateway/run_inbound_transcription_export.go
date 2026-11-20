package gateway

// Transcript of one voice clip, nil when transcription failed, with the note that stands in for it
type STTClip struct {
	Transcript *string
	Note       string
}

// Per clip transcription results of the event, created on first use so merges keep earlier clips
func (e *MessageEvent) PendingSTTClips() map[string]STTClip {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	if e.pendingSTTClips == nil {
		e.pendingSTTClips = map[string]STTClip{}
	}
	return e.pendingSTTClips
}

// Count of pending transcripts already echoed to the chat
func (e *MessageEvent) PendingSTTEchoed() int {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	return e.pendingSTTEchoed
}

// Records how many pending transcripts were echoed
func (e *MessageEvent) SetPendingSTTEchoed(n int) {
	eventTurnStateMu.Lock()
	defer eventTurnStateMu.Unlock()
	e.pendingSTTEchoed = n
}
