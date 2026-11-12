package tuiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/activesessions"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pybase64"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var voiceRPCRegistry = &handlerRegistry{}

func init() {
	voiceRPCRegistry.method("gateway.capabilities", gatewayCapabilities)
	voiceRPCRegistry.method("client.capabilities", clientCapabilities)
	voiceRPCRegistry.method("wake.start", wakeStart)
	voiceRPCRegistry.method("wake.stop", wakeStop)
	voiceRPCRegistry.method("wake.pause", wakePause)
	voiceRPCRegistry.method("wake.resume", wakeResume)
	voiceRPCRegistry.method("wake.status", wakeStatus)
	voiceRPCRegistry.method("wake.feed", wakeFeed)
	voiceRPCRegistry.method("voice.toggle", voiceToggle)
	voiceRPCRegistry.method("voice.record", voiceRecord)
	voiceRPCRegistry.method("voice.tts", voiceTTS)
	voiceRPCRegistry.install()
}

// Tool env of the scoped profile home a voice call reads
func voiceEnv(ctx context.Context) *tools.Env {
	return tools.EnvForProfileHome(ctx, nil, home.Dir(ctx))
}

// What this build enforces, sourced from the enforcing module
func gatewayCapabilities(_ context.Context, rid any, _ map[string]any) map[string]any {
	return ok(rid, map[string]any{"per_session_exclusive_submit": activesessions.PerSessionExclusiveSubmit})
}

