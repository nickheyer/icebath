package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Where the usage command reports a missing snapshot, stderr like the hermes command
var usageCommandErrOutput io.Writer = os.Stderr

// UsageSnapshotDocument is the icebath usage --json document, keys stable and extended only by adding keys
func UsageSnapshotDocument(snapshot *AccountUsageSnapshot) map[string]any {
	windows := make([]any, 0, len(snapshot.Windows))
	for _, w := range snapshot.Windows {
		var used, resets any
		if w.UsedPercent != nil {
			used = *w.UsedPercent
		}
		if w.ResetAt != nil {
			resets = pydatetime.ISOFormat(*w.ResetAt)
		}
		windows = append(windows, map[string]any{"label": w.Label, "used_percent": used, "resets_at": resets, "detail": w.Detail})
	}
	details := make([]any, 0, len(snapshot.Details))
	for _, d := range snapshot.Details {
		details = append(details, d)
	}
	var plan, unavailable any
	if snapshot.Plan != "" {
		plan = snapshot.Plan
	}
	if snapshot.UnavailableReason != "" {
		unavailable = snapshot.UnavailableReason
	}
	return map[string]any{
		"provider":           snapshot.Provider,
		"source":             snapshot.Source,
		"title":              snapshot.Title,
		"plan":               plan,
		"fetched_at":         pydatetime.ISOFormat(snapshot.FetchedAt),
		"windows":            windows,
		"details":            details,
		"unavailable_reason": unavailable,
	}
}

// UsageCommand prints the configured or requested account's usage windows, exit 1 when nothing could be fetched
func UsageCommand(ctx context.Context, env PoolEnv, ns map[string]any, w io.Writer) (int, error) {
	provider, err := ResolveRequestedProvider(ctx, envHome(env), anyString(ns["provider"]))
	if err != nil {
		return 1, err
	}
	snapshot := FetchAccountUsage(ctx, provider, AccountUsageFetchOptions{})
	if snapshot == nil {
		fmt.Fprintf(usageCommandErrOutput, "No account usage available for provider '%s': no credential is configured for it, the provider has no usage endpoint, or the fetch failed.\n", provider)
		return 1, nil
	}
	if truthy(ns["json"]) {
		doc, err := pyjson.Dumps(UsageSnapshotDocument(snapshot), pyjson.Indent(2))
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(w, doc)
		return 0, nil
	}
	fmt.Fprintln(w, strings.Join(RenderAccountUsageLines(snapshot, false), "\n"))
	return 0, nil
}
