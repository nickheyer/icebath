package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Byte cap of an attached payload the owner's image staging enforces
const AttachBytesMaxBytes = attachBytesMaxBytes

// Copies a browser upload receipt into the session under the browser cap
func StageBrowserFileAttachment(ctx context.Context, session *tuigateway.Session, stagedUpload any, name string) (string, bool, error) {
	return stageBrowserFileAttachment(ctx, session, stagedUpload, name)
}

// Makes a desktop file attachment visible to the agent with the default cap and no staged receipt
func StageSessionFileAttachment(ctx context.Context, session *tuigateway.Session, rawPath, dataURL, name string) (string, bool, error) {
	return stageSessionFileAttachment(ctx, session, rawPath, dataURL, name, nil, false)
}

// Workspace relative path of an attachment, absolute outside the workspace
func AttachmentRefPath(ctx context.Context, session *tuigateway.Session, target string) string {
	return attachmentRefPath(ctx, session, target)
}

// Quoted file ref value that round trips whitespace, brackets and quotes
func FormatRefValue(value string) string { return formatRefValue(value) }

// Decoded data URL or bare base64 payload under the cap, nil when invalid
func DecodeAttachBase64(raw, mimePrefix string, maxBytes int) ([]byte, error) {
	return decodeAttachBase64(raw, mimePrefix, maxBytes, "attachment")
}

// Image extension from the filename hint, else magic bytes, else png
func SniffImageExt(data []byte, filename string) string { return sniffImageExt(data, filename) }

// Private collision free image staging shared by local and canonical byte uploads
func StageImageBytes(directory string, data []byte, ext, prefix string) (string, error) {
	return stageImageBytes(directory, data, ext, prefix)
}
