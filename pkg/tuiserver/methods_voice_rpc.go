//go:build !icebath_core && !icebath_noextras

package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Binds the wake word and voice method handlers
func init() {
	voiceHandlers["wake.start"] = wakeStart
	voiceHandlers["wake.stop"] = wakeStop
	voiceHandlers["wake.resume"] = wakeResume
	voiceHandlers["wake.status"] = wakeStatus
	voiceHandlers["voice.toggle"] = voiceToggle
	voiceHandlers["voice.record"] = voiceRecord
	registerAtExit(sweepVoiceRecordings)
}

// Longest a voice recording stays on disk once nothing reads it, a capture removing its own file after the transcription
const voiceRecordingKeepSeconds = 3600

// Removes the voice recordings a capture that died left behind
func sweepVoiceRecordings() {
	suppress(func() { media.CleanupTempRecordings(voiceRecordingKeepSeconds) })
}

// Tool env of the scoped profile home a voice call reads
func voiceEnv(ctx context.Context) *tools.Env {
	return tools.EnvForProfileHome(ctx, nil, home.Dir(ctx))
}

// Lease result of an owner gated wake call, reason not_owner when it did nothing
func ownerResult(rid any, field string, done bool, extra map[string]any) map[string]any {
	out := map[string]any{field: done, "reason": nil}
	if !done {
		out["reason"] = "not_owner"
	}
	for key, value := range extra {
		out[key] = value
	}
	return ok(rid, out)
}

// Requirements the listener would arm with, panicking when the probe fails
func wakeProbe(ctx context.Context, cfg map[string]any) media.WakeRequirements {
	reqs, err := media.CheckWakeWordRequirements(ctx, cfg, nil)
	if err != nil {
		panic(err)
	}
	return reqs
}

// Writes wake_word.enabled for an explicit gesture, false when it could not be saved
func persistWakeEnabled(ctx context.Context, enabled bool) (saved bool) {
	defer func() {
		if failure := recover(); failure != nil {
			pkgLog().Warn(fmt.Sprintf("wake: failed to persist wake_word.enabled=%v: %s", pyrepr.Repr(enabled), failureText(failure)))
			saved = false
		}
	}()
	return SaveConfigValue(ctx, "wake_word.enabled", enabled)
}

// Detection callback pausing the detector, checking ownership and emitting wake.detected on the owner's transport
func wakeDetectHandler(ctx context.Context, transport tuihost.Transport, sid, phrase string, newSession bool) func() error {
	return func() error {
		if !media.PauseListening(transport) || !media.OwnsListener(transport) {
			return nil
		}
		if tuihost.TransportIsDead(transport) {
			releaseWakeForTransport(transport)
			return nil
		}
		matchedPhrase, matchedProfile, matched := media.GetLastMatch()
		if !matched {
			matchedPhrase, matchedProfile = phrase, ""
		}
		pkgLog().Info(fmt.Sprintf("wake.detected: emitting to sid=%s (transport=%T, profile=%s)", pyrepr.Repr(sid), transport, pyrepr.Repr(matchedProfile)))
		if matchedPhrase == "" {
			matchedPhrase = phrase
		}
		var profile any
		if matchedProfile != "" {
			profile = matchedProfile
		}
		emitEvent(tuihost.BindTransport(ctx, transport), "wake.detected", sid,
			map[string]any{"phrase": matchedPhrase, "profile": profile, "start_new_session": newSession})
		return nil
	}
}