// Records what the calling client handles, a local Desktop starting the boot work once
func clientCapabilities(ctx context.Context, rid any, params map[string]any) map[string]any {
	transport := callerTransport(ctx)
	if raw, present := params["server_requests"]; present {
		tuigateway.Advertise(transport, truthy(raw))
	}
	if declared, present := params["desktop_local"]; present {
		if ws, isWS := transport.(*tuigateway.WSTransport); isWS && ws != nil {
			ws.DesktopLocal = tools.LocalDesktopClient(ws.AuthIdentity(), declared)
			if ws.DesktopLocal {
				beginLocalDesktopBoot(ctx)
			}
		}
	}
	names := make([]string, 0, len(tuigateway.ServerRequests))
	for name := range tuigateway.ServerRequests {
		names = append(names, name)
	}
	sort.Strings(names)
	return ok(rid, map[string]any{"server_requests": names, "declines_not_shown": true, "canonical_clarify_batch": true})
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

// Sample rate and frame length of a detector frame contract
func frameFields(frame media.WakeFrameInfo) map[string]any {
	sampleRate, frameLength := frame.SampleRate, frame.FrameLength
	if sampleRate == 0 {
		sampleRate = 16000
	}
	if frameLength == 0 {
		frameLength = 1280
	}
	return map[string]any{"sample_rate": sampleRate, "frame_length": frameLength}
}

// Capture mode the surface would arm with and the requirements probed for it
func wakeProbe(ctx context.Context, cfg map[string]any, params map[string]any, surface string) (string, media.WakeRequirements) {
	preferClient := surface == "gui" || surface == "desktop" || truthy(params["client_capture"])
	capture := media.ResolveCaptureMode(ctx, cfg, preferClient, false)
	probed := make(map[string]any, len(cfg)+1)
	for key, value := range cfg {
		probed[key] = value
	}
	probed["capture"] = capture
	reqs, err := media.CheckWakeWordRequirements(ctx, probed, nil)
	if err != nil {
		panic(err)
	}
	return capture, reqs
}

// Writes wake_word.enabled for an explicit gesture, false when it could not be saved
func persistWakeEnabled(ctx context.Context, enabled bool) (saved bool) {
	defer func() {
		if failure := recover(); failure != nil {
			voiceLogger().Warn(fmt.Sprintf("wake: failed to persist wake_word.enabled=%v: %s", pyrepr.Repr(enabled), failureText(failure)))
			saved = false
		}
	}()
	return SaveConfigValue(ctx, "wake_word.enabled", enabled)
}

// Detection callback pausing the detector, checking ownership and emitting wake.detected on the owner's transport
func wakeDetectHandler(ctx context.Context, transport tuigateway.Transport, sid, phrase string, newSession bool) func() error {
	return func() error {
		if !media.PauseListening(transport) || !media.OwnsListener(transport) {
			return nil
		}
		if tuigateway.TransportIsDead(transport) {
			releaseWakeForTransport(transport)
			return nil
		}
		matchedPhrase, matchedProfile, matched := media.GetLastMatch()
		if !matched {
			matchedPhrase, matchedProfile = phrase, ""
		}
		voiceLogger().Info(fmt.Sprintf("wake.detected: emitting to sid=%s (transport=%T, profile=%s)", pyrepr.Repr(sid), transport, pyrepr.Repr(matchedProfile)))
		if matchedPhrase == "" {
			matchedPhrase = phrase
		}
		var profile any
		if matchedProfile != "" {
			profile = matchedProfile
		}
		emitEvent(tuigateway.BindTransport(ctx, transport), "wake.detected", sid,
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
	capture, reqs := wakeProbe(ctx, cfg, params, surface)
	if !reqs.Available {
		voiceLogger().Warn(fmt.Sprintf("wake.start(%s): not available — %s", surface, reqs.Hint))
		return refused("unavailable", map[string]any{"hint": reqs.Hint, "capture": capture})
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
		voiceLogger().Info(fmt.Sprintf("wake.start(%s): %s (enabled=%s, surface=%s)", surface, reason, pyrepr.Repr(cfg["enabled"]), pyrepr.Repr(cfg["surface"])))
		return refused(reason, nil)
	}
	existingOwner, existingSurface := wakeOwnerSnapshot()
	if existingOwner != nil && (tuigateway.TransportIsDead(existingOwner) || !media.OwnsListener(existingOwner)) {
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
	if _, err := media.StartListening(context.WithoutCancel(ctx), onDetect, transport, cfg, capture == "client"); err != nil {
		var inUse *media.WakeWordInUse
		if errors.As(err, &inUse) {
			var owner any
			if existingSurface != "" {
				owner = existingSurface
			}
			return refused("owned", map[string]any{"owner_surface": owner})
		}
		voiceLogger().Warn(fmt.Sprintf("wake.start(%s): failed to start listener: %s", surface, err))
		return rpcErr(rid, 5026, err.Error(), nil)
	}
	wakeMu.Lock()
	wakeOwnerTransport, wakeOwnerSurface = transport, surface
	wakeMu.Unlock()
	frame := media.DetectorFrameInfo()
	voiceLogger().Info(fmt.Sprintf("wake.start(%s): listening for %s (%s) capture=%s frame=%d", surface, pyrepr.Repr(reqs.Phrase), reqs.Provider, capture, frame.FrameLength))
	out := map[string]any{"started": true, "phrase": reqs.Phrase, "provider": reqs.Provider, "owner_surface": surface,
		"enabled_persisted": enabledPersisted, "capture": capture}
	for key, value := range frameFields(frame) {
		out[key] = value
	}
	return ok(rid, out)
}

// Stops this surface's listener, persist also writing wake_word.enabled false
func wakeStop(ctx context.Context, rid any, params map[string]any) map[string]any {
	stopped := releaseWakeForTransport(callerTransport(ctx))
	disabledPersisted := false
	if truthy(params["persist"]) {
		currentlyEnabled := true
		suppress(func() { currentlyEnabled = truthy(media.LoadWakeWordConfig(ctx)["enabled"]) })
		disabledPersisted = currentlyEnabled && persistWakeEnabled(ctx, false)
	}
	return ownerResult(rid, "stopped", stopped, map[string]any{"disabled_persisted": disabledPersisted})
}

// Releases the microphone while another capture holds it
func wakePause(ctx context.Context, rid any, _ map[string]any) map[string]any {
	paused := false
	func() {
		defer func() {
			if failure := recover(); failure != nil {
				voiceLogger().Debug(fmt.Sprintf("wake.pause failed: %s", failureText(failure)))
				paused = false
			}
		}()
		paused = media.PauseListening(callerTransport(ctx))
		voiceLogger().Info(fmt.Sprintf("wake.pause: detector paused=%s", pyrepr.Repr(paused)))
	}()
	return ownerResult(rid, "paused", paused, nil)
}

// Reclaims the microphone after a pause, nothing when the listener is not armed
func wakeResume(ctx context.Context, rid any, _ map[string]any) map[string]any {
	resumed := wakeResumeIfOwner(callerTransport(ctx), 15e9, 1e9)
	voiceLogger().Info(fmt.Sprintf("wake.resume: detector resumed=%s", pyrepr.Repr(resumed)))
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

// Listener, ownership, requirement, input device and capture state for the calling surface
func wakeStatus(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, 5026, failureText(failure), nil)
		}
	}()
	cfg := media.LoadWakeWordConfig(ctx)
	probeCapture, reqs := wakeProbe(ctx, cfg, params, strings.ToLower(strings.TrimSpace(relayString(params["surface"]))))
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
	frame := media.DetectorFrameInfo()
	external := frame.ExternalAudio != nil && *frame.ExternalAudio
	var capture string
	switch {
	case ownedByCaller && (external || listening):
		capture = "local"
		if external {
			capture = "client"
		}
	case probeCapture != "":
		capture = probeCapture
	case reqs.Capture != "":
		capture = reqs.Capture
	default:
		capture = "auto"
		if truthy(cfg["capture"]) {
			capture = relayString(cfg["capture"])
		}
	}
	var surfaceOut any
	if owner != nil {
		surfaceOut = ownerSurface
	}
	configuredSurface := "auto"
	if truthy(cfg["surface"]) {
		configuredSurface = relayString(cfg["surface"])
	}
	out := map[string]any{"listening": listening, "owned_by_caller": ownedByCaller, "owner_surface": surfaceOut,
		"phrase": reqs.Phrase, "provider": reqs.Provider, "configured_surface": configuredSurface,
		"input_device": jsonMapping(inputDevice), "available": reqs.Available, "hint": hint, "enabled": truthy(cfg["enabled"]),
		"audio_silent": silent, "capture": capture, "local_input_available": reqs.LocalInputAvailable}
	for key, value := range frameFields(frame) {
		out[key] = value
	}
	return ok(rid, out)
}

// Pushes client captured 16 kHz int16 mono PCM into the armed detector
func wakeFeed(ctx context.Context, rid any, params map[string]any) map[string]any {
	raw := params["pcm"]
	if !truthy(raw) {
		raw = params["pcm_b64"]
	}
	if !truthy(raw) {
		raw = ""
	}
	text, isText := raw.(string)
	if !isText || strings.TrimSpace(text) == "" {
		return rpcErr(rid, 4001, "wake.feed requires base64 pcm", nil)
	}
	pcm, err := pybase64.B64Decode(text)
	if err != nil {
		return rpcErr(rid, 4001, fmt.Sprintf("invalid base64 pcm: %s", err), nil)
	}
	if len(pcm) == 0 {
		return ok(rid, map[string]any{"fed": false, "reason": "empty"})
	}
	if len(pcm) > 64000 {
		return rpcErr(rid, 4001, "pcm frame too large", nil)
	}
	if rate, present := params["sample_rate"]; present && rate != nil {
		value, err := pyint.Of(rate)
		if err != nil {
			panic(err)
		}
		if !value.IsInt64() || value.Int64() != 0 && value.Int64() != 16000 {
			return rpcErr(rid, 4001, "wake.feed only accepts 16 kHz PCM", nil)
		}
	}
	fed, err := media.FeedAudio(callerTransport(ctx), pcm)
	if err != nil {
		voiceLogger().Debug(fmt.Sprintf("wake.feed failed: %s", err))
		return rpcErr(rid, 5026, err.Error(), nil)
	}
	return ownerResult(rid, "fed", fed, nil)
}

// Voice cfg value when numeric and not a bool, else the default
func voiceCfgNumber(value any, def float64) float64 {
	switch v := value.(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return def
}

// Enabled, record key and TTS flags every voice.toggle branch answers with
func voiceStatusPayload(ctx context.Context, extra map[string]any) map[string]any {
	recordKey, isText := voiceCfgDict(ctx)["record_key"].(string)
	if !isText || recordKey == "" {
		recordKey = "ctrl+b"
	}
	out := map[string]any{"enabled": voiceModeEnabled(), "record_key": recordKey, "tts": voiceTTSEnabled()}
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
				voiceLogger().Debug(fmt.Sprintf("voice: tts lease %s active=%s failed: %s", lease, pyrepr.Repr(active), failureText(failure)))
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
	flag := "0"
	if on {
		flag = "1"
	}
	_ = os.Setenv("ICEBATH_VOICE_TTS", flag)
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
	switch {
	case isText && text == "status":
		payload := voiceStatusPayload(ctx, nil)
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					voiceLogger().Warn(fmt.Sprintf("voice.toggle status: requirements probe failed: %s", failureText(failure)))
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
		flag := "0"
		if enabled {
			flag = "1"
		}
		_ = os.Setenv("ICEBATH_VOICE", flag)
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
						voiceLogger().Warn(fmt.Sprintf("voice: stop_continuous failed during toggle off: %s", failureText(failure)))
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
	voiceCfg := voiceCfgDict(ctx)
	maxRec := voiceCfgNumber(voiceCfg["max_recording_seconds"], 120.0)
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
		SilenceThreshold: voiceCfgNumber(voiceCfg["silence_threshold"], 200), SilenceDuration: voiceCfgNumber(voiceCfg["silence_duration"], 3.0),
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

// Speaks text on a pipeline the full duplex listener can cut
func voiceTTS(ctx context.Context, rid any, params map[string]any) map[string]any {
	text, present := params["text"]
	if !present {
		text = ""
	}
	if !truthy(text) {
		return rpcErr(rid, 4020, "text required", nil)
	}
	speakTextWithBarge(context.WithoutCancel(ctx), pyrepr.Str(text))
	return ok(rid, map[string]any{"status": "speaking"})
}
