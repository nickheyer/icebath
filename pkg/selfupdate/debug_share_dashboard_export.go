package selfupdate

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Uploads the debug report and logs with the default expiry and returns the fields the dashboard route answers with
func DebugShareForDashboard(ctx context.Context, logLines int, redact bool) (*pyjson.Object, error) {
	result, err := BuildDebugShare(ctx, logLines, 1, redact)
	if err != nil {
		return nil, err
	}
	urls := pyjson.NewObject()
	for _, key := range result.URLKeys {
		urls.Set(key, result.URLs[key])
	}
	failures := make([]any, 0, len(result.Failures))
	for _, f := range result.Failures {
		failures = append(failures, f)
	}
	return pyjson.ObjectOf("urls", urls, "failures", failures, "redacted", result.Redacted,
		"auto_delete_seconds", int64(result.AutoDeleteSeconds)), nil
}
