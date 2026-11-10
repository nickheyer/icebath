package plugins

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins/verify"
	"github.com/nickheyer/icebath/pkg/tools"
)

// RunVerifyCommand detects a project's run recipe and smoke tests it, hermes_cli/verify_cmd.py run_verify_command
func RunVerifyCommand(ctx context.Context, ns map[string]any, w, errw io.Writer) int {
	root := nsVerifyString(ns, "path")
	if root == "" {
		root = "."
	}
	if resolved, err := pypath.Resolve(root); err == nil {
		root = resolved
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		fmt.Fprintf(errw, "error: not a directory: %s\n", root)
		return 2
	}
	asJSON := nsVerifyBool(ns, "json")
	recipe, source, err := verify.LoadOrDetect(root)
	if err != nil {
		fmt.Fprintln(errw, pyrepr.ExceptionRepr(err))
		return 1
	}
	if recipe == nil {
		if asJSON {
			fmt.Fprintln(w, mustDumps(pyjson.ObjectOf("ok", false, "error", "no-recipe", "root", root), false))
		} else {
			fmt.Fprintf(errw, "No recognizable project found at %s.\nCreate %s to define a recipe manually.\n", root, verify.ManifestPath(root))
		}
		return 1
	}
	if source == "detected" {
		mergeProjectFactsCommands(root, recipe)
	}
	port := nsVerifyInt(ns, "port")
	if port != 0 {
		recipe.Port = &port
	}
	if nsVerifyBool(ns, "save") {
		path, err := verify.SaveManifest(root, recipe)
		if err != nil {
			fmt.Fprintln(errw, pyrepr.ExceptionRepr(err))
			return 1
		}
		if !asJSON {
			fmt.Fprintf(w, "Saved manifest: %s\n", path)
		}
	}
	if nsVerifyBool(ns, "detect_only") {
		fmt.Fprintln(w, mustDumps(pyjson.ObjectOf("source", source, "recipe", recipe.ToDict()), !asJSON))
		return 0
	}
	phases := nsVerifyStrings(ns, "phase")
	opts := verify.NewVerifyOptions()
	opts.Phases, opts.SkipStart = phases, nsVerifyBool(ns, "skip_start")
	if timeout, ok := nsVerifyFloat(ns, "timeout"); ok {
		opts.PhaseTimeout = timeout
	}
	if ready, ok := nsVerifyFloat(ns, "ready_timeout"); ok {
		opts.ReadyTimeout = ready
	}
	if port != 0 {
		opts.PortOverride = &port
	}
	result, err := verify.RunVerify(ctx, root, recipe, opts)
	if err != nil {
		fmt.Fprintln(errw, pyrepr.ExceptionRepr(err))
		return 1
	}
	recordVerifyEvidence(ctx, root, recipe, result, len(phases) > 0 || opts.SkipStart)
	code := 1
	if result.OK() {
		code = 0
	}
	if asJSON {
		payload := result.ToDict()
		payload.Set("source", source)
		fmt.Fprintln(w, mustDumps(payload, false))
		return code
	}
	printVerifyHumanReport(w, recipe, source, result)
	return code
}

// Folds the detected project facts verify commands into a detected recipe's test phase, never a saved manifest
func mergeProjectFactsCommands(root string, recipe *verify.Recipe) {
	existing := map[string]bool{}
	for _, c := range append(append(append([]string{}, recipe.Bootstrap...), recipe.Build...), recipe.Test...) {
		if c != "" {
			existing[strings.TrimSpace(c)] = true
		}
	}
	for _, command := range tools.DetectProjectFacts(root).VerifyCommands {
		command = strings.TrimSpace(command)
		if command != "" && !existing[command] {
			recipe.Test = append(recipe.Test, command)
			existing[command] = true
		}
	}
}

