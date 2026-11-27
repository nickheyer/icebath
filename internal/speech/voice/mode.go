// Voice mode on the machine the app runs on: recordings from its microphone turned into text by the server, replies the server speaks played
// on its speakers, talking over a reply to cut it off, and the wake word
package voice

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/speech"
	speechv1 "github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1"
	"github.com/nickheyer/icebath/pkg/gen/icebath/speech/v1/speechv1connect"
)

// Silent recordings in a row that end voice chat
const silentLimit = 3

// Runes of spoken text kept to tell the speaker's own sound from a person talking over it
const spokenKeep = 3000

// What voice mode tells the app, each called off the app's own goroutine
type Events struct {
	// listening, transcribing or idle
	Status func(state string)
	// Words heard so far of a recording a live transcription follows
	Partial func(text string)
	// What a recording said, to send or put in the prompt
	Transcript func(text string)
	// A stop phrase ended voice chat
	Stopped func(text string)
	// Recordings came back silent three times, which ended voice chat
	Silent func()
	// Talking over a reply that was still being written, which the app stops
	Interrupted func()
	// The wake phrase was heard: of the profile, and whether a new session starts
	Woke func(phrase, profile string, fresh bool)
	// Something to tell the person
	Notice func(text string)
}

// Voice mode of the app
type Mode struct {
	speech  *speech.Speech
	backend *backend
	events  Events
	player  *player

	mu       sync.Mutex
	settings *speechv1.VoiceReply
	// Continuous recording: running, stopping while the last capture transcribes, rearmed after each transcript
	active   bool
	stopping bool
	restart  bool
	rec      recorder
	silent   int
	gen      int
	// Agent busy probe: a silent recording during a reply doesn't count
	busy func() bool
	// Reply being written, its speech, and the listener for talking over them
	reply  *Turn
	turn   *speaker
	duplex bool
	spoken []rune
	// Speech that sounded was cut, which the next message is told
	interrupted bool
	wake        *listener
}

// Voice mode over the speech service, of the profile the app is on, and the speech host of this machine for what runs here; the
// recordings a capture that died left behind are removed
func New(sp *speech.Speech, client speechv1connect.SpeechServiceClient, profile func() string, events Events) *Mode {
	m := &Mode{speech: sp, backend: &backend{client: client, profile: profile}, events: events}
	m.player = &player{speech: sp, volume: m.beepVolume}
	go sweep(keepRecordings)
	return m
}

func (m *Mode) beepVolume() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return min(1, max(0, m.settings.GetMode().GetBeepVolume()))
}

func (m *Mode) mode() *speechv1.Mode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.GetMode()
}

// Settings of the profile the app is on, read again from the server
func (m *Mode) Refresh(ctx context.Context) (*speechv1.VoiceReply, error) {
	reply, err := m.backend.voice(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.settings = reply
	m.mu.Unlock()
	return reply, nil
}

// Settings last read
func (m *Mode) Settings() *speechv1.VoiceReply {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings
}

// Whether this machine can record, the server can transcribe and the environment has audio
func (m *Mode) Requirements() Requirements {
	r := m.Settings().GetRecognition()
	return Check(r.GetEnabled(), r.GetLabel(), r.GetProvider())
}

// Spoken hint naming the first stop phrase, empty when there are none
func (m *Mode) StopHint() string {
	return speech.StopHint(m.mode().GetStopPhrases())
}

// Whether the text alone is one of the stop phrases
func (m *Mode) StopPhrase(text string) bool {
	return speech.StopPhrase(m.mode().GetStopPhrases(), text)
}

// Probe of the agent working on a reply
func (m *Mode) Busy(probe func() bool) {
	m.mu.Lock()
	m.busy = probe
	m.mu.Unlock()
}

func (m *Mode) debug(text string) {
	if m.mode().GetDebug() {
		fmt.Fprintln(os.Stderr, "[voice] "+text)
	}
}

func (m *Mode) beep(frequency, count int) {
	if !m.mode().GetBeep() {
		return
	}
	m.player.beep(frequency, 0.12, count)
}

func call(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn(fmt.Sprintf("voice callback failed: %v", r))
		}
	}()
	fn()
}

func (m *Mode) status(state string) {
	if m.events.Status != nil {
		call(func() { m.events.Status(state) })
	}
}

// Opens the live transcription of a recording when the recognizer streams
func (m *Mode) live(ctx context.Context, rate int) *liveSession {
	if !m.Settings().GetRecognition().GetLive() {
		return nil
	}
	m.mu.Lock()
	gen := m.gen
	m.mu.Unlock()
	session, err := m.backend.listen(ctx, rate, func(text string) {
		m.mu.Lock()
		current := gen == m.gen
		m.mu.Unlock()
		if current && m.events.Partial != nil {
			call(func() { m.events.Partial(text) })
		}
	})
	if err != nil {
		slog.Debug(fmt.Sprintf("Live STT session did not open: %s", err))
		return nil
	}
	if session != nil {
		slog.Info(fmt.Sprintf("Live STT streaming (%s)", session.provider))
	}
	return session
}

