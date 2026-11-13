package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Wake word and full duplex state of the voice mixin
type cliVoiceExtra struct {
	fdActive            *syncEvent
	noSpeechCount       int
	wakeWordActive      bool
	wakeSuspended       bool
	wakeStartNewSession bool
	wakeWatchdogStarted bool
}

// Audio and speech entry points of the voice mixin, the tools.voice_mode and tts_tool functions
var (
	voiceCheckRequirements      = media.CheckVoiceRequirements
	voiceDetectAudioEnvironment = media.DetectAudioEnvironment
	voiceCreateAudioRecorder    = media.CreateAudioRecorder
	voicePlayBeep               = media.PlayBeep
	voiceTranscribeRecording    = media.TranscribeRecording
	voiceTextToSpeech           = media.TextToSpeechTool
	voicePlayAudioFile          = media.PlayAudioFile
	voiceStopPlayback           = media.StopPlayback
	voiceFullDuplexListen       = media.FullDuplexListen
	voiceIsAudioOutputActive    = media.IsAudioOutputActive
)

// Section of the loaded config as a dict, empty for a non dict or a failed load
func voiceConfigSection(ctx context.Context, name string) (out map[string]any) {
	defer func() {
		if r := recover(); r != nil {
			out = map[string]any{}
		}
	}()
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err != nil {
		return map[string]any{}
	}
	if m, ok := cfg[name].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// Value when it is a real number and not a bool, else def
func numericOr(value any, def *float64) *float64 {
	switch v := value.(type) {
	case bool:
		return def
	case int, int64, float64:
		f, err := pyint.Float(v)
		if err != nil {
			return def
		}
		return &f
	}
	return def
}

// Removes a file when it exists, ignoring errors
func voiceUnlinkQuietly(path string) {
	if path == "" {
		return
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		_ = os.Remove(path)
	}
}

// Dim voice line through cprint
func voiceDim(text string) string { return dimANSI + text + resetANSI }

// Repaints the TUI when the app is live
func (c *CLI) voiceInvalidate() {
	if c.app != nil {
		_ = c.app.Invalidate()
	}
}

// Starts microphone capture after checking requirements, with the silence auto stop and level refresh
func (c *CLI) voiceStartRecording() error {
	c.uiMu.Lock()
	exiting := c.shouldExit
	c.uiMu.Unlock()
	if exiting {
		return nil
	}
	reqs, err := voiceCheckRequirements(c.ctx)
	if err != nil {
		return err
	}
	if !reqs.AudioAvailable {
		if hostinfo.IsTermux() {
			if strings.Contains(reqs.Details, "Termux:API Android app is not installed") {
				return fmt.Errorf("%s", c.t("cli.voice.err_termux_app_missing", nil))
			}
			return fmt.Errorf("%s", c.t("cli.voice.err_mic_access", nil))
		}
		return fmt.Errorf("%s", c.t("cli.voice.err_audio_capture", nil)+"\n"+reqs.Details)
	}
	if !reqs.STTAvailable {
		return fmt.Errorf("%s", c.t("cli.voice.err_stt_provider", nil))
	}
	c.voiceLock.Lock()
	if c.voiceRecording {
		c.voiceLock.Unlock()
		return nil
	}
	c.voiceRecording = true
	c.voiceLock.Unlock()
	voiceCfg := voiceConfigSection(c.ctx, "voice")
	if c.voiceRecorder == nil {
		c.voiceRecorder = voiceCreateAudioRecorder()
	}
	rec := c.voiceRecorder.(media.VoiceRecorder)
	c.voiceLiveText = ""
	rec.SetOnLivePartial(c.voiceOnLivePartial)
	threshold, duration := 200.0, 3.0
	rec.SetSilenceThreshold(*numericOr(voiceCfg["silence_threshold"], &threshold))
	rec.SetSilenceDuration(*numericOr(voiceCfg["silence_duration"], &duration))
	maxRec := 120.0
	if m := numericOr(voiceCfg["max_recording_seconds"], nil); m != nil {
		maxRec = 0.0
		if *m > 0 {
			maxRec = *m
		}
	}
	rec.SetMaxRecordingSeconds(maxRec)
	onSilence := func() {
		c.voiceLock.Lock()
		recording := c.voiceRecording
		c.voiceLock.Unlock()
		if !recording {
			return
		}
		cprint("\n" + voiceDim(c.t("cli.voice.silence_autostop", nil)))
		c.voiceInvalidate()
		c.voiceStopAndTranscribe()
	}
	c.voiceBeep(880, 1)
	if err := rec.Start(c.ctx, onSilence); err != nil {
		c.voiceLock.Lock()
		c.voiceRecording = false
		c.voiceLock.Unlock()
		return err
	}
	label := c.voiceRecordKeyLabel()
	hint := c.t("cli.voice.hint_stop", map[string]any{"shortcut": label})
	if rec.SupportsSilenceAutostop() {
		hint = c.t("cli.voice.hint_continuous", map[string]any{"shortcut": label})
	} else if hostinfo.IsTermux() {
		hint = c.t("cli.voice.hint_termux", map[string]any{"shortcut": label})
	}
	cprint("\n" + accentANSI.String() + c.t("cli.voice.recording", nil) + resetANSI + " " + voiceDim("("+hint+")"))
	go func() {
		for {
			c.voiceLock.Lock()
			still := c.voiceRecording
			c.voiceLock.Unlock()
			if !still {
				return
			}
			c.voiceInvalidate()
			time.Sleep(150 * time.Millisecond)
		}
	}()
	return nil
}

// Plays a record start or stop beep when enabled, never failing
func (c *CLI) voiceBeep(frequency, count int) {
	if c.voiceBeepsEnabled() {
		probeCall(func() { voicePlayBeep(c.ctx, frequency, 0.1, count) })
	}
}

// STT model override from config, the local model defaulting to base, empty for the provider default
func (c *CLI) voiceSTTModel() string {
	stt := voiceConfigSection(c.ctx, "stt")
	if pystr.Lower(pystr.Strip(pyrepr.Str(pyrepr.Or(stt["provider"], "")))) == "local" {
		local, _ := stt["local"].(map[string]any)
		if m := local["model"]; pyrepr.Truthy(m) {
			return pyrepr.Str(m)
		}
		return "base"
	}
	if m, ok := stt["model"]; ok && m != nil {
		return pyrepr.Str(m)
	}
	return ""
}

// Configured STT provider name lowercased, empty when unset
func (c *CLI) voiceSTTProvider() string {
	return pystr.Lower(pystr.Strip(pyrepr.Str(pyrepr.Or(voiceConfigSection(c.ctx, "stt")["provider"], ""))))
}

// Restarts continuous recording off the calling goroutine
func (c *CLI) voiceRestartRecordingAsync() {
	go func() {
		if err := c.voiceStartRecording(); err != nil {
			cprint(voiceDim(c.t("cli.voice.autorestart_failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})))
			return
		}
		c.voiceInvalidate()
	}()
}

// Shows a live STT partial in the input hint while recording
func (c *CLI) voiceOnLivePartial(text string) {
	c.voiceLiveText = text
	c.voiceInvalidate()
}

// Stops recording, transcribes and queues the transcript as voice input, restarting continuous capture
func (c *CLI) voiceStopAndTranscribe() {
	c.voiceLock.Lock()
	if !c.voiceRecording {
		c.voiceLock.Unlock()
		return
	}
	c.voiceRecording = false
	c.voiceProcessing = true
	c.voiceLock.Unlock()
	submitted, failed := false, false
	wavPath := ""
	defer func() {
		c.voiceLock.Lock()
		c.voiceProcessing = false
		c.voiceLock.Unlock()
		c.voiceLiveText = ""
		c.voiceInvalidate()
		if wavPath != "" {
			if fi, err := os.Stat(wavPath); err == nil && fi.Mode().IsRegular() {
				if failed {
					cprint(voiceDim(c.t("cli.voice.recording_preserved", map[string]any{"path": wavPath})))
				} else {
					_ = os.Remove(wavPath)
				}
			}
		}
		stopRestart := false
		hold := c.agentRunning || (c.voiceTTSDone != nil && !c.voiceTTSDone.IsSet())
		if submitted {
			c.voiceExtra.noSpeechCount = 0
		} else if !hold {
			c.voiceExtra.noSpeechCount++
			if c.voiceExtra.noSpeechCount >= 3 {
				c.voiceContinuous = false
				c.voiceExtra.noSpeechCount = 0
				cprint(voiceDim(c.t("cli.voice.no_speech_stop", nil)))
				stopRestart = true
			}
		}
		if c.voiceContinuous && !submitted && !c.voiceRecording && !stopRestart {
			c.voiceRestartRecordingAsync()
		}
	}()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		if c.voiceRecorder == nil {
			return nil
		}
		path, err := c.voiceRecorder.(media.VoiceRecorder).Stop()
		if err != nil {
			return err
		}
		wavPath = path
		c.voiceBeep(660, 2)
		if wavPath == "" {
			cprint(voiceDim(c.t("cli.voice.no_speech", nil)))
			return nil
		}
		c.voiceInvalidate()
		model := c.voiceSTTModel()
		if c.voiceSTTProvider() == "local" {
			cprint(voiceDim(c.t("cli.voice.preparing_stt", map[string]any{"model": model})))
		} else {
			cprint(voiceDim(c.t("cli.voice.transcribing", nil)))
		}
		result, err := voiceTranscribeRecording(c.ctx, wavPath, model)
		if err != nil {
			return err
		}
		transcript := pystr.Strip(cliStr(result["transcript"], ""))
		switch {
		case pyrepr.Truthy(result["success"]) && transcript != "":
			if media.IsVoiceStopPhrase(transcript) {
				cprint(voiceDim(c.t("cli.voice.stop_phrase", nil)))
				c.disableVoiceMode()
				return nil
			}
			c.attachedImages = nil
			c.voiceInvalidate()
			c.pendingInput.Put(&voiceInputMessage{Text: transcript})
			submitted = true
		case pyrepr.Truthy(result["success"]):
			cprint(voiceDim(c.t("cli.voice.no_speech", nil)))
		default:
			errText := pyrepr.Or(result["error"], c.t("cli.shared.unknown_error", nil))
			cprint("\n" + voiceDim(c.t("cli.voice.transcription_failed", map[string]any{"error": errText})))
			failed = true
		}
		return nil
	}()
	if err != nil {
		cprint("\n" + voiceDim(c.t("cli.voice.processing_error", map[string]any{"error": pyrepr.ExceptionMessage(err)})))
		failed = wavPath != ""
	}
}

// Schedules TTS of a reply and arms the full duplex listener in continuous mode
func (c *CLI) voiceSpeakResponseAsync(text string) {
	if !c.voiceTTS || text == "" {
		return
	}
	c.voiceTTSDone.Clear()
	go c.voiceSpeakResponse(text)
	if c.voiceContinuous {
		go c.voiceFullDuplexListener()
	}
}

var (
	ttsFenceRe   = regexp.MustCompile("```[\\s\\S]*?```")
	ttsLinkRe    = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	ttsURLRe     = regexp.MustCompile(`https?://\S+`)
	ttsBoldRe    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	ttsItalicRe  = regexp.MustCompile(`\*(.+?)\*`)
	ttsCodeRe    = regexp.MustCompile("`(.+?)`")
	ttsHeaderRe  = regexp.MustCompile(`(?m)^#+\s*`)
	ttsListRe    = regexp.MustCompile(`(?m)^\s*[-*]\s+`)
	ttsRuleRe    = regexp.MustCompile(`---+`)
	ttsNewlineRe = regexp.MustCompile(`\n{3,}`)
)

// Markdown stripped speech text, the fallback when the shared normalizer fails
func ttsFallbackText(text string) string {
	out := ttsFenceRe.ReplaceAllString(text, " ")
	out = ttsLinkRe.ReplaceAllString(out, "$1")
	out = ttsURLRe.ReplaceAllString(out, "")
	out = ttsBoldRe.ReplaceAllString(out, "$1")
	out = ttsItalicRe.ReplaceAllString(out, "$1")
	out = ttsCodeRe.ReplaceAllString(out, "$1")
	out = ttsHeaderRe.ReplaceAllString(out, "")
	out = ttsListRe.ReplaceAllString(out, "")
	out = ttsRuleRe.ReplaceAllString(out, "")
	out = ttsNewlineRe.ReplaceAllString(out, "\n\n")
	return pystr.Strip(out)
}

// Speaks a reply through TTS and plays every produced file, marking TTS done at the end
func (c *CLI) voiceSpeakResponse(text string) {
	if !c.voiceTTS {
		return
	}
	c.voiceTTSDone.Clear()
	defer c.voiceTTSDone.Set()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		ttsText := ""
		func() {
			defer func() {
				if r := recover(); r != nil {
					ttsText = ttsFallbackText(text)
				}
			}()
			ttsText = tools.PrepareSpokenText(text, nil)
		}()
		if ttsText == "" {
			return nil
		}
		c.voiceLastTTSText = ttsText
		outDir := filepath.Join(os.TempDir(), "icebath_voice")
		if err := os.MkdirAll(outDir, 0o777); err != nil {
			return err
		}
		mp3Path := filepath.Join(outDir, "tts_"+time.Now().Format("20060102_150405")+".mp3")
		raw, err := voiceTextToSpeech(c.ctx, ttsText, mp3Path)
		if err != nil {
			return err
		}
		var result map[string]any
		if json.Unmarshal([]byte(raw), &result) != nil || result == nil {
			result = map[string]any{}
		}
		var playPaths []string
		if paths, ok := result["file_paths"].([]any); ok && len(paths) > 0 {
			for _, p := range paths {
				playPaths = append(playPaths, pyrepr.Str(p))
			}
		} else {
			playPaths = []string{pyrepr.Str(pyrepr.Or(result["file_path"], mp3Path))}
		}
		if pyrepr.Truthy(result["success"]) {
			for _, p := range playPaths {
				if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Size() > 0 {
					voicePlayAudioFile(p)
				}
			}
		}
		seen := map[string]bool{}
		for _, p := range append(playPaths, mp3Path, strings.TrimSuffix(mp3Path, ".mp3")+".ogg") {
			if !seen[p] {
				seen[p] = true
				voiceUnlinkQuietly(p)
			}
		}
		return nil
	}()
	if err != nil {
		cliLogger.Warn(fmt.Sprintf("Voice TTS playback failed: %s", pyrepr.ExceptionMessage(err)))
		cprint(voiceDim(c.t("cli.voice.tts_failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})))
	}
}

// Keeps the mic live for the whole agent turn, an interjection interrupting generation or cutting playback
func (c *CLI) voiceFullDuplexListener() {
	c.voiceLock.Lock()
	if c.voiceExtra.fdActive == nil {
		c.voiceExtra.fdActive = newSyncEvent()
	}
	fd := c.voiceExtra.fdActive
	if fd.IsSet() {
		c.voiceLock.Unlock()
		return
	}
	fd.Set()
	c.voiceLock.Unlock()
	defer fd.Clear()
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		cfg, err := config.LoadContext(c.ctx, home.Dir(c.ctx), nil)
		if err != nil {
			return err
		}
		rawVoice := cfg["voice"]
		if !pyrepr.Truthy(rawVoice) {
			rawVoice = map[string]any{}
		}
		voiceCfg, isDict := rawVoice.(map[string]any)
		if !isDict {
			return nil
		}
		if bargeIn, ok := voiceCfg["barge_in"]; ok && !pyrepr.Truthy(bargeIn) {
			return nil
		}
		mult := 0.0
		if raw := voiceCfg["barge_in_threshold_multiplier"]; pyrepr.Truthy(raw) {
			if f, ferr := pyint.Float(raw); ferr == nil {
				mult = f
			}
		}
		graceMS := 500
		if raw, ok := voiceCfg["barge_in_grace_seconds"]; ok {
			if f, ferr := pyint.Float(raw); ferr == nil {
				graceMS = int(f * 1000)
			}
		}
		ttsDone := c.voiceTTSDone
		shouldStop := func() bool {
			if !(c.voiceMode && c.voiceContinuous) {
				return true
			}
			if c.agentRunning {
				return false
			}
			if ttsDone != nil && !ttsDone.IsSet() {
				return false
			}
			return !voiceIsAudioOutputActive()
		}
		onTrigger := func(phase string) {
			c.voiceBargeCapture.Set()
			c.voiceBargePhase = phase
			pipeStop := c.voiceTTSStop
			if phase == "playback" {
				cliLogger.Debug("TTS CUT: full-duplex listener tripped during playback")
				media.MarkSpeechInterrupted()
				if pipeStop != nil {
					pipeStop.Set()
				}
				voiceStopPlayback()
				return
			}
			cliLogger.Debug("full-duplex listener tripped during generation — interrupting agent turn")
			if pipeStop != nil {
				pipeStop.Set()
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						cliLogger.Debug(fmt.Sprintf("voice interjection interrupt failed: %s", errorText(r)))
					}
				}()
				if a := c.agentRef(); a != nil && c.agentRunning {
					cprint("\n" + voiceDim(c.t("cli.voice.interjection", nil)))
					a.Interrupt()
				}
			}()
		}
		opts := media.DefaultDuplexOptions()
		opts.IsPlaying, opts.OnTrigger, opts.Multiplier, opts.GraceMS = voiceIsAudioOutputActive, onTrigger, mult, max(0, graceMS)
		wavPath := voiceFullDuplexListen(shouldStop, opts)
		if wavPath != "" && c.voiceBargeCapture.IsSet() {
			c.voiceSubmitBargeUtterance(wavPath)
		} else {
			c.voiceBargeCapture.Clear()
		}
		return nil
	}()
	if err != nil {
		c.voiceBargeCapture.Clear()
		cliLogger.Debug(fmt.Sprintf("Voice full-duplex listener failed: %s", pyrepr.ExceptionMessage(err)))
	}
}

// Transcribes a barge captured interruption and queues it as the next turn, dropping TTS echo
func (c *CLI) voiceSubmitBargeUtterance(wavPath string) {
	submitted := false
	defer func() {
		voiceUnlinkQuietly(wavPath)
		c.voiceBargeCapture.Clear()
		c.voiceBargePhase = ""
		if !submitted && c.voiceMode && c.voiceContinuous && !c.voiceRecording {
			c.voiceRestartRecordingAsync()
		}
	}()
	result, err := voiceTranscribeRecording(c.ctx, wavPath, c.voiceSTTModel())
	if err != nil {
		cprint("\n" + voiceDim(c.t("cli.voice.processing_error", map[string]any{"error": pyrepr.ExceptionMessage(err)})))
		return
	}
	transcript := ""
	if pyrepr.Truthy(result["success"]) {
		transcript = pystr.Strip(cliStr(result["transcript"], ""))
	}
	if transcript != "" {
		if media.IsVoiceStopPhrase(transcript) {
			cprint("\n" + voiceDim(c.t("cli.voice.stop_phrase", nil)))
			c.disableVoiceMode()
			return
		}
		if c.voiceBargePhase == "playback" && media.IsTTSEcho(transcript, c.voiceLastTTSText) {
			cliLogger.Debug(fmt.Sprintf("Dropping playback-phase barge transcript as TTS echo: %s", pyrepr.Repr(transcript)))
			cprint("\n" + voiceDim(c.t("cli.voice.echo_ignored", nil)))
			return
		}
		c.pendingInput.Put(&voiceInputMessage{Text: transcript})
		submitted = true
	} else if !pyrepr.Truthy(result["success"]) {
		errText := pyrepr.Or(result["error"], c.t("cli.shared.unknown_error", nil))
		cprint("\n" + voiceDim(c.t("cli.voice.transcription_failed", map[string]any{"error": errText})))
	}
}

// Whether voice mode plays record start and stop beeps, voice.beep_enabled defaulting on
func (c *CLI) voiceBeepsEnabled() bool {
	v, ok := voiceConfigSection(c.ctx, "voice")["beep_enabled"]
	if !ok || v == nil {
		return true
	}
	return provider.IsTruthyValue(v)
}

// Enables voice mode after the audio environment and requirement checks
func (c *CLI) enableVoiceMode() {
	if c.voiceMode {
		cprint(voiceDim(c.t("cli.voice.already_enabled", nil)))
		return
	}
	envCheck, err := voiceDetectAudioEnvironment()
	if err != nil {
		panic(err)
	}
	if !envCheck.Available {
		cprint("\n" + accentANSI.String() + c.t("cli.voice.unavailable_header", nil) + resetANSI)
		for _, w := range envCheck.Warnings {
			cprint("  " + voiceDim(w))
		}
		return
	}
	reqs, err := voiceCheckRequirements(c.ctx)
	if err != nil {
		panic(err)
	}
	if !reqs.Available {
		cprint("\n" + accentANSI.String() + c.t("cli.voice.requirements_header", nil) + resetANSI)
		for _, line := range strings.Split(reqs.Details, "\n") {
			cprint("  " + voiceDim(line))
		}
		if !reqs.AudioAvailable && hostinfo.IsTermux() {
			cprint("\n  " + boldANSI + c.t("cli.voice.termux_install_cmd", nil) + resetANSI)
			cprint("  " + voiceDim(c.t("cli.voice.termux_install_hint", nil)))
		}
		return
	}
	c.voiceLock.Lock()
	c.voiceMode = true
	c.voiceLock.Unlock()
	if pyrepr.Truthy(voiceConfigSection(c.ctx, "voice")["auto_tts"]) {
		c.voiceLock.Lock()
		c.voiceTTS = true
		c.voiceLock.Unlock()
	}
	ttsStatus := ""
	if c.voiceTTS {
		ttsStatus = c.t("cli.voice.tts_enabled_suffix", nil)
		c.ttsLeaseAsync(true)
	}
	cprint("\n" + accentANSI.String() + c.t("cli.voice.enabled", map[string]any{"tts_status": ttsStatus}) + resetANSI)
	cprint("  " + voiceDim(c.t("cli.voice.record_key_hint", map[string]any{"shortcut": c.voiceRecordKeyLabel()})))
	stopHint := ""
	probeCall(func() { stopHint = media.VoiceStopHint() })
	if stopHint != "" {
		cprint("  " + voiceDim(stopHint))
	}
	cprint("  " + voiceDim(c.t("cli.voice.tts_toggle_hint", nil)))
	cprint("  " + voiceDim(c.t("cli.voice.disable_hint", nil)))
}

// Ends an active voice chat on a typed bare stop phrase, true when it did
func (c *CLI) typedVoiceStop(userInput any) bool {
	text, ok := userInput.(string)
	if !ok {
		return false
	}
	c.voiceLock.Lock()
	voiceOn := c.voiceMode || c.voiceContinuous
	c.voiceLock.Unlock()
	if !voiceOn {
		return false
	}
	isStop := false
	probeCall(func() { isStop = media.IsVoiceStopPhrase(text) })
	if !isStop {
		return false
	}
	cprint("\n" + voiceDim(c.t("cli.voice.stop_phrase_typed", nil)))
	c.disableVoiceMode()
	return true
}

// Disables voice mode, cancelling any recording and stopping TTS
func (c *CLI) disableVoiceMode() {
	c.voiceLock.Lock()
	if c.voiceRecording && c.voiceRecorder != nil {
		c.voiceRecorder.(media.VoiceRecorder).Cancel()
		c.voiceRecording = false
	}
	recorder := c.voiceRecorder
	c.voiceMode, c.voiceTTS, c.voiceContinuous = false, false, false
	c.voiceLock.Unlock()
	c.ttsLeaseAsync(false)
	if recorder != nil {
		go probeCall(func() { recorder.(media.VoiceRecorder).Shutdown() })
		c.voiceRecorder = nil
	}
	probeCall(func() {
		if c.voiceTTSStop != nil {
			cliLogger.Info("TTS CUT: _disable_voice_mode setting stop event")
			c.voiceTTSStop.Set()
		}
		voiceStopPlayback()
	})
	c.voiceTTSDone.Set()
	cprint("\n" + voiceDim(c.t("cli.voice.disabled", nil)))
}

// Starts the wake word listener at startup when this surface is eligible
func (c *CLI) maybeStartWakeWord() error {
	enabled := false
	probeCall(func() { enabled = media.WakeSurfaceEnabled(c.ctx, "cli", nil) })
	if !enabled {
		return nil
	}
	c.startWakeWordListener(true)
	return nil
}

// Builds and starts the hotword detector, true on success
func (c *CLI) startWakeWordListener(announce bool) bool {
	say := func(text string) {
		if announce {
			cprint(text)
		}
	}
	if c.voiceExtra.wakeWordActive && media.OwnsListener(c) {
		say(voiceDim(c.t("cli.voice.wake_already", nil)))
		return true
	}
	c.voiceExtra.wakeWordActive = false
	cfg := media.LoadWakeWordConfig(c.ctx)
	reqs, err := media.CheckWakeWordRequirements(c.ctx, cfg, nil)
	if err != nil {
		say(voiceDim(c.t("cli.voice.wake_unavailable", map[string]any{"error": pyrepr.ExceptionMessage(err)})))
		return false
	}
	if !reqs.Available {
		say("\n" + accentANSI.String() + c.t("cli.voice.wake_requirements_header", nil) + resetANSI)
		if reqs.Hint != "" {
			say("  " + voiceDim(reqs.Hint))
		}
		return false
	}
	if !reqs.DepsAvailable {
		say(voiceDim(c.t("cli.voice.wake_installing", nil)))
	}
	startNew, ok := cfg["start_new_session"]
	c.voiceExtra.wakeStartNewSession = !ok || pyrepr.Truthy(startNew)
	if _, err := media.StartListening(c.ctx, c.onWakeWord, c, cfg, false); err != nil {
		say("\n" + voiceDim(c.t("cli.voice.wake_start_failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})))
		return false
	}
	c.voiceExtra.wakeWordActive = true
	c.voiceExtra.wakeSuspended = false
	cliWakeOwner = c
	c.startWakeWatchdog()
	say("\n" + accentANSI.String() + c.t("cli.voice.wake_listening", nil) + resetANSI + " " +
		voiceDim(c.t("cli.voice.wake_listening_hint", map[string]any{"phrase": reqs.Phrase})))
	return true
}

// Stops and tears down the hotword detector
func (c *CLI) stopWakeWordListener(announce bool) {
	wasActive := c.voiceExtra.wakeWordActive
	c.voiceExtra.wakeWordActive = false
	c.voiceExtra.wakeSuspended = false
	probeCall(func() { media.StopListening(c) })
	if cliWakeOwner == c {
		cliWakeOwner = nil
	}
	if announce {
		key := "cli.voice.wake_not_running"
		if wasActive {
			key = "cli.voice.wake_stopped"
		}
		cprint(voiceDim(c.t(key, nil)))
	}
}

// Handles a heard wake phrase, pausing the detector and starting a capture
func (c *CLI) onWakeWord() error {
	if c.shouldExit || c.agentRunning || c.voiceRecording || c.voiceProcessing {
		return nil
	}
	if !media.PauseListening(c) {
		c.voiceExtra.wakeWordActive = false
		return nil
	}
	c.voiceExtra.wakeSuspended = true
	if _, profile, ok := media.GetLastMatch(); ok && profile != "" && profile != media.WakeActiveProfileName(c.ctx) {
		cprint("\n" + voiceDim(c.t("cli.voice.wake_profile_hint", map[string]any{"profile": profile})))
		c.voiceExtra.wakeSuspended = true
		return nil
	}
	cprint("\n" + accentANSI.String() + c.t("cli.voice.wake_detected", nil) + resetANSI)
	if c.app != nil {
		_ = c.app.Invalidate()
	}
	if c.voiceExtra.wakeStartNewSession {
		probeCall(func() { c.newSession(true, "") })
	}
	c.voiceLock.Lock()
	c.voiceMode = true
	c.voiceLock.Unlock()
	c.voiceContinuous = false
	if err := c.voiceStartRecording(); err != nil {
		cprint(voiceDim(c.t("cli.voice.wake_capture_failed", map[string]any{"error": pyrepr.ExceptionMessage(err)})))
	}
	return nil
}

// Resumes the paused detector once the CLI is idle for three polls
func (c *CLI) startWakeWatchdog() {
	if c.voiceExtra.wakeWatchdogStarted {
		return
	}
	c.voiceExtra.wakeWatchdogStarted = true
	go func() {
		defer func() { c.voiceExtra.wakeWatchdogStarted = false }()
		idle := 0
		for c.voiceExtra.wakeWordActive && !c.shouldExit {
			time.Sleep(250 * time.Millisecond)
			if !c.voiceExtra.wakeSuspended {
				idle = 0
				continue
			}
			if c.agentRunning || c.voiceRecording || c.voiceProcessing || !c.pendingInput.Empty() {
				idle = 0
				continue
			}
			idle++
			if idle >= 3 {
				idle = 0
				resumed, err := media.ResumeListening(c)
				switch {
				case err != nil:
					cliLogger.Debug(fmt.Sprintf("wake word resume failed: %s", pyrepr.ExceptionMessage(err)))
				case resumed:
					c.voiceExtra.wakeSuspended = false
				default:
					c.voiceExtra.wakeWordActive = false
				}
			}
		}
	}()
}

// Shows the wake word listener state, phrase, provider, surface and new session setting
func (c *CLI) showWakeWordStatus() {
	cfg := media.LoadWakeWordConfig(c.ctx)
	reqs, err := media.CheckWakeWordRequirements(c.ctx, cfg, nil)
	if err != nil {
		panic(err)
	}
	owned := media.OwnsListener(c)
	state := "off"
	if owned && media.IsListening() {
		state = "listening"
	} else if owned {
		state = "paused"
	}
	cprint("\n" + boldANSI + c.t("cli.voice.wake_status_title", nil) + resetANSI)
	cprint(c.t("cli.voice.label_state", map[string]any{"state": c.t("cli.voice.state_"+state, nil)}))
	cprint(c.t("cli.voice.label_phrase", map[string]any{"phrase": reqs.Phrase}))
	cprint(c.t("cli.voice.label_provider", map[string]any{"provider": reqs.Provider}))
	surface, ok := cfg["surface"]
	if !ok {
		surface = "auto"
	}
	cprint(c.t("cli.voice.label_surface", map[string]any{"surface": surface}))
	yes := "cli.shared.label_no"
	if v, ok := cfg["start_new_session"]; !ok || pyrepr.Truthy(v) {
		yes = "cli.shared.label_yes"
	}
	cprint(c.t("cli.voice.label_new_session", map[string]any{"value": c.t(yes, nil)}))
	if state == "listening" && media.AudioIsSilent() {
		cprint("  " + accentANSI.String() + c.t("cli.voice.mic_silent_warning", nil) + resetANSI)
		cprint("  " + voiceDim(c.t("cli.voice.mic_silent_macos_hint", nil)))
	}
	if !reqs.Available && reqs.Hint != "" {
		cprint("  " + voiceDim(reqs.Hint))
	}
	if !owned {
		cprint("  " + voiceDim(c.t("cli.voice.wake_enable_hint", nil)))
	}
}

// Acquires or releases this CLI's TTS engine lease on a goroutine, never failing the toggle
func (c *CLI) ttsLeaseAsync(active bool) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				cliLogger.Debug(fmt.Sprintf("voice: tts lease active=%v failed: %v", pyrepr.Str(active), r))
			}
		}()
		if active {
			media.AcquireTTSLease(c.ctx, c.env, "cli:voice-tts", nil)
		} else {
			media.ReleaseTTSLease(c.ctx, c.env, "cli:voice-tts")
		}
	}()
}

// Toggles TTS output of voice mode
func (c *CLI) toggleVoiceTTS() {
	if !c.voiceMode {
		cprint(voiceDim(c.t("cli.voice.enable_first_hint", nil)))
		return
	}
	c.voiceLock.Lock()
	c.voiceTTS = !c.voiceTTS
	on := c.voiceTTS
	c.voiceLock.Unlock()
	status := c.t("cli.shared.label_disabled", nil)
	if on {
		status = c.t("cli.shared.label_enabled", nil)
		if !media.CheckTTSRequirements(c.ctx) {
			cprint(voiceDim(c.t("cli.voice.tts_no_provider", nil)))
		}
	}
	c.ttsLeaseAsync(on)
	cprint(accentANSI.String() + c.t("cli.voice.tts_status", map[string]any{"status": status}) + resetANSI)
}

// Shows voice mode, TTS, recording, the record key and the requirement details
func (c *CLI) showVoiceStatus() {
	reqs, err := voiceCheckRequirements(c.ctx)
	if err != nil {
		panic(err)
	}
	on, off := c.t("cli.voice.state_on", nil), c.t("cli.voice.state_off", nil)
	pick := func(b bool) string {
		if b {
			return on
		}
		return off
	}
	cprint("\n" + boldANSI + c.t("cli.voice.status_title", nil) + resetANSI)
	cprint(c.t("cli.voice.label_mode", map[string]any{"value": pick(c.voiceMode)}))
	cprint(c.t("cli.voice.label_tts", map[string]any{"value": pick(c.voiceTTS)}))
	rec := c.t("cli.shared.label_no", nil)
	if c.voiceRecording {
		rec = c.t("cli.voice.state_yes", nil)
	}
	cprint(c.t("cli.voice.label_recording", map[string]any{"value": rec}))
	cprint(c.t("cli.voice.label_record_key", map[string]any{"shortcut": c.voiceRecordKeyLabel()}))
	cprint("\n  " + boldANSI + c.t("cli.voice.label_requirements", nil) + resetANSI)
	for _, line := range strings.Split(reqs.Details, "\n") {
		cprint("    " + line)
	}
}

// Handles /voice on, off, tts or status, bare toggling
func (c *CLI) handleVoiceCommand(command string) {
	sub := commandArg(command, true)
	if sub == "" {
		sub = "on"
		if c.voiceMode {
			sub = "off"
		}
	}
	switch sub {
	case "on":
		c.enableVoiceMode()
	case "off":
		c.disableVoiceMode()
	case "tts":
		c.toggleVoiceTTS()
	case "status":
		c.showVoiceStatus()
	default:
		cp(c.commandsT("voice.unknown_subcommand", map[string]any{"subcommand": sub}), c.commandsT("voice.usage", nil))
	}
}

// Handles /wake on, off or status, the toggle persisted as wake_word.enabled
func (c *CLI) handleWakeCommand(command string) {
	sub := commandArg(command, true)
	if sub == "" {
		sub = "on"
		if c.voiceExtra.wakeWordActive {
			sub = "off"
		}
	}
	switch sub {
	case "on":
		if c.startWakeWordListener(true) {
			c.persistWakeWordEnabled(true)
		}
	case "off":
		c.stopWakeWordListener(true)
		c.persistWakeWordEnabled(false)
	case "status":
		c.showWakeWordStatus()
	default:
		cp(c.commandsT("wake.unknown_subcommand", map[string]any{"subcommand": sub}), c.commandsT("wake.usage", nil))
	}
}

// Saves wake_word.enabled so the toggle sticks, skipped when already persisted
func (c *CLI) persistWakeWordEnabled(enabled bool) {
	var persisted map[string]any
	probeCall(func() { persisted = media.LoadWakeWordConfig(c.ctx) })
	if persisted != nil && pyrepr.Truthy(persisted["enabled"]) == enabled {
		return
	}
	if saveConfigValue(c.ctx, "wake_word.enabled", enabled) {
		key := "wake.disabled_in_config"
		if enabled {
			key = "wake.enabled_in_config"
		}
		cp(dimText(c.commandsT(key, nil)))
	}
}
