package tuiserver

import (
	"context"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var (
	voiceSidMu          sync.Mutex
	voiceEventSID       string
	voiceWakeOwner      tuigateway.Transport
	voiceEventTransport tuigateway.Transport
	voiceCaptureSID     string
	voiceCaptureGen     int

	wakeResumeRetryMu     sync.Mutex
	wakeResumeRetryActive bool

	ttsStreamMu    sync.Mutex
	ttsStreamState *ttsStream

	fdListenerMu     sync.Mutex
	fdListenerActive bool
	fdSpeakPipelines = map[*speakPipeline]bool{}
)

// Stop and done events of one fallback whole reply speak
type speakPipeline struct{ stop, done *pythread.Event }

// Stop and done events of the live streaming TTS consumer
type ttsStream struct{ stop, done *pythread.Event }

func init() {
	gateway.RegisterSessionVoiceCaptureStop(func(sessionID string) { stopSessionVoiceCapture(context.Background(), sessionID) })
}

// Per turn text queue feeding the streaming TTS consumer, a nil item ending the text
type ttsTurnQueue struct{ items chan any }

// Queues one reply delta
func (q *ttsTurnQueue) Put(item any) { q.items <- item }

// Queues the end of text sentinel
func (q *ttsTurnQueue) Finish() { q.items <- nil }

// Next queued item, false when none arrived within the timeout
func (q *ttsTurnQueue) Get(timeout time.Duration) (any, bool) {
	select {
	case item := <-q.items:
		return item, true
	case <-time.After(timeout):
		return nil, false
	}
}

// Threading event over pythread for the speaker pipeline
type ttsEvent struct{ *pythread.Event }

func (e ttsEvent) Wait(timeout time.Duration) bool { return e.Event.Wait(timeout.Seconds()) }

// Emits toward the session that last turned voice on through the transport that started its capture
func voiceEmit(ctx context.Context, event string, payload map[string]any) {
	voiceSidMu.Lock()
	sid, transport := voiceEventSID, voiceEventTransport
	voiceSidMu.Unlock()
	if transport != nil {
		ctx = tuigateway.BindTransport(ctx, transport)
	}
	tuigateway.Emit(ctx, event, sid, payload)
}

// Resumes the wake detector for the transport that yielded it to a voice capture
func resumeVoiceWake() {
	voiceSidMu.Lock()
	owner := voiceWakeOwner
	voiceWakeOwner = nil
	voiceSidMu.Unlock()
	if owner != nil {
		wakeResumeIfOwner(owner, 15*time.Second, time.Second)
	}
}

// Resumes the wake detector for its owner, retrying in the background while the microphone is still busy
func wakeResumeIfOwner(owner tuigateway.Transport, retry, interval time.Duration) bool {
	resumed, err := media.ResumeListening(owner)
	if err == nil {
		return resumed
	}
	voiceLogger().Debug(fmt.Sprintf("wake resume failed (will retry): %s", err))
	wakeResumeRetryMu.Lock()
	if wakeResumeRetryActive {
		wakeResumeRetryMu.Unlock()
		return false
	}
	wakeResumeRetryActive = true
	wakeResumeRetryMu.Unlock()
	go func() {
		defer func() {
			wakeResumeRetryMu.Lock()
			wakeResumeRetryActive = false
			wakeResumeRetryMu.Unlock()
		}()
		deadline := time.Now().Add(retry)
		for time.Now().Before(deadline) {
			time.Sleep(interval)
			ok, err := media.ResumeListening(owner)
			if err != nil {
				continue
			}
			if ok {
				voiceLogger().Info("wake: detector resumed after retry")
			}
			return
		}
		voiceLogger().Warn("wake: could not resume detector after voice turn (microphone still busy?) — toggle the wake word to re-arm")
	}()
	return false
}

// Whether the runtime only voice mode flag is on
func voiceModeEnabled() bool { return pystr.Strip(os.Getenv("ICEBATH_VOICE")) == "1" }

// Whether the runtime only spoken reply flag is on
func voiceTTSEnabled() bool { return pystr.Strip(os.Getenv("ICEBATH_VOICE_TTS")) == "1" }

// Whether a continuous voice capture is live
func voiceContinuousActive() bool { return media.IsContinuousActive() }

// Turns a shared owner session's own voice mode off, false when no owner here holds it
func setSessionVoiceOff(sessionID string) bool {
	held, err := gateway.SetLocalVoiceMode(sessionID, "off")
	return err == nil && held != nil && *held
}

// Ends a session's live capture and invalidates its late callbacks, leaving other sessions alone
func stopSessionVoiceCapture(ctx context.Context, sessionID string) {
	voiceSidMu.Lock()
	if sessionID == "" || sessionID != voiceCaptureSID {
		voiceSidMu.Unlock()
		return
	}
	voiceCaptureGen++
	voiceSidMu.Unlock()
	suppress(func() { media.StopContinuous(ctx, false) })
	resumeVoiceWake()
}

// Flips voice and spoken replies off, optionally halting the loop and cutting live TTS
func endVoiceChat(ctx context.Context, stopLoop, stopTTS bool) {
	_ = os.Setenv("ICEBATH_VOICE", "0")
	_ = os.Setenv("ICEBATH_VOICE_TTS", "0")
	if stopLoop {
		suppress(func() { media.StopContinuous(ctx, false) })
	}
	if stopTTS {
		suppress(func() { ttsStreamStop(false) })
	}
}

// Ends a session's voice chat through its own mode when an owner holds it, else through the env flags
func endSessionVoiceChat(ctx context.Context, sessionID string, stopLoop, stopTTS bool) {
	if !setSessionVoiceOff(sessionID) {
		endVoiceChat(ctx, stopLoop, stopTTS)
	} else if stopTTS {
		suppress(func() { ttsStreamStop(false) })
	}
}

// Live sessions with a turn running
func runningSessions() []*tuigateway.Session {
	var out []*tuigateway.Session
	for _, live := range tuigateway.LiveSessions() {
		if truthy(live.Session.Get("running")) {
			out = append(out, live.Session)
		}
	}
	return out
}

// Whether any turn runs, false when the probe fails
func anySessionRunning() (running bool) {
	defer func() {
		if recover() != nil {
			running = false
		}
	}()
	return len(runningSessions()) > 0
}

// Starts the per turn streaming TTS consumer, nil when spoken replies are off or cannot stream
func ttsStreamBegin(ctx context.Context) *ttsTurnQueue {
	if !voiceTTSEnabled() {
		return nil
	}
	ready := false
	suppress(func() { ready = media.CheckTTSRequirements(ctx) })
	if !ready {
		return nil
	}
	ttsStreamStop(true)
	queue := &ttsTurnQueue{items: make(chan any, 4096)}
	stop, done := &pythread.Event{}, &pythread.Event{}
	go media.StreamTTSToSpeaker(context.WithoutCancel(ctx), queue, ttsEvent{stop}, ttsEvent{done}, nil, "")
	ttsStreamMu.Lock()
	ttsStreamState = &ttsStream{stop: stop, done: done}
	ttsStreamMu.Unlock()
	armBargeListenerIfEnabled(ctx)
	return queue
}

// Cuts in flight streaming TTS, a user barge latching the interruption for the next turn
func ttsStreamStop(userBarge bool) {
	ttsStreamMu.Lock()
	state := ttsStreamState
	ttsStreamState = nil
	ttsStreamMu.Unlock()
	if state == nil {
		return
	}
	if userBarge && !state.done.IsSet() {
		voiceLogger().Debug("TTS CUT: _tts_stream_stop(user_barge=True) — new turn or interrupt cutting in-flight TTS")
		media.MarkSpeechInterrupted()
	}
	state.stop.Set()
	suppress(media.StopPlayback)
}

// Arms the process global full duplex listener once, one microphone
func armFullDuplexListener(ctx context.Context) {
	fdListenerMu.Lock()
	if fdListenerActive {
		fdListenerMu.Unlock()
		return
	}
	fdListenerActive = true
	fdListenerMu.Unlock()
	go fullDuplexListener(context.WithoutCancel(ctx))
}

// Arms the listener when voice mode is on and barge in is not disabled
func armBargeListenerIfEnabled(ctx context.Context) {
	bargeIn, present := voiceCfgDict(ctx)["barge_in"]
	if voiceModeEnabled() && (!present || truthy(bargeIn)) {
		armFullDuplexListener(ctx)
	}
}

// Whether any streaming or fallback speak pipeline is unfinished
func fdTTSPending() bool {
	ttsStreamMu.Lock()
	state := ttsStreamState
	ttsStreamMu.Unlock()
	fdListenerMu.Lock()
	defer fdListenerMu.Unlock()
	if state != nil && !state.done.IsSet() {
		return true
	}
	for pipeline := range fdSpeakPipelines {
		if !pipeline.done.IsSet() {
			return true
		}
	}
	return false
}

// Listens from utterance submit to turn end, a trip transcribing into voice.transcript
func fullDuplexListener(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			voiceLogger().Debug(fmt.Sprintf("full-duplex listener failed: %s", panicMessage(r)))
		}
		fdListenerMu.Lock()
		fdListenerActive = false
		fdListenerMu.Unlock()
	}()
	shouldStop := func() bool {
		return !voiceModeEnabled() || !(anySessionRunning() || fdTTSPending() || media.IsAudioOutputActive())
	}
	tripped := &pythread.Event{}
	mult, graceMS := fdBargeParams(voiceCfgDict(ctx))
	opts := media.DefaultDuplexOptions()
	opts.IsPlaying, opts.Multiplier, opts.GraceMS = media.IsAudioOutputActive, mult, graceMS
	opts.OnTrigger = func(phase string) {
		tripped.Set()
		fdTrip(ctx, phase)
	}
	wavPath := media.FullDuplexListen(shouldStop, opts)
	if wavPath == "" || !tripped.IsSet() {
		return
	}
	defer func() { _ = os.Remove(wavPath) }()
	result, err := media.TranscribeRecording(ctx, wavPath, "")
	if err != nil {
		panic(err)
	}
	if truthy(result["success"]) {
		if text := pystr.Strip(strOr(result["transcript"])); text != "" {
			deliverFDTranscript(ctx, text)
		}
	}
}