// Arms the wake word listener for the calling surface, refusing when disabled, owned or not ready
func wakeStart(ctx context.Context, rid any, params map[string]any) map[string]any {
	surface := strings.ToLower(strings.TrimSpace(relayString(params["surface"])))
	if surface == "" {
		surface = "auto"
	}
	transport := callerTransport(ctx)
	refused := func(reason string, extra map[string]any) map[string]any {
		out := map[string]any{"started": false, "reason": reason}
		for key, value := range extra {
			out[key] = value
		}
		return ok(rid, out)
	}
	cfg := media.LoadWakeWordConfig(ctx)
	reqs := wakeProbe(ctx, cfg)
	if !reqs.Available {
		pkgLog().Warn(fmt.Sprintf("wake.start(%s): not available — %s", surface, reqs.Hint))
		return refused("unavailable", map[string]any{"hint": reqs.Hint})
	}
	enabledPersisted := truthy(params["persist"]) && !truthy(cfg["enabled"]) && persistWakeEnabled(ctx, true)
	if enabledPersisted {
		next := make(map[string]any, len(cfg)+1)
		for key, value := range cfg {
			next[key] = value
		}
		next["enabled"] = true
		cfg = next
	}
	if !media.WakeSurfaceEnabled(ctx, surface, cfg) {
		reason := "disabled_for_surface"
		if !truthy(cfg["enabled"]) {
			reason = "disabled"
		}
		pkgLog().Info(fmt.Sprintf("wake.start(%s): %s (enabled=%s, surface=%s)", surface, reason, pyrepr.Repr(cfg["enabled"]), pyrepr.Repr(cfg["surface"])))
		return refused(reason, nil)
	}
	existingOwner, existingSurface := wakeOwnerSnapshot()
	if existingOwner != nil && (tuihost.TransportIsDead(existingOwner) || !media.OwnsListener(existingOwner)) {
		releaseWakeForTransport(existingOwner)
		existingOwner, existingSurface = nil, ""
	}
	if existingOwner != nil && existingOwner != transport {
		return refused("owned", map[string]any{"owner_surface": existingSurface})
	}
	newSession := true
	if raw, present := cfg["start_new_session"]; present {
		newSession = truthy(raw)
	}
	onDetect := wakeDetectHandler(context.WithoutCancel(ctx), transport, relayString(params["session_id"]), media.WakePhrase(ctx, cfg), newSession)
	if _, err := media.StartListening(context.WithoutCancel(ctx), onDetect, transport, cfg); err != nil {
		var inUse *media.WakeWordInUse
		if errors.As(err, &inUse) {
			var owner any
			if existingSurface != "" {
				owner = existingSurface
			}
			return refused("owned", map[string]any{"owner_surface": owner})
		}
		pkgLog().Warn(fmt.Sprintf("wake.start(%s): failed to start listener: %s", surface, err))
		return rpcErr(rid, 5026, err.Error(), nil)
	}
	wakeMu.Lock()
	wakeOwnerTransport, wakeOwnerSurface = transport, surface
	wakeMu.Unlock()
	phrase, provider, armed := media.ListeningFor(transport)
	if !armed {
		phrase, provider = reqs.Phrase, reqs.Provider
	}
	pkgLog().Info(fmt.Sprintf("wake.start(%s): listening for %s (%s)", surface, pyrepr.Repr(phrase), provider))
	return ok(rid, map[string]any{"started": true, "phrase": phrase, "provider": provider, "owner_surface": surface,
		"enabled_persisted": enabledPersisted})
}

// Stops this surface's listener, persist also writing wake_word.enabled false, naming the phrase and engine it ran
func wakeStop(ctx context.Context, rid any, params map[string]any) map[string]any {
	transport := callerTransport(ctx)
	phrase, provider, running := media.ListeningFor(transport)
	stopped := releaseWakeForTransport(transport)
	disabledPersisted := false
	if truthy(params["persist"]) {
		currentlyEnabled := true
		suppress(func() { currentlyEnabled = truthy(media.LoadWakeWordConfig(ctx)["enabled"]) })
		disabledPersisted = currentlyEnabled && persistWakeEnabled(ctx, false)
	}
	if !running {
		phrase, provider = media.WakePhrase(ctx, nil), ""
	}
	return ownerResult(rid, "stopped", stopped, map[string]any{"disabled_persisted": disabledPersisted,
		"phrase": phrase, "provider": provider})
}

// Reclaims the microphone after a pause, nothing when the listener is not armed
func wakeResume(ctx context.Context, rid any, _ map[string]any) map[string]any {
	resumed := wakeResumeIfOwner(callerTransport(ctx), 15e9, 1e9)
	pkgLog().Info(fmt.Sprintf("wake.resume: detector resumed=%s", pyrepr.Repr(resumed)))
	return ownerResult(rid, "resumed", resumed, nil)
}

// Plain mapping of a JSON tagged value the way its Python dict reads
func jsonMapping(value any) map[string]any {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		panic(err)
	}
	return out
}

// Listener, ownership, requirement and input device state for the calling surface
func wakeStatus(ctx context.Context, rid any, _ map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, 5026, failureText(failure), nil)
		}
	}()
	cfg := media.LoadWakeWordConfig(ctx)
	reqs := wakeProbe(ctx, cfg)
	owner, ownerSurface := wakeOwnerSnapshot()
	ownedByCaller := media.OwnsListener(callerTransport(ctx))
	listening := ownedByCaller && media.IsListening()
	silent := listening && media.AudioIsSilent()
	inputDevice := media.GetInputDeviceStatus(ctx, cfg)
	hint := reqs.Hint
	if inputDevice.Error != nil && *inputDevice.Error != "" && hint == "" {
		hint = "Wake-word input device could not be resolved: " + *inputDevice.Error
	}
	if silent && hint == "" {
		hint = media.SilentAudioHint(inputDevice)
	}
	var surfaceOut any
	if owner != nil {
		surfaceOut = ownerSurface
	}
	configuredSurface := "auto"
	if truthy(cfg["surface"]) {
		configuredSurface = relayString(cfg["surface"])
	}
	return ok(rid, map[string]any{"listening": listening, "owned_by_caller": ownedByCaller, "owner_surface": surfaceOut,
		"phrase": reqs.Phrase, "provider": reqs.Provider, "configured_surface": configuredSurface,
		"input_device": jsonMapping(inputDevice), "available": reqs.Available, "hint": hint, "enabled": truthy(cfg["enabled"]),
		"audio_silent": silent, "local_input_available": reqs.LocalInputAvailable})
}