// Whether a recording runs
func (m *Mode) Recording() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

// Whether a recording that stopped is still being transcribed
func (m *Mode) Transcribing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopping
}

// Starts recording until the speaker falls silent, the transcript sent to the app; rearmed after each one when restart is set. False while
// the last recording still transcribes
func (m *Mode) Record(ctx context.Context, restart bool) (bool, error) {
	m.mu.Lock()
	if m.active {
		m.mu.Unlock()
		m.debug("start_continuous: already active — no-op")
		return true, nil
	}
	if m.stopping {
		m.mu.Unlock()
		m.debug("start_continuous: stop/transcribe in progress — busy")
		return false, nil
	}
	m.active, m.restart = true, restart
	m.gen++
	if restart {
		m.silent = 0
	}
	if m.rec == nil {
		m.rec = newRecorder(m.live)
	}
	rec := m.rec
	mode := m.settings.GetMode()
	m.mu.Unlock()
	// A recording starts over speech still playing, and the listener for talking over the reply gives up the microphone
	m.drop()
	m.pauseWake()
	rec.tune(float64(mode.GetSilenceThreshold()), mode.GetSilenceDuration(), mode.GetMaxRecording())
	m.debug(fmt.Sprintf("start_continuous: begin (threshold=%d, duration=%gs)", mode.GetSilenceThreshold(), mode.GetSilenceDuration()))
	m.beep(880, 1)
	if err := rec.start(ctx, func() { m.silenced(ctx) }); err != nil {
		slog.Error(fmt.Sprintf("failed to start continuous recording: %s", err))
		m.debug(fmt.Sprintf("start_continuous: rec.start raised %s", err))
		m.mu.Lock()
		m.active = false
		m.mu.Unlock()
		m.resumeWake()
		return false, err
	}
	m.status("listening")
	return true, nil
}

// Ends recording, the recording made so far transcribed when asked
func (m *Mode) Stop(ctx context.Context, transcribe bool) {
	m.mu.Lock()
	if !m.active {
		m.mu.Unlock()
		return
	}
	m.active = false
	rec := m.rec
	track := transcribe && !m.restart
	m.stopping = rec != nil
	if !track {
		m.silent = 0
	}
	m.mu.Unlock()
	if rec == nil {
		m.stopped()
		return
	}
	if transcribe {
		m.status("transcribing")
		got, err := rec.stop()
		if err != nil {
			slog.Warn(fmt.Sprintf("failed to stop recorder: %s", err))
			rec.cancel()
			got = capture{}
		}
		// The microphone is free again while the recording transcribes
		m.resumeWake()
		go func() {
			text, stop, said := m.heard(ctx, got, heardStop)
			if stop {
				m.halt(true, said)
			}
			if text != "" && m.events.Transcript != nil {
				call(func() { m.events.Transcript(text) })
			}
			if track {
				held := m.held()
				m.mu.Lock()
				limit, _ := m.tally(text != "" || stop, held, "stop_continuous")
				m.mu.Unlock()
				if limit {
					m.halt(false, "")
				}
			}
			m.stopped()
		}()
		return
	}
	rec.cancel()
	m.stopped()
}

// Clears the stopping mark, plays the stopped cue and hands the microphone back to the wake word
func (m *Mode) stopped() {
	m.mu.Lock()
	m.stopping = false
	m.mu.Unlock()
	m.beep(660, 2)
	m.status("idle")
	m.resumeWake()
}

// Whether a silent recording doesn't count: speech playing or the agent working
func (m *Mode) held() (held bool) {
	if m.speaking() {
		return true
	}
	m.mu.Lock()
	probe := m.busy
	m.mu.Unlock()
	defer func() {
		if recover() != nil {
			held = false
		}
	}()
	return probe != nil && probe()
}

// Counts a silent recording, the lock held: whether it reached the limit
func (m *Mode) tally(spoke, held bool, where string) (bool, int) {
	if spoke {
		m.silent = 0
		return false, 0
	}
	if held {
		m.debug(where + ": silent cycle ignored (agent busy or TTS playing)")
		return false, m.silent
	}
	m.silent++
	if count := m.silent; count >= silentLimit {
		m.silent = 0
		return true, count
	}
	return false, m.silent
}

// Tells the app voice chat ended: by a stop phrase, else by silence
func (m *Mode) halt(stop bool, said string) {
	switch {
	case stop && m.events.Stopped != nil:
		call(func() { m.events.Stopped(said) })
	case m.events.Silent != nil:
		call(m.events.Silent)
	}
}