// Threshold multiplier and grace milliseconds of the voice config, malformed values reading as defaults
func fdBargeParams(cfg map[string]any) (float64, int) {
	num := func(key string, def any, conv func(any) (float64, error)) float64 {
		raw, present := cfg[key]
		if !present {
			raw = def
		}
		if v, err := conv(raw); err == nil {
			return v
		}
		v, _ := conv(def)
		return v
	}
	mult := num("barge_in_threshold_multiplier", 0, func(v any) (float64, error) {
		if !truthy(v) {
			return 0, nil
		}
		return pyint.Float(v)
	})
	grace := num("barge_in_grace_seconds", 0.5, func(v any) (float64, error) {
		f, err := pyint.Float(v)
		if err != nil {
			return 0, err
		}
		return math.Trunc(f * 1000), nil
	})
	return mult, max(0, int(grace))
}

// Latches the interruption, cuts every TTS path and interrupts running turns when tripped during generation
func fdTrip(ctx context.Context, phase string) {
	media.MarkSpeechInterrupted()
	if phase == "playback" {
		voiceLogger().Debug("TTS CUT: full-duplex listener tripped during playback")
	} else {
		voiceLogger().Debug("full-duplex listener tripped during generation — interrupting running turn(s)")
	}
	ttsStreamStop(true)
	fdListenerMu.Lock()
	for pipeline := range fdSpeakPipelines {
		pipeline.stop.Set()
	}
	fdListenerMu.Unlock()
	media.StopPlayback()
	if phase != "playback" {
		if failure := catchPanic(func() {
			for _, s := range runningSessions() {
				if a := sessionAgent(s); a != nil {
					suppress(func() { a.Interrupt("") })
				}
			}
		}); failure != nil {
			voiceLogger().Debug(fmt.Sprintf("voice interjection interrupt failed: %s", panicMessage(failure)))
		}
	}
	voiceEmit(ctx, "voice.interrupted", nil)
}

