package tuiserver

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/nickheyer/icebath/pkg/settings"
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

// Voice input and spoken reply flags of this process, loaded from the settings and kept current by their changes
var voiceFlags struct {
	once          sync.Once
	input, speaks atomic.Bool
}

func init() {
	settings.Subscribe(func(_ context.Context, c settings.Change) {
		if on, isBool := c.New.Value.(bool); isBool {
			storeVoiceFlag(c.Key, on)
		}
	})
}

// Loads both voice flags from the settings of the process home
func loadVoiceFlags() {
	ctx := context.Background()
	voiceFlags.input.Store(settings.VoiceInput.Get(ctx))
	voiceFlags.speaks.Store(settings.SpeakReplies.Get(ctx))
}

// Process flag of a voice setting, any other key ignored
func storeVoiceFlag(key string, on bool) {
	switch key {
	case settings.VoiceInput.Key:
		voiceFlags.input.Store(on)
	case settings.SpeakReplies.Key:
		voiceFlags.speaks.Store(on)
	}
}

// Stores a voice flag in the settings, the process flag following even when the write is refused
func setVoiceFlag(ctx context.Context, key string, on bool) {
	voiceFlags.once.Do(loadVoiceFlags)
	if _, err := settings.Set(context.WithoutCancel(ctx), key, on, settings.Global); err != nil {
		pkgLog().Warn("voice: keeping " + key + " for this run only: " + err.Error())
	}
	storeVoiceFlag(key, on)
}

// Whether voice input is on
func voiceModeEnabled() bool {
	voiceFlags.once.Do(loadVoiceFlags)
	return voiceFlags.input.Load()
}

// Whether replies are spoken
func voiceTTSEnabled() bool {
	voiceFlags.once.Do(loadVoiceFlags)
	return voiceFlags.speaks.Load()
}
