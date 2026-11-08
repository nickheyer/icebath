package gateway

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Uniform draw in [lo, hi] for the human delay, swapped by tests the way hermes tests patch random.uniform
var humanDelayUniform = func(lo, hi float64) float64 { return lo + (hi-lo)*rand.Float64() }

// TTS tool requirement check and synthesis, swapped by tests the way hermes tests patch tools.tts_tool
var (
	autoTTSRequirementsMet = func(ctx context.Context) bool { return checkTTSRequirements(ctx) }
	autoTTSSynthesize      = func(ctx context.Context, text, outputPath string) (string, error) {
		return textToSpeechTool(ctx, text, outputPath)
	}
)

// Whether the configured text to speech provider can run
func checkTTSRequirements(ctx context.Context) bool { return media.CheckTTSRequirements(ctx) }

// Speech synthesized into the output path, the text_to_speech tool result envelope
func textToSpeechTool(ctx context.Context, text, outputPath string) (string, error) {
	return media.TextToSpeechTool(ctx, text, outputPath)
}

// Unique temp path for gateway auto TTS, ogg for Opus voice platforms and mp3 everywhere else
func BuildAutoTTSOutputPath(platform Platform) (string, error) {
	ext := "mp3"
	if opusVoicePlatforms()[platformName(platform)] {
		ext = "ogg"
	}
	var raw [16]byte
	for i := range raw {
		raw[i] = byte(rand.IntN(256))
	}
	audioPath := filepath.Join(os.TempDir(), "icebath_voice", fmt.Sprintf("tts_reply_%s.%s", hex.EncodeToString(raw[:])[:12], ext))
	if err := os.MkdirAll(filepath.Dir(audioPath), 0o777); err != nil {
		return "", err
	}
	return audioPath, nil
}

// Random human like pacing delay in seconds from the installed range, zero when no range is installed
func (b *BasePlatformAdapter) getHumanDelay() float64 {
	b.mu.Lock()
	bounds := b.humanDelayRangeMs
	b.mu.Unlock()
	if bounds == nil {
		return 0
	}
	return humanDelayUniform(float64(bounds[0])/1000.0, float64(bounds[1])/1000.0)
}

// Synthesizes auto TTS audio into existing paths plus the requested path, empty on any logged failure
func (b *BasePlatformAdapter) synthesizeAutoTTS(ctx context.Context, textContent string) ([]string, *string) {
	paths := []string{}
	var requested *string
	fail := func(err error) ([]string, *string) {
		slog.Warn(fmt.Sprintf("[%s] Auto-TTS failed: %v", b.Self().Name(), err))
		return paths, requested
	}
	if !autoTTSRequirementsMet(ctx) {
		return paths, requested
	}
	speech := b.Self().PrepareTTSText(ctx, textContent)
	if speech == "" {
		return fail(errors.New("Empty text after markdown cleanup"))
	}
	out, err := BuildAutoTTSOutputPath(b.Platform)
	if err != nil {
		return fail(err)
	}
	requested = &out
	raw, err := autoTTSSynthesize(ctx, speech, out)
	if err != nil {
		return fail(err)
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return fail(err)
	}
	if success, ok := data["success"]; ok && !pyrepr.Truthy(success) {
		return paths, requested
	}
	candidates, _ := data["file_paths"].([]any)
	if !pyrepr.Truthy(data["file_paths"]) {
		candidates = []any{data["file_path"]}
	}
	for _, p := range candidates {
		if !pyrepr.Truthy(p) {
			continue
		}
		path := pyrepr.Str(p)
		if _, err := os.Stat(path); err == nil {
			paths = append(paths, path)
		}
	}
	return paths, requested
}

// Whether a voice input reply gets auto TTS, gated per chat and skipped when streaming TTS already spoke
func (b *BasePlatformAdapter) wantsAutoTTS(event *MessageEvent, sessionKey string, interruptEvent *InterruptEvent, textContent string, mediaFiles []MediaRef) bool {
	var generation any
	if interruptEvent != nil {
		generation = interruptEvent.RunGeneration()
	}
	return b.shouldAutoTTSForChat(event.Source.ChatID) && event.MessageType == MessageTypeVoice && textContent != "" &&
		len(mediaFiles) == 0 && !b.streamingTTSTurnCompleted(sessionKey, generation, event)
}

// Plays one synthesized TTS file, true when the reply text rode along as the first Telegram caption
func (b *BasePlatformAdapter) playTTSFile(ctx context.Context, event *MessageEvent, textContent, ttsPath string, first bool, metadata map[string]any, recordDelivery func(SendResult)) (bool, error) {
	caption := ""
	if first && b.Platform == PlatformTelegram && textContent != "" && pystr.Slice(textContent, 0, 1024) == textContent {
		caption = textContent
	}
	result, err := b.Self().PlayTTS(ctx, event.Source.ChatID, ttsPath, caption, "", metadata, nil)
	if err != nil {
		return false, err
	}
	recordDelivery(result)
	return caption != "" && result.Success, nil
}

// Platforms whose voice bubbles take Opus, the TTS tool's single source of truth
func opusVoicePlatforms() map[string]bool { return tools.OpusVoicePlatforms }