// Emits the captured interjection, a bare stop phrase also ending the voice chat
func deliverFDTranscript(ctx context.Context, text string) {
	isStop := false
	suppress(func() { isStop = media.IsVoiceStopPhrase(text) })
	if isStop {
		voiceSidMu.Lock()
		sid := voiceEventSID
		voiceSidMu.Unlock()
		endSessionVoiceChat(ctx, sid, true, false)
		voiceEmit(ctx, "voice.transcript", map[string]any{"stop_phrase": true, "text": text})
		return
	}
	voiceEmit(ctx, "voice.transcript", map[string]any{"text": text})
}

// Speaks a whole reply on a pipeline the full duplex listener can cut and waits on
func speakTextWithBarge(ctx context.Context, text string) {
	pipeline := &speakPipeline{stop: &pythread.Event{}, done: &pythread.Event{}}
	fdListenerMu.Lock()
	fdSpeakPipelines[pipeline] = true
	fdListenerMu.Unlock()
	go func() {
		defer func() {
			pipeline.done.Set()
			fdListenerMu.Lock()
			delete(fdSpeakPipelines, pipeline)
			fdListenerMu.Unlock()
		}()
		media.SpeakText(ctx, text, pipeline.stop)
	}()
	armBargeListenerIfEnabled(ctx)
}

// voice section of the config, empty for any non dict shape
func voiceCfgDict(ctx context.Context) map[string]any {
	voice, isDict := loadCfg(ctx)["voice"].(map[string]any)
	if !isDict {
		return map[string]any{}
	}
	return voice
}