// Where a recording ended, how its transcription failing is logged, what a stop phrase does and whether the transcript is traced
type ending struct {
	where, failed, halts string
	trace                bool
}

var (
	heardStop    = ending{where: "stop_continuous", failed: "failed to stop/transcribe recorder: %s", halts: "ending voice chat"}
	heardSilence = ending{where: "_continuous_on_silence", failed: "continuous transcription failed: %s", halts: "ending loop", trace: true}
)

// Transcript of a finished recording, whether it was only a stop phrase, and that phrase; the file removed. A live transcription that heard
// nothing gives way to transcribing the file
func (m *Mode) heard(ctx context.Context, got capture, e ending) (string, bool, string) {
	if got.path == "" {
		if got.live != nil {
			got.live.drop()
		}
		return "", false, ""
	}
	defer os.Remove(got.path)
	var t speech.Transcript
	if got.live != nil {
		if t = got.live.finish(speech.FinishTimeout); !t.Success || strings.TrimSpace(t.Text) == "" && !t.Filtered {
			reason := "empty"
			if t.Error != "" {
				reason = t.Error
			}
			slog.Info(fmt.Sprintf("Live STT gave no transcript (%s); transcribing the recording", reason))
			got.live = nil
		}
	}
	if got.live == nil {
		var err error
		if t, err = m.backend.transcribe(ctx, got.path); err != nil {
			slog.Warn(fmt.Sprintf(e.failed, err))
			if e.trace {
				m.debug(fmt.Sprintf("%s: transcribe raised %s", e.where, err))
			}
			return "", false, ""
		}
	}
	text := strings.TrimSpace(t.Text)
	if e.trace {
		m.debug(fmt.Sprintf("%s: transcribe -> success=%t text=%q err=%q", e.where, t.Success, text, t.Error))
	}
	if !t.Success || text == "" || speech.Hallucination(text) && !m.StopPhrase(text) {
		return "", false, ""
	}
	if m.StopPhrase(text) {
		m.debug(fmt.Sprintf("%s: stop phrase %q — %s", e.where, text, e.halts))
		return "", true, text
	}
	return text, false, ""
}

// The recorder fell silent: the recording transcribed and sent, then recording rearmed or ended
func (m *Mode) silenced(ctx context.Context) {
	m.debug("_continuous_on_silence: fired")
	m.mu.Lock()
	if !m.active {
		m.mu.Unlock()
		m.debug("_continuous_on_silence: loop inactive — abort")
		return
	}
	rec := m.rec
	m.mu.Unlock()
	if rec == nil {
		m.debug("_continuous_on_silence: no recorder — abort")
		return
	}
	m.status("transcribing")
	got, err := rec.stop()
	if err != nil {
		slog.Warn(fmt.Sprintf("continuous silence callback failed: %s", err))
		return
	}
	m.debug(fmt.Sprintf("_continuous_on_silence: rec.stop -> %q (peak_rms=%d)", got.path, got.peak))
	m.beep(660, 2)
	text, stop, said := m.heard(ctx, got, heardSilence)
	held := text == "" && !stop && m.held()
	m.mu.Lock()
	if !m.active {
		m.mu.Unlock()
		m.debug("_continuous_on_silence: stopped during transcribe — no restart")
		return
	}
	limit, silent := m.tally(text != "" || stop, held, "_continuous_on_silence")
	m.mu.Unlock()
	if text != "" && m.events.Transcript != nil {
		call(func() { m.events.Transcript(text) })
	}
	if stop || limit {
		reason := fmt.Sprintf("%d silent cycles", silent)
		if stop {
			reason = "stop phrase"
		}
		m.debug("_continuous_on_silence: halting (" + reason + ")")
		m.mu.Lock()
		m.active, m.silent = false, 0
		m.mu.Unlock()
		m.halt(stop, said)
		rec.cancel()
		m.status("idle")
		m.resumeWake()
		return
	}
	m.rearm(ctx, rec, silent)
}

// Waits out speech that plays, then records again, or ends a one shot recording
func (m *Mode) rearm(ctx context.Context, rec recorder, silent int) {
	if m.speaking() {
		m.debug("_continuous_on_silence: waiting for TTS to finish")
		deadline := time.Now().Add(time.Minute)
		for m.speaking() && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(300 * time.Millisecond)
		if !m.Recording() {
			m.debug("_continuous_on_silence: stopped while waiting for TTS")
			return
		}
	}
	m.mu.Lock()
	restart := m.restart
	if !restart {
		m.active = false
	}
	m.mu.Unlock()
	if !restart {
		m.debug("_continuous_on_silence: auto_restart=False, stopping loop")
		m.status("idle")
		m.resumeWake()
		return
	}
	m.debug(fmt.Sprintf("_continuous_on_silence: restarting loop (no_speech=%d)", silent))
	m.beep(880, 1)
	if err := rec.start(ctx, func() { m.silenced(ctx) }); err != nil {
		slog.Error(fmt.Sprintf("failed to restart continuous recording: %s", err))
		m.debug(fmt.Sprintf("_continuous_on_silence: restart raised %s", err))
		m.mu.Lock()
		m.active = false
		m.mu.Unlock()
		m.status("idle")
		m.resumeWake()
		return
	}
	m.status("listening")
}

