package gatewayrun

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/media"
	"github.com/nickheyer/icebath/pkg/tools"
)

const emptyContentText = "(The user sent a message with no text content)"

// Configured speech to text call and its local fallback
type (
	sttTranscribe         func(ctx context.Context, filePath, model, source string) (map[string]any, error)
	sttTranscribeFallback func(ctx context.Context, filePath, model string) (map[string]any, error)
)

// Absolute form of a path, the path itself when the working directory is unknown
func inboundAbspath(path string) string {
	abs, err := pypath.Abspath(path)
	if err != nil {
		return path
	}
	return abs
}

// Transcribes voice clips and prepends the transcripts, returning the enriched text and successful transcripts
func (r *GatewayRunner) enrichMessageWithTranscription(ctx context.Context, userText string, audioPaths []string, clipMemo map[string]gateway.STTClip) (string, []string) {
	seen := map[string]bool{}
	unique := []string{}
	for _, p := range audioPaths {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	audioPaths = unique
	if !r.config.STTEnabled {
		notes := []string{}
		for _, path := range audioPaths {
			absPath := inboundAbspath(path)
			suffix := ""
			if duration, ok := probeAudioDuration(ctx, absPath); ok && duration != "" {
				suffix = fmt.Sprintf(" (duration: %s)", duration)
			}
			notes = append(notes, fmt.Sprintf("[The user sent a voice message: %s%s]", absPath, suffix))
		}
		if len(notes) == 0 {
			return userText, []string{}
		}
		return prependMediaPrefix(strings.Join(notes, "\n\n"), userText), []string{}
	}
	enrichedParts := []string{}
	successful := []string{}
	for _, path := range audioPaths {
		clip, memoized := clipMemo[path]
		if !memoized {
			inboundLog().Debug("Transcribing user voice: " + path)
			var err error
			clip, err = r.transcribeOneClip(ctx, path, media.TranscribeAudio, media.TranscribeAudioLocalFallback)
			if err != nil {
				inboundLog().Error(fmt.Sprintf("Transcription error: %s", err))
				enrichedParts = append(enrichedParts, untranscribedAudioNote(ctx, path))
				continue
			}
			if clipMemo != nil {
				clipMemo[path] = clip
			}
		}
		if clip.Transcript != nil {
			successful = append(successful, *clip.Transcript)
		}
		enrichedParts = append(enrichedParts, clip.Note)
	}
	if len(enrichedParts) > 0 {
		userText = prependMediaPrefix(strings.Join(enrichedParts, "\n\n"), userText)
	}
	return userText, successful
}

// Speech to text eligible paths of a pending voice message
func pendingEventAudioPaths(event *gateway.MessageEvent) []string {
	paths := []string{}
	for i, path := range event.MediaURLs {
		if eventMediaIsSTTInput(event, i) {
			paths = append(paths, path)
		}
	}
	return paths
}

// Caption override when given, else the live event text
func pendingCaption(event *gateway.MessageEvent, userText *string) string {
	if userText != nil {
		return *userText
	}
	return event.Text
}

// Transcribes a pending audio event once and caches the enriched text and transcripts on the event
func (r *GatewayRunner) transcribePendingAudioEventOnce(ctx context.Context, event *gateway.MessageEvent, userText *string) (*string, []string) {
	if text, transcripts, ok := event.PendingSTT(); ok {
		return &text, transcripts
	}
	audioPaths := pendingEventAudioPaths(event)
	if len(audioPaths) == 0 {
		if userText != nil {
			return userText, []string{}
		}
		if event.Text == "" {
			return nil, []string{}
		}
		text := event.Text
		return &text, []string{}
	}
	clipMemo := event.PendingSTTClips()
	enriched, transcripts := r.enrichMessageWithTranscription(ctx, pendingCaption(event, userText), audioPaths, clipMemo)
	for current := pendingEventAudioPaths(event); !reflect.DeepEqual(current, audioPaths); current = pendingEventAudioPaths(event) {
		audioPaths = current
		enriched, transcripts = r.enrichMessageWithTranscription(ctx, pendingCaption(event, userText), audioPaths, clipMemo)
	}
	event.SetPendingSTT(enriched, transcripts)
	return &enriched, append([]string(nil), transcripts...)
}

// True when successful transcripts are echoed back to the chat
func (r *GatewayRunner) shouldEchoSTTTranscripts() bool {
	return r.config.STTEchoTranscripts
}

// Sends each transcript back as a short quoted echo, failures logged and never raised
func (r *GatewayRunner) echoSTTTranscripts(ctx context.Context, adapter gateway.PlatformAdapter, source *gateway.SessionSource, transcripts []string, metadata map[string]any, logContext string) {
	for _, tx := range transcripts {
		echo := i18n.T(ctx, "gateway.voice.transcript_echo_short", "", map[string]any{"text": tx})
		if _, err := adapter.Send(ctx, source.ChatID, echo, "", metadata); err != nil {
			inboundLog().Debug(fmt.Sprintf("%s echo failed (non-fatal): %s", logContext, err))
		}
	}
}

// Echoes the not yet echoed tail of the pending transcripts, at most once per transcript
func (r *GatewayRunner) echoPendingSTTTranscriptsOnce(ctx context.Context, event *gateway.MessageEvent, adapter gateway.PlatformAdapter, source *gateway.SessionSource, transcripts []string, metadata map[string]any, logContext string) {
	if len(transcripts) == 0 || !r.shouldEchoSTTTranscripts() || adapter == nil {
		return
	}
	already := event.PendingSTTEchoed()
	event.SetPendingSTTEchoed(max(already, len(transcripts)))
	if already < len(transcripts) {
		r.echoSTTTranscripts(ctx, adapter, source, transcripts[already:], metadata, logContext)
	}
}

// Transcribes a pending voice event and echoes its transcripts once, the fallback text when nothing is eligible
func (r *GatewayRunner) transcribeAndEchoPendingVoice(ctx context.Context, event *gateway.MessageEvent, adapter gateway.PlatformAdapter, source *gateway.SessionSource, text, logContext string, metadata map[string]any, metadataSet bool) (result string, transcripts []string) {
	if len(pendingEventAudioPaths(event)) == 0 {
		return text, []string{}
	}
	defer func() {
		if rec := recover(); rec != nil {
			inboundLog().Warn(fmt.Sprintf("%s transcription failed: %s", logContext, pyrepr.Str(rec)))
			result, transcripts = text, []string{}
		}
	}()
	enriched, transcripts := r.transcribePendingAudioEventOnce(ctx, event, nil)
	if !metadataSet {
		metadata = r.threadMetadataForSource(source, gateway.ReplyAnchorForEvent(event))
	}
	r.echoPendingSTTTranscriptsOnce(ctx, event, adapter, source, transcripts, metadata, logContext)
	if enriched != nil && *enriched != "" {
		return *enriched, transcripts
	}
	return text, transcripts
}

// Prefix then the user text, dropping the empty content notice of Discord as redundant
func prependMediaPrefix(prefix, userText string) string {
	if userText != "" && pystr.Strip(userText) != emptyContentText {
		return prefix + "\n\n" + userText
	}
	return prefix
}

// Neutral marker for every speech to text failure naming where the audio is available
func untranscribedAudioNote(ctx context.Context, path string) string {
	agentPath := tools.ToAgentVisibleCachePath(inboundToolsEnv(ctx), inboundAbspath(path))
	return fmt.Sprintf("[voice message could not be transcribed automatically; the audio is available at: %s]", agentPath)
}

// Transcript or nil and the note for one clip through the configured provider with the local fallback
func (r *GatewayRunner) transcribeOneClip(ctx context.Context, path string, transcribe sttTranscribe, fallback sttTranscribeFallback) (gateway.STTClip, error) {
	result, err := transcribe(ctx, path, "", "gateway")
	if err != nil {
		return gateway.STTClip{}, err
	}
	if !pyrepr.Truthy(result["success"]) {
		recovered, err := fallback(ctx, path, "")
		if err != nil {
			return gateway.STTClip{}, err
		}
		if pyrepr.Truthy(recovered["success"]) {
			inboundLog().Info(fmt.Sprintf("Configured STT failed for %s; recovered with local STT", path))
			result = recovered
		}
	}
	if !pyrepr.Truthy(result["success"]) {
		reason, ok := result["error"]
		if !ok {
			reason = "unknown error"
		}
		inboundLog().Info(fmt.Sprintf("Voice transcription failed for %s: %s", path, pyrepr.Str(reason)))
		return gateway.STTClip{Note: untranscribedAudioNote(ctx, path)}, nil
	}
	transcript, _ := result["transcript"].(string)
	if pystr.Strip(transcript) == "" {
		return gateway.STTClip{Note: "[The user sent a voice message but it came through " +
			"empty or inaudible — speech-to-text returned no " +
			"words. Do not guess at the content; ask the user " +
			"to resend or type it out.]"}, nil
	}
	return gateway.STTClip{Transcript: &transcript, Note: fmt.Sprintf("\"%s\"", transcript)}, nil
}