// Enabled, record key and TTS flags every voice.toggle branch answers with
func voiceStatusPayload(ctx context.Context, extra map[string]any) map[string]any {
	out := map[string]any{"enabled": voiceModeEnabled(), "record_key": settings.RecordKey.Get(ctx), "tts": voiceTTSEnabled()}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

// Acquires or releases a TTS lease off the RPC goroutine, a failure only logged
func ttsLeaseAsync(ctx context.Context, lease string, active bool) {
	detached := context.WithoutCancel(ctx)
	env := voiceEnv(ctx)
	go func() {
		defer func() {
			if failure := recover(); failure != nil {
				pkgLog().Debug(fmt.Sprintf("voice: tts lease %s active=%s failed: %s", lease, pyrepr.Repr(active), failureText(failure)))
			}
		}()
		if active {
			media.AcquireTTSLease(detached, env, lease, nil)
		} else {
			media.ReleaseTTSLease(detached, env, lease)
		}
	}()
}

// Flips spoken replies, off silencing live speech, the lease warming or releasing the engine
func setVoiceTTS(ctx context.Context, on bool) {
	setVoiceFlag(ctx, settings.SpeakReplies.Key, on)
	if !on {
		ttsStreamStop(false)
	}
	ttsLeaseAsync(ctx, "tui:voice-tts", on)
}

// voice status, mode on or off, or TTS toggle as the CLI voice command does
func voiceToggle(ctx context.Context, rid any, params map[string]any) map[string]any {
	action, present := params["action"]
	if !present {
		action = "status"
	}
	text, isText := action.(string)
	if isText && (text == "on" || text == "off") {
		go sweepVoiceRecordings()
	}
	if isText && (text == "status" || text == "on" || text == "off" || text == "tts") {
		if sid := relayString(params["session_id"]); sid != "" {
			mode, held, err := gateway.LocalVoiceMode(sid)
			if err != nil {
				return rpcErr(rid, 5025, err.Error(), nil)
			}
			if held {
				return sessionVoiceToggle(ctx, rid, sid, mode, text)
			}
		}
		if gateway.ActiveAPIRunner() != nil && text != "status" {
			return rpcErr(rid, 4014, "voice mode belongs to a session on the shared gateway: name the session_id of a live session", nil)
		}
	}
	switch {
	case isText && text == "status":
		payload := voiceStatusPayload(ctx, nil)
		if gateway.ActiveAPIRunner() != nil {
			payload["enabled"], payload["tts"] = false, false
		}
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
			payload["available"], payload["audio_available"], payload["stt_available"] = reqs.Available, reqs.AudioAvailable, reqs.STTAvailable
			payload["details"] = reqs.Details
		}()
		return ok(rid, payload)
	case isText && (text == "on" || text == "off"):
		enabled := text == "on"
		setVoiceFlag(ctx, settings.VoiceInput.Key, enabled)
		stopHint := ""
		if enabled {
			suppress(func() { stopHint = media.VoiceStopHint() })
			if voiceTTSEnabled() {
				ttsLeaseAsync(ctx, "tui:voice-tts", true)
			}
		} else {
			func() {
				defer func() {
					if failure := recover(); failure != nil {
						pkgLog().Warn(fmt.Sprintf("voice: stop_continuous failed during toggle off: %s", failureText(failure)))
					}
				}()
				media.StopContinuous(ctx, false)
			}()
			setVoiceTTS(ctx, false)
		}
		return ok(rid, voiceStatusPayload(ctx, map[string]any{"stop_hint": stopHint}))
	case isText && text == "tts":
		if !voiceModeEnabled() {
			return rpcErr(rid, 4014, "enable voice mode first: /voice on", nil)
		}
		setVoiceTTS(ctx, !voiceTTSEnabled())
		return ok(rid, voiceStatusPayload(ctx, nil))
	}
	return rpcErr(rid, 4013, fmt.Sprintf("unknown voice action: %s", pyrepr.Str(action)), nil)
}

// Wraps fn so it runs only while the capture it was registered for is current
func voiceCaptureCallback[T any](fn func(T)) func(T) {
	voiceSidMu.Lock()
	gen := voiceCaptureGen
	voiceSidMu.Unlock()
	return func(arg T) {
		voiceSidMu.Lock()
		current := gen == voiceCaptureGen
		voiceSidMu.Unlock()
		if current {
			fn(arg)
		}
	}
}

