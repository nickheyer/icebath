//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Voice state of shared gateway sessions, the window that plays each one's speech and the voice turn speaking it
var (
	sessionVoiceMu   sync.Mutex
	sessionSpeakers  = map[string]tuihost.Transport{}
	sessionTurns     = map[string]*sessionVoiceTurn{}
	sessionTTSLeases = map[string]tuihost.Transport{}
	remotePlaying    = map[tuihost.Transport]bool{}
)

func init() {
	gateway.OpenLocalVoiceTurn = openLocalVoiceTurn
	voiceHandlers["voice.playback"] = voicePlayback
	voiceModeOps.releaseTransport = releaseVoiceTransport
}

// One turn of a shared gateway session with voice on, its reply synthesized here and played by the session's window
type sessionVoiceTurn struct {
	sid      string
	ctx      context.Context
	speaker  tuihost.Transport
	queue    *ttsTurnQueue
	stop     *pythread.Event
	done     *pythread.Event
	mu       sync.Mutex
	finished bool
}

// Records the window that used voice on a session as the one that plays its speech
func claimSessionSpeaker(sid string, transport tuihost.Transport) {
	if sid == "" || transport == nil {
		return
	}
	sessionVoiceMu.Lock()
	sessionSpeakers[sid] = transport
	sessionVoiceMu.Unlock()
}

// Window playing a session's speech, the claimed one while it still views the session, else the first live viewer
func sessionSpeaker(sid string) tuihost.Transport {
	viewers := gateway.LocalVoiceViewers(sid)
	sessionVoiceMu.Lock()
	defer sessionVoiceMu.Unlock()
	claimed := sessionSpeakers[sid]
	for _, viewer := range viewers {
		if viewer == claimed && !tuihost.TransportIsDead(viewer) {
			return viewer
		}
	}
	for _, viewer := range viewers {
		if !tuihost.TransportIsDead(viewer) {
			sessionSpeakers[sid] = viewer
			return viewer
		}
	}
	delete(sessionSpeakers, sid)
	return nil
}

// Sends one speech event of a session to the window that plays it
func emitSpeech(ctx context.Context, speaker tuihost.Transport, sid string, payload tuigateway.VoiceSpeechPayload) {
	if speaker == nil || tuihost.TransportIsDead(speaker) {
		return
	}
	tuihost.Emit(tuihost.BindTransport(ctx, speaker), tuigateway.VoiceSpeechEvent.Name(), sid, tuigateway.VoiceSpeechEvent.Wire(payload))
}

// Opens the voice turn of a local session whose voice mode is on, nil when it is off
func openLocalVoiceTurn(ctx context.Context, source *gateway.SessionSource) gateway.TTSStream {
	if source == nil || source.ChatID == "" {
		return nil
	}
	sid := source.ChatID
	mode, held, err := gateway.LocalVoiceMode(sid)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("voice turn: voice mode of %s unreadable: %s", sid, err))
		return nil
	}
	if !held || mode == "off" {
		return nil
	}
	cutSessionSpeech(sid, false)
	speaker := sessionSpeaker(sid)
	turn := &sessionVoiceTurn{sid: sid, ctx: context.WithoutCancel(ctx), speaker: speaker, stop: &pythread.Event{}, done: &pythread.Event{}}
	ready := false
	if mode == "all" && speaker != nil {
		suppress(func() { ready = media.CheckTTSRequirements(turn.ctx) })
	}
	if ready {
		turn.queue = newTTSTurnQueue()
		go media.StreamTTSToFiles(turn.ctx, turn.queue, ttsEvent{turn.stop}, ttsEvent{turn.done}, "", turn.deliver)
	} else {
		turn.done.Set()
	}
	sessionVoiceMu.Lock()
	sessionTurns[sid] = turn
	sessionVoiceMu.Unlock()
	if speaker != nil {
		voiceSidMu.Lock()
		voiceEventSID, voiceEventTransport = sid, speaker
		voiceSidMu.Unlock()
		emitSpeech(turn.ctx, speaker, sid, tuigateway.VoiceSpeechPayload{Kind: "think"})
	}
	if bargeIn, present := voiceCfgDict(turn.ctx)["barge_in"]; !present || truthy(bargeIn) {
		armFullDuplexListener(turn.ctx)
	}
	return turn
}

