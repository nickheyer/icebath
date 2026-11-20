package tuiserver

import (
	"context"
	"os"
	"sync"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var (
	voiceSidMu          sync.Mutex
	voiceEventSID       string
	voiceWakeOwner      tuihost.Transport
	voiceEventTransport tuihost.Transport
)

// Per turn text queue feeding the streaming TTS consumer, a nil item ending the text
type ttsTurnQueue struct{ items chan any }

// Runes of spoken text kept for the playback listener, enough to hold the stretch a capture can overlap
const spokenKeep = 3000

// Tail of what the reply in flight says aloud, which the playback listener compares a capture with to tell the speaker's own sound from a person
var spoken struct {
	mu   sync.Mutex
	text []rune
}

// Starts the record of a new spoken reply
func newTTSTurnQueue() *ttsTurnQueue {
	spoken.mu.Lock()
	spoken.text = nil
	spoken.mu.Unlock()
	return &ttsTurnQueue{items: make(chan any, 4096)}
}

// Adds text handed to speech to the record
func noteSpoken(text string) {
	spoken.mu.Lock()
	spoken.text = append(spoken.text, []rune(text)...)
	if extra := len(spoken.text) - spokenKeep; extra > 0 {
		spoken.text = append([]rune(nil), spoken.text[extra:]...)
	}
	spoken.mu.Unlock()
}

// Text of the record
func spokenText() string {
	spoken.mu.Lock()
	defer spoken.mu.Unlock()
	return string(spoken.text)
}

// Queues one reply delta
func (q *ttsTurnQueue) Put(item any) {
	if text, isText := item.(string); isText {
		noteSpoken(text)
	}
	q.items <- item
}

// Queues the end of text sentinel
func (q *ttsTurnQueue) Finish() { q.items <- nil }

// Emits toward the session that last turned voice on through the transport that started its capture
func voiceEmit(ctx context.Context, event string, payload map[string]any) {
	voiceSidMu.Lock()
	sid, transport := voiceEventSID, voiceEventTransport
	voiceSidMu.Unlock()
	if transport != nil {
		ctx = tuihost.BindTransport(ctx, transport)
	}
	tuihost.Emit(ctx, event, sid, payload)
}

// Whether the runtime only voice mode flag is on
func voiceModeEnabled() bool { return pystr.Strip(os.Getenv("ICEBATH_VOICE")) == "1" }

// Whether the runtime only spoken reply flag is on
func voiceTTSEnabled() bool { return pystr.Strip(os.Getenv("ICEBATH_VOICE_TTS")) == "1" }

// voice section of the config, empty for any non dict shape
func voiceCfgDict(ctx context.Context) map[string]any {
	voice, isDict := loadCfg(ctx)["voice"].(map[string]any)
	if !isDict {
		return map[string]any{}
	}
	return voice
}