// Adds text handed to speech to what the reply said aloud
func (m *Mode) noteSpoken(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spoken = append(m.spoken, []rune(text+" ")...)
	if extra := len(m.spoken) - spokenKeep; extra > 0 {
		m.spoken = append([]rune(nil), m.spoken[extra:]...)
	}
}

// Whether speech is still to say or sounding
func (m *Mode) speaking() bool {
	m.mu.Lock()
	turn := m.turn
	m.mu.Unlock()
	return turn != nil && turn.pending() || m.player.sounding()
}

// Speech of one reply as it is written, which the app feeds and ends
type Turn struct {
	mode    *Mode
	speaker *speaker
	mu      sync.Mutex
	ended   bool
}

// Opens the speech of the reply now being written: its text read aloud as it arrives when replies are spoken, the thinking blips meanwhile,
// and talking over it heard; nil when voice mode is off
func (m *Mode) Reply(ctx context.Context) *Turn {
	settings := m.Settings()
	mode := settings.GetMode()
	if !mode.GetInput() {
		return nil
	}
	m.Cut()
	turn := &Turn{mode: m}
	if speaking := settings.GetSpeaking(); mode.GetSpeak() && speaking.GetReady() {
		turn.speaker = newSpeaker(m, context.WithoutCancel(ctx), int(speaking.GetLeast()), speaking.GetStreams())
		m.mu.Lock()
		m.spoken, m.turn = nil, turn.speaker
		m.mu.Unlock()
	}
	m.mu.Lock()
	m.reply = turn
	m.mu.Unlock()
	if mode.GetThinkingSound() {
		m.player.think(func() bool { return !m.player.sounding() && !m.Recording() })
	}
	if mode.GetBargeIn() {
		m.listen(ctx)
	}
	return turn
}

// Adds reply text
func (t *Turn) Delta(text string) {
	if t == nil || t.speaker == nil || text == "" {
		return
	}
	t.speaker.add(text)
}

// The reply text ended: the blips stop, what is queued is still said
func (t *Turn) Finish() {
	if t == nil {
		return
	}
	t.mu.Lock()
	ended := t.ended
	t.ended = true
	t.mu.Unlock()
	if ended {
		return
	}
	t.mode.player.quiet()
	if t.speaker != nil {
		t.speaker.finish()
	}
}

// Whether the reply is still written or said
func (t *Turn) live() bool {
	t.mu.Lock()
	ended := t.ended
	t.mu.Unlock()
	return !ended || t.speaker != nil && t.speaker.pending()
}

// Cuts the speech of the reply: what is queued dropped and what sounds silenced, speech that sounded or waited to latched for the next
// message. The thinking blips go on until the reply ends
func (m *Mode) Cut() {
	m.mu.Lock()
	turn := m.turn
	m.turn = nil
	m.mu.Unlock()
	live := m.player.sounding()
	if turn != nil && turn.pending() {
		live = live || turn.audible()
		turn.halt()
	}
	if !live {
		return
	}
	m.player.Stop()
	m.mu.Lock()
	m.interrupted = true
	m.mu.Unlock()
}

// Silences the speech of the reply with the next message not told of it, for the person turning spoken replies or voice mode off
func (m *Mode) Hush() {
	m.mu.Lock()
	turn := m.turn
	m.turn = nil
	m.mu.Unlock()
	if turn != nil && turn.pending() {
		turn.halt()
	}
	m.player.Stop()
}

// Cuts the speech and lets go of the reply, so nothing listens for talking over it any more
func (m *Mode) drop() {
	m.mu.Lock()
	m.reply = nil
	m.mu.Unlock()
	m.Cut()
}

// Whether the next message cuts a spoken reply off or follows one that was, the latch cleared; speech still playing cut first
func (m *Mode) TakeInterrupted() bool {
	if m.speaking() {
		m.Cut()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	interrupted := m.interrupted
	m.interrupted = false
	return interrupted
}

// Recording stopped, speech cut, listening for talking over the reply ended, the blips silenced
func (m *Mode) Off(ctx context.Context) {
	m.Stop(ctx, false)
	m.drop()
	m.player.quiet()
}

// Frees the microphone and speakers
func (m *Mode) Close() {
	m.Off(context.Background())
	m.mu.Lock()
	rec := m.rec
	m.rec = nil
	m.mu.Unlock()
	if rec != nil {
		rec.shutdown()
	}
	m.StopWake()
}
