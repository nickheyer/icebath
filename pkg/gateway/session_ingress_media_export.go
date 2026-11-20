package gateway

import "context"

// Most attachments one submission may carry
const AttachmentLimit = attachmentLimit

// Staged file extension of each admitted image type
var IngressImageExt = map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}

// Admission image type the bytes really are, empty for anything else
func SniffIngressImageMime(data []byte) string { return sniffImageMime(data) }

// Frozen policy of a bypass launch from its session.create parameters, code defaults and never the profile
func BypassLaunchPolicy(ctx context.Context, params map[string]any) (*LocalSessionPolicy, error) {
	return bypassPolicy(ctx, params, &RecoveredSecrets{}, false)
}
