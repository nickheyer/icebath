package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Outbound dispatch partition of video files for MEDIA and local file delivery
var videoExts = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}

var (
	ttsThinkBlockRE = regexp.MustCompile(`(?s)<think[\s>].*?</think>`)
	ttsMarkupRE     = regexp.MustCompile("[*_`#\\[\\]()]")
)

// Declared PCM format of a streaming TTS session, raw little endian samples at this shape
type AudioFormat struct {
	SampleRate  int
	Channels    int
	SampleWidth int
}

// Mono 24 kHz int16 PCM, the dataclass defaults
func DefaultAudioFormat() AudioFormat {
	return AudioFormat{SampleRate: 24000, Channels: 1, SampleWidth: 2}
}

// Handle of one streaming TTS session with the audible and aborted bookkeeping
type StreamingTTSHandle struct {
	ChatID      string
	AudioFormat AudioFormat
	Audible     bool
	Aborted     bool
}

// Handle for the chat with the default audio format, the dataclass defaults
func NewStreamingTTSHandle(chatID string) *StreamingTTSHandle {
	return &StreamingTTSHandle{ChatID: chatID, AudioFormat: DefaultAudioFormat()}
}

// Turn scoped suppression key from the run generation or else the event message or update id
func StreamingTTSTurnKey(sessionKey string, turnMarker any, event *MessageEvent) (string, bool) {
	if sessionKey == "" {
		return "", false
	}
	if turnMarker == nil && event != nil {
		switch {
		case event.MessageID != nil && *event.MessageID != "":
			turnMarker = *event.MessageID
		case event.PlatformUpdateID != nil:
			turnMarker = *event.PlatformUpdateID
		}
	}
	if turnMarker == nil {
		return "", false
	}
	return sessionKey + ":" + pyrepr.Str(turnMarker), true
}

// Whether the turn already streamed its audio so the whole file auto TTS is skipped
func StreamingTTSShouldSkipWholeFile(completedTurns map[string]struct{}, sessionKey string, turnMarker any, event *MessageEvent) bool {
	key, ok := StreamingTTSTurnKey(sessionKey, turnMarker, event)
	if !ok {
		return false
	}
	_, done := completedTurns[key]
	return done
}

// Friendly notice for a media kind the platform cannot send natively, never echoing the host path
func (b *BasePlatformAdapter) sendMediaFallbackNotice(ctx context.Context, method, kind, path, chatID, caption, replyTo string, metadata map[string]any, fileName string) (SendResult, error) {
	slog.Warn(fmt.Sprintf("[%s] %s fallback: native %s send unavailable for %s", b.Self().Name(), method, kind, path))
	text := mediaFailureText(ctx, kind, fileName)
	return b.Self().EmitMediaWarning(ctx, chatID, text, caption, replyTo, metadata, metadata)
}

