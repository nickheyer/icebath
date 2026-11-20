package verify

import "context"

// Ledger writer a completed verify run records through, assigned by the agent package
var EvidenceRecorder func(ctx context.Context, root, sessionID string, ok bool, command, scope, output string) (map[string]any, error)
