package gatewayrun

import (
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Per attachment MIME at index, empty when the adapter set only a message level type
func eventMediaTypeAt(event *gateway.MessageEvent, index int) string {
	if index < len(event.MediaTypes) {
		return event.MediaTypes[index]
	}
	return ""
}

// Per attachment MIME prefix first, the message level type only when the MIME is unknown
func eventMediaKindIs(event *gateway.MessageEvent, index int, mimePrefix string, fallback ...gateway.MessageType) bool {
	if mtype := eventMediaTypeAt(event, index); mtype != "" {
		return strings.HasPrefix(mtype, mimePrefix)
	}
	for _, kind := range fallback {
		if event.MessageType == kind {
			return true
		}
	}
	return false
}

// True when the attachment at index is an image
func eventMediaIsImage(event *gateway.MessageEvent, index int) bool {
	return eventMediaKindIs(event, index, "image/", gateway.MessageTypePhoto)
}

// True when the attachment at index is audio
func eventMediaIsAudio(event *gateway.MessageEvent, index int) bool {
	return eventMediaKindIs(event, index, "audio/", gateway.MessageTypeVoice, gateway.MessageTypeAudio)
}

// True when an audio attachment should enter the automatic speech to text pipeline
func eventMediaIsSTTInput(event *gateway.MessageEvent, index int) bool {
	if event.MessageType == gateway.MessageTypeAudio || event.MessageType == gateway.MessageTypeDocument {
		return false
	}
	return event.MessageType == gateway.MessageTypeVoice || strings.HasPrefix(eventMediaTypeAt(event, index), "audio/")
}

// True when the attachment at index is a video
func eventMediaIsVideo(event *gateway.MessageEvent, index int) bool {
	return eventMediaKindIs(event, index, "video/", gateway.MessageTypeVideo)
}

// Text naming each attachment of a media only event, later replaced by vision enrichment
func buildMediaOnlyText(event *gateway.MessageEvent) string {
	parts := make([]string, 0, len(event.MediaURLs))
	for i, url := range event.MediaURLs {
		switch {
		case eventMediaIsImage(event, i):
			parts = append(parts, fmt.Sprintf("[User sent an image: %s]", url))
		case eventMediaIsAudio(event, i):
			parts = append(parts, fmt.Sprintf("[User sent audio: %s]", url))
		case eventMediaIsVideo(event, i):
			parts = append(parts, fmt.Sprintf("[User sent a video: %s]", url))
		default:
			parts = append(parts, fmt.Sprintf("[User sent a file: %s]", url))
		}
	}
	return strings.Join(parts, "\n")
}

// Context note prepended to a user turn for an attached document, telling the agent to read it when not inlined
func buildDocumentContextNote(displayName, agentPath, mtype string, contentInlined bool) string {
	if strings.HasPrefix(mtype, "text/") && contentInlined {
		return fmt.Sprintf("[The user sent a text document: '%s'. Its content has been included below. "+
			"The file is also saved at: %s]", displayName, agentPath)
	}
	if strings.HasPrefix(mtype, "text/") {
		return fmt.Sprintf("[The user sent a text document: '%s'. It is saved at: %s. "+
			"Its content is not inlined here. Read the cached file yourself before answering "+
			"when the user's request involves its contents.]", displayName, agentPath)
	}
	return fmt.Sprintf("[The user sent a document: '%s'. It is saved at: %s. "+
		"Its text is not inlined here (it's a binary format such as PDF or DOCX). "+
		"To read it, extract the document's text yourself — for example with the "+
		"terminal tool or the ocr-and-documents skill — before answering, instead "+
		"of asking the user to paste the contents.]", displayName, agentPath)
}