// Emits a terminal capture event and hands the microphone back to the wake detector
func voiceRecordTranscript(ctx context.Context, payload map[string]any) {
	voiceEmit(ctx, "voice.transcript", payload)
	resumeVoiceWake()
}

// VAD bounded push to talk, start capturing until silence and stop forcing transcription
func voiceRecord(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	action, present := params["action"]
	if !present {
		action = "start"
	}
	if action != "start" && action != "stop" {
		return rpcErr(rid, 4019, fmt.Sprintf("unknown voice action: %s", pyrepr.Str(action)), nil)
	}
	transport := callerTransport(ctx)
	if wakeOwner, _ := wakeOwnerSnapshot(); wakeOwner != nil && wakeOwner != transport {
		return ok(rid, map[string]any{"status": "busy", "reason": "wake_owned"})
	}
	wakePaused := false
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			if wakePaused || action == "stop" {
				resumeVoiceWake()
			}
			response = rpcErr(rid, 5025, failureText(failure), nil)
		}
	}()
	if action == "start" {
		enabled, err := sessionVoiceEnabled(params["session_id"])
		if err != nil {
			panic(err)
		}
		if !enabled {
			return rpcErr(rid, 4015, "voice mode is off — enable with /voice on", nil)
		}
	}
	sid := relayString(params["session_id"])
	if action == "start" && sid != "" {
		if _, held, _ := gateway.LocalVoiceMode(sid); held {
			claimSessionSpeaker(sid, transport)
			cutSessionSpeech(sid, false)
		}
	}
	voiceSidMu.Lock()
	captureSID := voiceCaptureSID
	voiceSidMu.Unlock()
	if captureSID != "" && sid != captureSID && voiceContinuousActive() {
		return rpcErr(rid, 4016, "voice capture is busy in another session", nil)
	}
	voiceSidMu.Lock()
	if sid != "" {
		voiceEventSID = sid
	}
	voiceEventTransport = transport
	voiceSidMu.Unlock()
	detached := context.WithoutCancel(ctx)
	if action == "stop" {
		media.StopContinuous(ctx, true)
		resumeVoiceWake()
		return ok(rid, map[string]any{"status": "stopped"})
	}
	ttsStreamStop(true)
	suppress(func() { media.SetVoiceBusyProbe(anySessionRunning) })
	maxRec := settings.VoiceMaxRecordingSeconds.Get(ctx).Seconds()
	suppress(func() { wakePaused = media.PauseListening(transport) })
	if wakePaused {
		voiceSidMu.Lock()
		voiceWakeOwner = transport
		voiceSidMu.Unlock()
	}
	maxRecording := 0.0
	if maxRec > 0 {
		maxRecording = maxRec
	}
	onSilent := voiceCaptureCallback(func(struct{}) {
		voiceSidMu.Lock()
		current := voiceCaptureSID
		voiceSidMu.Unlock()
		setSessionVoiceOff(current)
		voiceRecordTranscript(detached, map[string]any{"no_speech_limit": true})
	})
	started, err := media.StartContinuous(ctx, media.ContinuousOptions{
		ContinuousCallbacks: media.ContinuousCallbacks{
			OnTranscript: voiceCaptureCallback(func(text string) { voiceRecordTranscript(detached, map[string]any{"text": text}) }),
			OnStatus: func(state string) {
				voiceEmit(detached, "voice.status", map[string]any{"state": state})
				if state == "idle" {
					resumeVoiceWake()
				}
			},
			OnSilentLimit: func() { onSilent(struct{}{}) },
			OnStopPhrase: voiceCaptureCallback(func(text string) {
				voiceSidMu.Lock()
				current := voiceCaptureSID
				voiceSidMu.Unlock()
				endSessionVoiceChat(detached, current, false, true)
				voiceRecordTranscript(detached, map[string]any{"stop_phrase": true, "text": text})
			}),
		},
		SilenceThreshold: float64(settings.VoiceSilenceThreshold.Get(ctx)), SilenceDuration: settings.VoiceSilenceDuration.Get(ctx),
		AutoRestart: false, MaxRecordingSeconds: maxRecording,
		OnPartial: voiceCaptureCallback(func(text string) { voiceEmit(detached, "voice.partial", map[string]any{"text": text}) }),
	})
	if err != nil {
		panic(err)
	}
	if !started {
		resumeVoiceWake()
		return ok(rid, map[string]any{"status": "busy"})
	}
	voiceSidMu.Lock()
	voiceCaptureSID = sid
	voiceSidMu.Unlock()
	return ok(rid, map[string]any{"status": "recording"})
}
