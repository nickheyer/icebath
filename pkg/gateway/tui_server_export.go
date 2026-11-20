package gateway

import "context"

// Approval command or description with credential shaped values redacted for a client prompt
func RedactApprovalCommand(ctx context.Context, text any) string {
	return redactApprovalCommand(ctx, text)
}

// Capabilities a bootstrap purpose grants, nil for an unknown purpose
func PurposeCapabilities(purpose string) map[string]bool {
	out := map[string]bool{}
	for capability := range purposeCapabilities[purpose] {
		out[capability] = true
	}
	return out
}
