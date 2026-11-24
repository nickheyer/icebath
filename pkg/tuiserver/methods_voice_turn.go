//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"strings"

	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var (
	voiceCaptureSID string
	voiceCaptureGen int

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
func voiceWakeResumeIfOwner(owner tuihost.Transport, retry, interval time.Duration) bool {
	resumed, err := media.ResumeListening(owner)
	if err == nil {
		return resumed
	}
	pkgLog().Debug(fmt.Sprintf("wake resume failed (will retry): %s", err))
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
				pkgLog().Info("wake: detector resumed after retry")
			}
			return
		}
		pkgLog().Warn("wake: could not resume detector after voice turn (microphone still busy?) — toggle the wake word to re-arm")
	}()
	return false
}

// Whether a continuous voice capture is live
func voiceContinuousLoopActive() bool { return media.IsContinuousActive() }

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
	setVoiceFlag(ctx, settings.VoiceInput.Key, false)
	setVoiceFlag(ctx, settings.SpeakReplies.Key, false)
	if stopLoop {
		suppress(func() { media.StopContinuous(ctx, false) })
	}
	if stopTTS {
		suppress(func() { ttsStreamStop(false) })
	}
}

// Ends a session's voice chat through its own mode when an owner holds it, else through the env flags
func voiceEndSessionChat(ctx context.Context, sessionID string, stopLoop, stopTTS bool) {
	if !setSessionVoiceOff(sessionID) {
		endVoiceChat(ctx, stopLoop, stopTTS)
		return
	}
	sessionTTSLease(ctx, sessionID, nil, false)
	if stopTTS {
		cutSessionSpeech(sessionID, false)
	}
}

// Binds the voice mode operations the turn and session code reaches
func init() {
	voiceModeOps.endSessionChat = voiceEndSessionChat
	voiceModeOps.resumeWakeIfOwner = voiceWakeResumeIfOwner
	voiceModeOps.stopTTSStream = voiceTTSStreamStop
	voiceModeOps.armFullDuplex = voiceArmFullDuplex
	voiceModeOps.beginTTSStream = voiceTTSStreamBegin
	voiceModeOps.continuousActive = voiceContinuousLoopActive
	voiceModeOps.speakWithBarge = voiceSpeakWithBarge
	voiceModeOps.isStopPhrase = media.IsVoiceStopPhrase
	voiceModeOps.stopListening = func(owner tuihost.Transport) { media.StopListening(owner) }
	voiceModeOps.startThinkingSound = func(ctx context.Context) bool {
		return media.StartThinkingSound(ctx, func() bool {
			if media.IsAudioOutputActive() {
				return false
			}
			return !voiceContinuousActive()
		})
	}
	voiceModeOps.stopThinkingSound = media.StopThinkingSound
	voiceModeOps.markInterrupted = media.MarkSpeechInterrupted
	voiceModeOps.takeInterrupted = media.TakeSpeechInterrupted
}

// Starts the per turn streaming TTS consumer, nil when spoken replies are off or cannot stream
func voiceTTSStreamBegin(ctx context.Context) *ttsTurnQueue {
	if !voiceTTSEnabled() {
		return nil
	}
	ready := false
	suppress(func() { ready = media.CheckTTSRequirements(ctx) })
	if !ready {
		return nil
	}
	ttsStreamStop(true)
	queue := newTTSTurnQueue()
	stop, done := &pythread.Event{}, &pythread.Event{}
	go media.StreamTTSToSpeaker(context.WithoutCancel(ctx), queue, ttsEvent{stop}, ttsEvent{done}, nil, "")
	ttsStreamMu.Lock()
	ttsStreamState = &ttsStream{stop: stop, done: done}
	ttsStreamMu.Unlock()
	armBargeListenerIfEnabled(ctx)
	return queue
}

// Cuts in flight streaming TTS, a user barge latching the interruption for the next turn
func voiceTTSStreamStop(userBarge bool) {
	ttsStreamMu.Lock()
	state := ttsStreamState
	ttsStreamState = nil
	ttsStreamMu.Unlock()
	if state == nil {
		return
	}
	if userBarge && !state.done.IsSet() {
		pkgLog().Debug("TTS CUT: _tts_stream_stop(user_barge=True) — new turn or interrupt cutting in-flight TTS")
		media.MarkSpeechInterrupted()
	}
	state.stop.Set()
	suppress(media.StopPlayback)
}

// Arms the process global full duplex listener once, one microphone
func voiceArmFullDuplex(ctx context.Context) {
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
	if voiceModeEnabled() && settings.VoiceBargeIn.Get(ctx) {
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
			pkgLog().Debug(fmt.Sprintf("full-duplex listener failed: %s", panicMessage(r)))
		}
		fdListenerMu.Lock()
		fdListenerActive = false
		fdListenerMu.Unlock()
	}()
	shouldStop := func() bool {
		direct := voiceModeEnabled() && (fdTTSPending() || media.IsAudioOutputActive())
		return !direct && !sessionVoiceLive()
	}
	tripped := &pythread.Event{}
	var trippedDuring atomic.Value
	mult, graceMS := settings.VoiceBargeInThresholdMultiplier.Get(ctx), max(0, int(settings.VoiceBargeInGraceSeconds.Get(ctx).Milliseconds()))
	opts := media.DefaultDuplexOptions()
	opts.IsPlaying, opts.Multiplier, opts.GraceMS = media.IsAudioOutputActive, mult, graceMS
	opts.OnTrigger = func(phase string) {
		trippedDuring.Store(phase)
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
		if text := strings.TrimSpace(strOr(result["transcript"])); text != "" {
			if phase, _ := trippedDuring.Load().(string); phase == "playback" && media.IsTTSEcho(text, spokenText()) {
				pkgLog().Debug("full-duplex listener: capture during playback repeats the spoken reply, dropped as speaker bleed")
				return
			}
			deliverFDTranscript(ctx, text)
		}
	}
}

// Latches the interruption and cuts every TTS path, each cut session announcing the interruption so its window stops the turn
func fdTrip(ctx context.Context, phase string) {
	media.MarkSpeechInterrupted()
	if phase == "playback" {
		pkgLog().Debug("TTS CUT: full-duplex listener tripped during playback")
	} else {
		pkgLog().Debug("full-duplex listener tripped during generation — interrupting running turn(s)")
	}
	ttsStreamStop(true)
	fdListenerMu.Lock()
	for pipeline := range fdSpeakPipelines {
		pipeline.stop.Set()
	}
	fdListenerMu.Unlock()
	media.StopPlayback()
	if !cutAllSessionSpeech() {
		voiceEmit(ctx, "voice.interrupted", nil)
	}
}

// Emits the captured interjection, a bare stop phrase also ending the voice chat
func deliverFDTranscript(ctx context.Context, text string) {
	isStop := false
	suppress(func() { isStop = media.IsVoiceStopPhrase(ctx, text) })
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
func voiceSpeakWithBarge(ctx context.Context, text string) {
	noteSpoken(text)
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