// Reads one synthesized sentence and sends it to the window, the pipeline removing the file after
func (t *sessionVoiceTurn) deliver(path string) bool {
	if t.stop.IsSet() {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		pkgLog().Warn(fmt.Sprintf("voice turn: synthesized sentence unreadable: %v", err))
		return false
	}
	format := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	emitSpeech(t.ctx, t.speaker, t.sid, tuigateway.VoiceSpeechPayload{Kind: "audio", Audio: base64.StdEncoding.EncodeToString(data), Format: format})
	return true
}

// Always live once opened, the turn keeping the thinking blips and barge in listener for a voice session
func (t *sessionVoiceTurn) Active() bool { return true }

// The turn drains through the speech pipeline goroutine, nothing to start
func (t *sessionVoiceTurn) Start() *gateway.STTSDrainTask { return nil }

// Feeds reply text to the speech pipeline, a segment boundary left to the sentence chunker
func (t *sessionVoiceTurn) OnDelta(text *string) {
	t.mu.Lock()
	open := !t.finished
	t.mu.Unlock()
	if text != nil && open && t.queue != nil {
		t.queue.Put(*text)
	}
}

// Ends the reply text and the thinking blips, the queued sentences still spoken
func (t *sessionVoiceTurn) Finish() {
	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	t.mu.Unlock()
	if t.queue != nil {
		t.queue.Finish()
	}
	emitSpeech(t.ctx, t.speaker, t.sid, tuigateway.VoiceSpeechPayload{Kind: "quiet"})
}

// A turn cleaned up before it finished speaks what streamed, any other abort cuts the speech
func (t *sessionVoiceTurn) Abort(reason string) {
	if reason == "cleanup" {
		t.Finish()
		return
	}
	sessionVoiceMu.Lock()
	if sessionTurns[t.sid] == t {
		delete(sessionTurns, t.sid)
	}
	sessionVoiceMu.Unlock()
	t.cut()
}

// Whether the reply text has ended
func (t *sessionVoiceTurn) Done() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished
}

// The window plays the reply on its own, the gateway never waits for it
func (t *sessionVoiceTurn) SuppressWholeFile() bool { return false }

// Whether the reply text has ended, synthesis carrying on in the background as the direct route does
func (t *sessionVoiceTurn) WaitComplete(float64) bool { return t.Done() }

// Stops synthesis and tells the window to drop and silence what it queued
func (t *sessionVoiceTurn) cut() {
	t.stop.Set()
	t.mu.Lock()
	wasOpen := !t.finished
	t.finished = true
	t.mu.Unlock()
	if wasOpen && t.queue != nil {
		t.queue.Finish()
	}
	emitSpeech(t.ctx, t.speaker, t.sid, tuigateway.VoiceSpeechPayload{Kind: "stop"})
}

// Whether the turn still writes reply text or synthesizes sentences
func (t *sessionVoiceTurn) busy() bool {
	t.mu.Lock()
	finished := t.finished
	t.mu.Unlock()
	return !finished || !t.done.IsSet()
}

// Cuts the speech of one session, interrupted also telling its window that a spoken interjection cut it
func cutSessionSpeech(sid string, interrupted bool) bool {
	sessionVoiceMu.Lock()
	turn := sessionTurns[sid]
	delete(sessionTurns, sid)
	sessionVoiceMu.Unlock()
	if turn == nil {
		return false
	}
	turn.cut()
	if interrupted && turn.speaker != nil {
		tuihost.Emit(tuihost.BindTransport(turn.ctx, turn.speaker), tuigateway.VoiceInterruptedEvent.Name(), sid, nil)
	}
	return true
}

// Cuts every shared gateway session's speech for a barge in, true when one was cut
func cutAllSessionSpeech() bool {
	sessionVoiceMu.Lock()
	sids := make([]string, 0, len(sessionTurns))
	for sid := range sessionTurns {
		sids = append(sids, sid)
	}
	sessionVoiceMu.Unlock()
	cut := false
	for _, sid := range sids {
		if cutSessionSpeech(sid, true) {
			cut = true
		}
	}
	return cut
}

// Whether a shared gateway voice turn writes or speaks, or a window still sounds its speech
func sessionVoiceLive() bool {
	sessionVoiceMu.Lock()
	defer sessionVoiceMu.Unlock()
	for _, turn := range sessionTurns {
		if turn.busy() {
			return true
		}
	}
	return len(sessionTurns) > 0 && len(remotePlaying) > 0
}

