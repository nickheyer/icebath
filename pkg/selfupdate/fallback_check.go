package selfupdate

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Providers that are no single fixed route, auto picking its backend per call and moa fanning out a preset, so they keep their static verdict
var fallbackCheckNotProbed = map[string]bool{"auto": true, "moa": true}

// Installs icebath fallback check on the fallback command dispatcher
func init() { provider.SetFallbackCheckCommand(FallbackCheckCommand) }

// RunFallbackChecks is the static check of every chain entry in order, then with live the statically ready fixed routes probed in parallel, the instance metadata lookup kept for an EC2 role
func RunFallbackChecks(ctx context.Context, chain []map[string]any, live bool) ([]*provider.FallbackEntryCheck, error) {
	results := make([]*provider.FallbackEntryCheck, 0, len(chain))
	for _, entry := range chain {
		result, err := provider.CheckFallbackEntryStatic(ctx, entry, redactDebugSupportText)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if !live {
		return results, nil
	}
	var probes []doctorProbe
	for i, result := range results {
		if !result.Ready() || fallbackCheckNotProbed[result.Provider] {
			continue
		}
		probes = append(probes, doctorProbe{result.Provider, func() (doctorProbeResult, error) {
			return doctorProbeResult{}, provider.CheckFallbackEntryLive(ctx, chain[i], result, redactDebugSupportText)
		}})
	}
	if _, err := doctorRunProbes(probes, true); err != nil {
		return nil, err
	}
	return results, nil
}

// Table cell of a verdict, a dash while unknown
func fallbackCheckMark(value *bool) string {
	switch {
	case value == nil:
		return "—"
	case *value:
		return "ok"
	}
	return "FAIL"
}

// One table line of the seven columns at their fixed widths
func fallbackCheckRow(index, label, configured, auth, live, latency, status string) string {
	return "  " + pystr.Ljust(index, 3) + pystr.Ljust(label, 44) + pystr.Ljust(configured, 8) + pystr.Ljust(auth, 8) +
		pystr.Ljust(live, 8) + pystr.Ljust(latency, 10) + status
}

// Prints the header, one row per entry with its error on the next line, and the static only note when no probe was asked for
func printFallbackCheckTable(w io.Writer, results []*provider.FallbackEntryCheck, live bool) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, fallbackCheckRow("#", "Provider / model", "Config", "Auth", "Live", "Latency", "Status"))
	for i, r := range results {
		latency := "—"
		if r.LatencyMs != nil {
			latency = strconv.Itoa(*r.LatencyMs) + " ms"
		}
		var auth *bool
		if r.Configured {
			auth = &r.AuthReady
		}
		fmt.Fprintln(w, fallbackCheckRow(strconv.Itoa(i+1), pystr.Slice(r.Provider+" / "+r.Model, 0, 43), fallbackCheckMark(&r.Configured),
			fallbackCheckMark(auth), fallbackCheckMark(r.InferenceReady), latency, r.Status()))
		if r.Error != "" {
			fmt.Fprintln(w, "     "+r.Error)
		}
	}
	fmt.Fprintln(w)
	if !live {
		fmt.Fprintln(w, "  Static check only (no inference request sent). Use --live to send one minimal request per entry.")
		fmt.Fprintln(w)
	}
}

// JSON object of one verdict, its fields in declaration order then ready and status
func fallbackCheckObject(r *provider.FallbackEntryCheck) *pyjson.Object {
	var inferenceReady, latencyMs any
	if r.InferenceReady != nil {
		inferenceReady = *r.InferenceReady
	}
	if r.LatencyMs != nil {
		latencyMs = *r.LatencyMs
	}
	return pyjson.ObjectOf("provider", r.Provider, "model", r.Model, "base_url", r.BaseURL, "api_mode", r.APIMode, "auth_source", r.AuthSource,
		"configured", r.Configured, "auth_ready", r.AuthReady, "inference_ready", inferenceReady, "latency_ms", latencyMs,
		"model_returned", r.ModelReturned, "error_class", r.ErrorClass, "error", r.Error, "ready", r.Ready(), "status", r.Status())
}

// FallbackCheckCommand is icebath fallback check, the readiness table or JSON of every chain entry without an agent session, 1 when any entry is not ready
func FallbackCheckCommand(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	live := pyrepr.Truthy(ns["live"])
	results, err := RunFallbackChecks(ctx, provider.FallbackChain(home.WithOverride(ctx, h)), live)
	if err != nil {
		return 1, err
	}
	ready := true
	for _, r := range results {
		ready = ready && r.Ready()
	}
	switch {
	case pyrepr.Truthy(ns["json"]):
		rows := make([]any, 0, len(results))
		for _, r := range results {
			rows = append(rows, fallbackCheckObject(r))
		}
		text, err := pyjson.Dumps(pyjson.ObjectOf("profile", profiles.GetActiveProfileName(ctx), "live", live, "ready", ready,
			"checked_at", pydatetime.ISOFormatUTC(time.Now()), "results", rows), pyjson.Indent(2))
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(w, text)
	case len(results) == 0:
		fmt.Fprint(w, "\n  No fallback providers configured. Add one with: icebath fallback add\n\n")
	default:
		printFallbackCheckTable(w, results, live)
	}
	if !ready {
		return 1, nil
	}
	return 0, nil
}