// Records the run in the verification evidence ledger, a partial pass downgraded to targeted and ledger failures ignored
func recordVerifyEvidence(ctx context.Context, root string, recipe *verify.Recipe, result *verify.VerifyResult, partial bool) {
	if verify.EvidenceRecorder == nil {
		return
	}
	var tails []string
	for _, p := range result.Phases {
		if p.OutputTail != "" {
			tails = append(tails, fmt.Sprintf("[%s] %s\n%s", p.Phase, p.Command, p.OutputTail))
		}
	}
	if result.Readiness != nil {
		tails = append(tails, fmt.Sprintf("[start] %s -> %s", recipeStartText(recipe), readinessStatus(result.Readiness)))
	}
	scope := "full"
	if partial {
		scope = "targeted"
	}
	func() {
		defer func() { _ = recover() }()
		_, _ = verify.EvidenceRecorder(ctx, root, os.Getenv("ICEBATH_SESSION_ID"), result.OK(), "icebath verify", scope, strings.Join(tails, "\n"))
	}()
}

// The start command as Python prints it, None for a recipe without one
func recipeStartText(recipe *verify.Recipe) string {
	if recipe.Start == nil {
		return "None"
	}
	return *recipe.Start
}

// ready (HTTP code) or not ready (error or timeout)
func readinessStatus(r *verify.ReadinessResult) string {
	if r.Ready {
		if r.StatusCode == nil {
			return "ready (HTTP None)"
		}
		return fmt.Sprintf("ready (HTTP %d)", *r.StatusCode)
	}
	reason := "timeout"
	if r.Error != nil && *r.Error != "" {
		reason = *r.Error
	}
	return fmt.Sprintf("not ready (%s)", reason)
}

// The phase table, readiness line, result and failing output tails of a human run
func printVerifyHumanReport(w io.Writer, recipe *verify.Recipe, source string, result *verify.VerifyResult) {
	fmt.Fprintf(w, "Recipe: %s (%s) — source: %s\n", recipe.Name, recipe.Kind, source)
	fmt.Fprintln(w)
	width := 0
	for _, p := range result.Phases {
		width = max(width, len(p.Phase))
	}
	if len(result.Phases) > 0 {
		for _, p := range result.Phases {
			status := "FAIL"
			if p.OK() {
				status = "PASS"
			} else if p.TimedOut {
				status = "TIMEOUT"
			}
			fmt.Fprintf(w, "  %-*s  %-7s  %6.1fs  %s\n", width, p.Phase, status, p.Duration, p.Command)
		}
	} else {
		fmt.Fprintln(w, "  (no phases executed)")
	}
	if r := result.Readiness; r != nil {
		status := "FAIL"
		if r.Ready {
			status = "PASS"
		}
		fmt.Fprintf(w, "  %-*s  %-7s  %6.1fs  %s\n", max(5, width), "start", status, r.Duration, recipeStartText(recipe))
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Readiness: %s -> %s\n", r.URL, readinessStatus(r))
	}
	fmt.Fprintln(w)
	verdict := "FAILED"
	if result.OK() {
		verdict = "OK"
	}
	fmt.Fprintf(w, "Result: %s\n", verdict)
	for _, p := range result.Phases {
		if !p.OK() {
			fmt.Fprintf(w, "\n--- output tail: %s (%s) ---\n", p.Phase, p.Command)
			fmt.Fprintln(w, strings.TrimRight(p.OutputTail, " \t\r\n\x0b\x0c"))
		}
	}
	if r := result.Readiness; r != nil && !r.Ready && r.OutputTail != "" {
		fmt.Fprintln(w, "\n--- output tail: start ---")
		fmt.Fprintln(w, strings.TrimRight(r.OutputTail, " \t\r\n\x0b\x0c"))
	}
}

// json.dumps text of a payload, indent 2 when pretty
func mustDumps(v any, pretty bool) string {
	opts := []pyjson.Option{}
	if pretty {
		opts = append(opts, pyjson.Indent(2))
	}
	text, err := pyjson.Dumps(v, opts...)
	if err != nil {
		panic(err)
	}
	return text
}

func nsVerifyString(ns map[string]any, key string) string {
	if v, ok := ns[key].(string); ok {
		return v
	}
	return ""
}

func nsVerifyBool(ns map[string]any, key string) bool {
	return pyrepr.Truthy(ns[key])
}

func nsVerifyInt(ns map[string]any, key string) int {
	switch v := ns[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}

func nsVerifyFloat(ns map[string]any, key string) (float64, bool) {
	switch v := ns[key].(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	}
	return 0, false
}

func nsVerifyStrings(ns map[string]any, key string) []string {
	switch v := ns[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, pyrepr.Str(item))
		}
		return out
	}
	return nil
}