// Records whether a window sounds speech, counted as audio output so barge in raises its threshold and keeps listening
func setRemotePlaying(transport tuihost.Transport, playing bool) {
	if transport == nil {
		return
	}
	sessionVoiceMu.Lock()
	was := remotePlaying[transport]
	if playing {
		remotePlaying[transport] = true
	} else {
		delete(remotePlaying, transport)
	}
	sessionVoiceMu.Unlock()
	if was != playing {
		media.MarkAudioOutputActive(playing)
	}
}

// voice.playback, the calling window's speech started or stopped sounding
func voicePlayback(ctx context.Context, rid any, params map[string]any) map[string]any {
	setRemotePlaying(callerTransport(ctx), truthy(params["playing"]))
	return ok(rid, map[string]any{})
}

// Forgets a window that left, its playback count, its speaker claims and the warm TTS leases it held
func releaseVoiceTransport(transport tuihost.Transport) {
	setRemotePlaying(transport, false)
	var leases []string
	sessionVoiceMu.Lock()
	for sid, speaker := range sessionSpeakers {
		if speaker == transport {
			delete(sessionSpeakers, sid)
		}
	}
	for sid, holder := range sessionTTSLeases {
		if holder == transport {
			leases = append(leases, sid)
		}
	}
	sessionVoiceMu.Unlock()
	for _, sid := range leases {
		sessionTTSLease(context.Background(), sid, nil, false)
	}
}

// Takes or drops the warm TTS lease of a session for the window that turned its speech on
func sessionTTSLease(ctx context.Context, sid string, holder tuihost.Transport, active bool) {
	sessionVoiceMu.Lock()
	_, held := sessionTTSLeases[sid]
	if active {
		sessionTTSLeases[sid] = holder
	} else {
		delete(sessionTTSLeases, sid)
	}
	sessionVoiceMu.Unlock()
	if active != held {
		ttsLeaseAsync(ctx, "tui:voice-tts:"+sid, active)
	}
}

// Voice action on a shared gateway session, its stored mode off, voice_only or all and the caller its speaker
func sessionVoiceToggle(ctx context.Context, rid any, sid, mode, action string) map[string]any {
	caller := callerTransport(ctx)
	claimSessionSpeaker(sid, caller)
	setMode := func(next string) error {
		if _, err := gateway.SetLocalVoiceMode(sid, next); err != nil {
			return err
		}
		mode = next
		return nil
	}
	extra := map[string]any{}
	switch action {
	case "status":
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					pkgLog().Warn(fmt.Sprintf("voice.toggle status: requirements probe failed: %s", failureText(failure)))
				}
			}()
			reqs, err := media.CheckVoiceRequirements(ctx)
			if err != nil {
				panic(err)
			}
			extra["available"], extra["audio_available"], extra["stt_available"] = reqs.Available, reqs.AudioAvailable, reqs.STTAvailable
			extra["details"] = reqs.Details
		}()
	case "on":
		if mode == "off" {
			if err := setMode("voice_only"); err != nil {
				return rpcErr(rid, 5025, err.Error(), nil)
			}
		}
		stopHint := ""
		suppress(func() { stopHint = media.VoiceStopHint() })
		extra["stop_hint"] = stopHint
		if mode == "all" {
			sessionTTSLease(ctx, sid, caller, true)
		}
	case "off":
		if err := setMode("off"); err != nil {
			return rpcErr(rid, 5025, err.Error(), nil)
		}
		cutSessionSpeech(sid, false)
		sessionTTSLease(ctx, sid, nil, false)
		extra["stop_hint"] = ""
	case "tts":
		if mode == "off" {
			return rpcErr(rid, 4014, "enable voice mode first: /voice on", nil)
		}
		next := "all"
		if mode == "all" {
			next = "voice_only"
		}
		if err := setMode(next); err != nil {
			return rpcErr(rid, 5025, err.Error(), nil)
		}
		if next == "voice_only" {
			cutSessionSpeech(sid, false)
		}
		sessionTTSLease(ctx, sid, caller, next == "all")
	}
	payload := voiceStatusPayload(ctx, extra)
	payload["enabled"], payload["tts"] = mode != "off", mode == "all"
	return ok(rid, payload)
}
