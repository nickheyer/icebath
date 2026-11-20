package gatewayrun

import (
	"context"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	relayMediaPrefix       = "relay_media_"
	unknownAttachmentName  = "(original filename unknown)"
	inboundAttachmentChars = `[^\w.\- ]`
)

var inboundAttachmentUnsafe = pyre.MustCompile(inboundAttachmentChars)

// Model facing routing note for a Discord turn naming the triggering message id
func discordTriggeringNote(messageID any) string {
	return fmt.Sprintf("[Triggering message id: `%v` — use as `message_id` for reply/react/pin via the discord tools.]", messageID)
}

// Authored text for the durable user row with the triggering note of this event peeled off
func stripDiscordTriggeringNote(event *gateway.MessageEvent, messageText any) any {
	text, ok := messageText.(string)
	if event == nil || event.MessageID == nil || *event.MessageID == "" || !ok {
		return messageText
	}
	return strings.TrimPrefix(text, discordTriggeringNote(*event.MessageID)+"\n\n")
}

// Tool environment of the active profile scope for host to sandbox path translation
func inboundToolsEnv(ctx context.Context) *tools.Env {
	return &tools.Env{Home: home.Dir(ctx), TerminalScope: tools.GetTerminalScope(ctx)}
}

// Splits the event attachments into image, speech to text voice, audio file and video paths
func classifyInboundMedia(event *gateway.MessageEvent, pendingSTTPrepared bool) (imagePaths, audioPaths, audioFilePaths, videoPaths []string) {
	imagePaths, audioPaths, audioFilePaths, videoPaths = []string{}, []string{}, []string{}, []string{}
	for i, path := range event.MediaURLs {
		mtype := eventMediaTypeAt(event, i)
		if eventMediaIsImage(event, i) {
			imagePaths = append(imagePaths, path)
		}
		if eventMediaIsAudio(event, i) {
			if event.MessageType == gateway.MessageTypeAudio || event.MessageType == gateway.MessageTypeDocument {
				audioFilePaths = append(audioFilePaths, path)
			} else if !pendingSTTPrepared && eventMediaIsSTTInput(event, i) {
				audioPaths = append(audioPaths, path)
			}
		}
		if strings.HasPrefix(mtype, "video/") || (mtype == "" && event.MessageType == gateway.MessageTypeVideo) {
			videoPaths = append(videoPaths, path)
		}
	}
	return imagePaths, audioPaths, audioFilePaths, videoPaths
}

// Display name and agent visible path of an attachment, the original name else the parsed cache name
func inboundAttachmentDisplayName(ctx context.Context, path, originalName string) (string, string) {
	basename := pypath.Basename(path)
	name := gateway.CleanAttachmentFilename(originalName)
	agentPath := tools.ToAgentVisibleCachePath(inboundToolsEnv(ctx), path)
	if name == "" && strings.HasPrefix(basename, relayMediaPrefix) {
		return unknownAttachmentName, agentPath
	}
	if name == "" {
		if parts := strings.SplitN(basename, "_", 3); len(parts) >= 3 {
			name = parts[2]
		} else {
			name = basename
		}
	}
	safe, err := inboundAttachmentUnsafe.Replace(name, "_", -1, -1)
	if err != nil {
		panic(err)
	}
	return safe, agentPath
}

// Path to original name for the attachments whose original name the adapter kept
func inboundOriginalNames(event *gateway.MessageEvent) map[string]string {
	names := map[string]string{}
	for i, path := range event.MediaURLs {
		if i >= len(event.MediaFilenames) {
			break
		}
		if n := event.MediaFilenames[i]; n != "" {
			names[path] = n
		}
	}
	return names
}

// Prepends a path pointing note per audio file and video attachment whose content is not inlined
func prependInboundMediaFileNotes(ctx context.Context, messageText string, audioFilePaths, videoPaths []string, originalNames map[string]string) string {
	kinds := []struct {
		kind, noun, verb, tool string
		paths                  []string
	}{
		{"an audio file attachment", "audio", "transcribe or process", "a transcription or media tool", audioFilePaths},
		{"a video attachment", "video", "inspect or process", "a video analysis or media tool", videoPaths},
	}
	for _, k := range kinds {
		for _, path := range k.paths {
			display, agentPath := inboundAttachmentDisplayName(ctx, path, originalNames[path])
			messageText = fmt.Sprintf("[The user sent %s: '%s'. It is saved at: %s. "+
				"Its content is not inlined here. If the user's request involves "+
				"what the %s contains, %s it yourself — for "+
				"example by passing the path to %s — "+
				"instead of asking the user to describe it. Only ask what to do "+
				"with it if their intent is genuinely unclear.]\n\n%s",
				k.kind, display, agentPath, k.noun, k.verb, k.tool, messageText)
		}
	}
	return messageText
}

var inboundTextExtensions = map[string]bool{
	".txt": true, ".md": true, ".csv": true, ".log": true, ".json": true, ".xml": true,
	".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".cfg": true,
}

// Prepends a context note per attachment that is not routed as image, audio or video
func prependInboundDocumentNotes(ctx context.Context, event *gateway.MessageEvent, messageText string) string {
	if len(event.MediaURLs) == 0 {
		return messageText
	}
	for i, path := range event.MediaURLs {
		if eventMediaIsImage(event, i) || eventMediaIsAudio(event, i) || eventMediaIsVideo(event, i) {
			continue
		}
		mtype := eventMediaTypeAt(event, i)
		if mtype == "" || mtype == "application/octet-stream" {
			_, ext := pypath.Splitext(path)
			if inboundTextExtensions[strings.ToLower(ext)] {
				mtype = "text/plain"
			} else if guessed, _, _ := pymimetypes.GuessType(path, true); guessed != "" {
				mtype = guessed
			} else {
				mtype = "application/octet-stream"
			}
		}
		original := ""
		if i < len(event.MediaFilenames) {
			original = event.MediaFilenames[i]
		}
		displayName, agentPath := inboundAttachmentDisplayName(ctx, path, original)
		inlined := true
		if i < len(event.MediaTextInlined) && event.MediaTextInlined[i] != nil && !*event.MediaTextInlined[i] {
			inlined = false
		}
		messageText = buildDocumentContextNote(displayName, agentPath, mtype, inlined) + "\n\n" + messageText
	}
	return messageText
}

// Prepends the reply to pointer, then the Discord triggering message note as the outermost prefix
func prependInboundReplyContext(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, messageText string) string {
	if event.ReplyToText != nil && *event.ReplyToText != "" && event.ReplyToMessageID != nil && *event.ReplyToMessageID != "" {
		who := ""
		if event.ReplyToIsOwnMessage {
			who = " your previous message"
		}
		messageText = fmt.Sprintf("[Replying to%s: \"%s\"]\n\n%s", who, *event.ReplyToText, messageText)
	}
	if source != nil && source.Platform == gateway.PlatformDiscord && event.MessageID != nil && *event.MessageID != "" {
		if gateway.DiscordToolsLoaded(ctx) {
			messageText = discordTriggeringNote(*event.MessageID) + "\n\n" + messageText
		}
	}
	return messageText
}
