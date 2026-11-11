package gatewayrun

import (
	"context"
	"fmt"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Attributes the sender in shared multi user sessions and prepends the history backfill channel context
func (r *GatewayRunner) prefixInboundSenderContext(event *gateway.MessageEvent, source *gateway.SessionSource, messageText string) string {
	shared := gateway.IsSharedMultiUserSession(source, r.config.GroupSessionsPerUser, r.config.ThreadSessionsPerUser)
	if shared && source.UserName != nil && *source.UserName != "" {
		safeUserName := gateway.NeutralizeUntrustedInlineText(*source.UserName, gateway.MaxPromptMetadataChars)
		if source.Platform == gateway.PlatformSlack && source.UserID != nil && *source.UserID != "" {
			safeUserName = fmt.Sprintf("%s | Slack user <@%s>", safeUserName, *source.UserID)
		}
		messageText = fmt.Sprintf("[%s] %s", safeUserName, messageText)
	}
	if event.ChannelContext != nil && *event.ChannelContext != "" {
		messageText = fmt.Sprintf("%s\n\n[New message]\n%s", *event.ChannelContext, messageText)
	}
	return messageText
}

// Transcribes inbound voice clips into the text and echoes the transcripts through the answering adapter
func (r *GatewayRunner) enrichInboundVoice(ctx context.Context, event *gateway.MessageEvent, source *gateway.SessionSource, messageText string, audioPaths []string) (string, error) {
	messageText, transcripts := r.enrichMessageWithTranscription(ctx, messageText, audioPaths, nil)
	if len(transcripts) > 0 && r.shouldEchoSTTTranscripts() {
		adapter, err := r.deliveryAdapterFor(ctx, source)
		if err != nil {
			return "", err
		}
		if adapter != nil {
			metadata := r.threadMetadataForSource(source, gateway.ReplyAnchorForEvent(event))
			r.echoSTTTranscripts(ctx, adapter, source, transcripts, metadata, "Transcript")
		}
	}
	return messageText, nil
}