// Classified channel diagnostic sent in the caller's scope, nil when the warning policy hides it
func (b *BasePlatformAdapter) EmitWarning(ctx context.Context, chatID, content, replyTo string, metadata map[string]any, logicalPlatform Platform) (*SendResult, error) {
	self := b.Self()
	if !self.WarningNotificationsEnabled(ctx, logicalPlatform, chatID, metadata) {
		return nil, nil
	}
	result, err := self.Send(ctx, chatID, content, replyTo, metadata)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Media diagnostic that keeps the caption, a failed result carrying the notice when the warning is hidden
func (b *BasePlatformAdapter) EmitMediaWarning(ctx context.Context, chatID, notice, caption, replyTo string, metadata, shownMetadata map[string]any) (SendResult, error) {
	self := b.Self()
	content := notice
	if caption != "" {
		content = caption + "\n" + notice
	}
	result, err := self.EmitWarning(ctx, chatID, content, replyTo, shownMetadata, "")
	if err != nil {
		return SendResult{}, err
	}
	if result != nil {
		return *result, nil
	}
	if caption != "" {
		if _, err := self.Send(ctx, chatID, caption, replyTo, metadata); err != nil {
			return SendResult{}, err
		}
	}
	return SendResult{Success: false, Error: notice}, nil
}

// Diagnostic variant when warnings show, else the requested remainder
func (b *BasePlatformAdapter) WarningText(ctx context.Context, visible string, hidden *string, logicalPlatform Platform, chatID string, metadata map[string]any) *string {
	if b.Self().WarningNotificationsEnabled(ctx, logicalPlatform, chatID, metadata) {
		return &visible
	}
	return hidden
}

// Warning presentation policy of the logical platform, the adapter platform when none is given
func (b *BasePlatformAdapter) WarningNotificationsEnabled(ctx context.Context, logicalPlatform Platform, chatID string, metadata map[string]any) bool {
	if logicalPlatform == "" {
		logicalPlatform = b.Platform
	}
	return config.WarningNotificationsEnabled(ctx, string(logicalPlatform), nil)
}

// Spoken script of chat markdown, a markup strip when the normalizer fails
func (b *BasePlatformAdapter) PrepareTTSText(ctx context.Context, text string) string {
	spoken, err := prepareSpokenText(ctx, text, nil)
	if err == nil {
		return spoken
	}
	text = ttsThinkBlockRE.ReplaceAllString(text, " ")
	return pystr.Strip(ttsMarkupRE.ReplaceAllString(text, ""))
}

// Plays auto TTS audio as a voice message, adapters with invisible playback override it
func (b *BasePlatformAdapter) PlayTTS(ctx context.Context, chatID, audioPath, caption, replyTo string, metadata, extra map[string]any) (SendResult, error) {
	return b.Self().SendVoice(ctx, chatID, audioPath, caption, replyTo, metadata, extra)
}

// Whether the adapter accepts streaming PCM for the chat, never by default
func (b *BasePlatformAdapter) SupportsStreamingTTS(chatID string, format AudioFormat) bool {
	return false
}

// Opens a streaming audio session, nil declines so the whole file fallback runs
func (b *BasePlatformAdapter) BeginStreamingTTS(ctx context.Context, chatID string, format AudioFormat, metadata map[string]any) (*StreamingTTSHandle, error) {
	return nil, nil
}

// Writes one PCM chunk to the outbound audio track, nothing by default
func (b *BasePlatformAdapter) WriteStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, chunk []byte) error {
	return nil
}

// Signals the normal end of the audio stream, nothing by default
func (b *BasePlatformAdapter) FinishStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, interrupted bool) error {
	return nil
}

// Aborts the audio stream idempotently, nothing by default
func (b *BasePlatformAdapter) AbortStreamingTTS(ctx context.Context, handle *StreamingTTSHandle, errText *string) error {
	return nil
}

// Turn scoped streaming TTS key of the session and turn
func (b *BasePlatformAdapter) streamingTTSTurnKey(sessionKey string, turnMarker any, event *MessageEvent) (string, bool) {
	return StreamingTTSTurnKey(sessionKey, turnMarker, event)
}

// Records that the turn streamed its audio
func (b *BasePlatformAdapter) markStreamingTTSCompletedTurn(sessionKey string, turnMarker any, event *MessageEvent) {
	if key, ok := b.streamingTTSTurnKey(sessionKey, turnMarker, event); ok {
		b.mu.Lock()
		lazyAttr(&b.streamingTTSCompletedTurns)[key] = struct{}{}
		b.mu.Unlock()
	}
}

// Whether the turn already streamed its audio
func (b *BasePlatformAdapter) streamingTTSTurnCompleted(sessionKey string, turnMarker any, event *MessageEvent) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return StreamingTTSShouldSkipWholeFile(b.streamingTTSCompletedTurns, sessionKey, turnMarker, event)
}

// User visible notice that a MEDIA upload failed after its tag was stripped from the text
func (b *BasePlatformAdapter) notifyMediaDeliveryFailure(ctx context.Context, chatID, mediaPath string, isVoice bool, metadata map[string]any) {
	ext := pystr.Lower(pypath.PathSuffix(mediaPath))
	var text string
	switch {
	case isVoice || ShouldSendMediaAsAudio(b.Platform, ext, isVoice):
		text = mediaFailureText(ctx, "audio", "")
	case videoExts[ext]:
		text = mediaFailureText(ctx, "video", "")
	default:
		text = mediaFailureText(ctx, "file", pypath.Basename(mediaPath))
	}
	var problem any
	notice, err := b.Self().EmitWarning(ctx, chatID, text, "", metadata, "")
	switch {
	case err != nil:
		problem = err
	case notice != nil && !notice.Success:
		problem = notice.Error
	}
	if problem != nil {
		slog.Debug(fmt.Sprintf("[%s] Could not send media-delivery-failure notice: %v", b.Self().Name(), problem))
	}
}

// Spoken form of reply text for TTS through the tool lane normalizer, nil max chars meaning uncapped
func prepareSpokenText(ctx context.Context, text string, maxChars *int) (string, error) {
	return tools.PrepareSpokenText(text, maxChars), nil
}
