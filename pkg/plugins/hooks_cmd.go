package plugins

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Parsed icebath hooks arguments, Action empty when no subcommand was given
type HooksArgs struct {
	Action, Event, ForTool, PayloadFile, Command string
}

var hookActions = map[string]func(ctx context.Context, args HooksArgs, w io.Writer) error{
	"list": hooksList, "ls": hooksList,
	"test":   hooksTest,
	"revoke": hooksRevoke, "remove": hooksRevoke, "rm": hooksRevoke,
	"doctor": hooksDoctor,
}

// Dispatches one icebath hooks action, printing usage for a missing one and a notice for an unknown one
func HooksCommand(ctx context.Context, args HooksArgs, w io.Writer) error {
	if args.Action == "" {
		fmt.Fprintln(w, "Usage: icebath hooks {list|test|revoke|doctor}")
		fmt.Fprintln(w, "Run 'icebath hooks --help' for details.")
		return nil
	}
	handler, ok := hookActions[args.Action]
	if !ok {
		fmt.Fprintf(w, "Unknown hooks subcommand: %s\n", args.Action)
		return nil
	}
	return handler(ctx, args, w)
}

// Prints configured shell hooks with consent and drift state, then outbound webhook targets
func hooksList(ctx context.Context, _ HooksArgs, w io.Writer) error {
	cfg, err := loadConfig(home.Dir(ctx))
	if err != nil {
		return err
	}
	specs := IterConfiguredShellHooks(cfg)
	outbound, err := IterConfiguredOutboundWebhooks(cfg)
	if err != nil {
		return err
	}
	if len(specs) == 0 && len(outbound) == 0 {
		fmt.Fprintln(w, "No shell hooks or outbound webhooks configured in ~/.icebath/config.yaml.")
		fmt.Fprintln(w, "See `icebath hooks --help` or")
		fmt.Fprintln(w, "    website/docs/user-guide/features/hooks.md")
		fmt.Fprintln(w, "for the config schema and worked examples.")
		return nil
	}
	if len(specs) == 0 {
		fmt.Fprintln(w, "No shell hooks configured in ~/.icebath/config.yaml.")
	} else {
		byEvent := map[string][]*ShellHookSpec{}
		for _, spec := range specs {
			byEvent[spec.Event] = append(byEvent[spec.Event], spec)
		}
		events := make([]string, 0, len(byEvent))
		for event := range byEvent {
			events = append(events, event)
		}
		sort.Strings(events)
		approved := AllowlistedShellHookPairs(ctx)
		fmt.Fprintf(w, "Configured shell hooks (%d total):\n\n", len(specs))
		for _, event := range events {
			fmt.Fprintf(w, "  [%s]\n", event)
			for _, spec := range byEvent[event] {
				isApproved := approved[[2]string{spec.Event, spec.Command}]
				status := "✗ not allowlisted"
				if isApproved {
					status = "✓ allowed"
				}
				fmt.Fprintf(w, "    - %s%s (timeout=%ds, %s)\n", spec.Command, hookMatcherPart(spec.Matcher), spec.Timeout, status)
				var entry map[string]any
				if isApproved {
					entry = ShellHookAllowlistEntry(ctx, spec.Event, spec.Command)
				}
				if len(entry) > 0 && config.PyTruthy(entry["approved_at"]) {
					fmt.Fprintf(w, "      approved_at: %s\n", pyrepr.Str(entry["approved_at"]))
					drift, at, now, err := hookMtimeDrift(spec, entry)
					if err != nil {
						return err
					}
					if drift == 1 {
						fmt.Fprintf(w, "      ⚠ script modified since approval (was %s, now %s) — run `icebath hooks doctor` to re-validate\n", at, now)
					}
				}
			}
			fmt.Fprintln(w)
		}
	}
	if len(outbound) > 0 {
		fmt.Fprintf(w, "Configured outbound webhooks (%d total):\n\n", len(outbound))
		for _, target := range outbound {
			signed := "UNSIGNED"
			if target.Secret != "" {
				signed = "signed"
			}
			fmt.Fprintf(w, "  - %s\n", target.Label())
			fmt.Fprintf(w, "      url:     %s\n", target.URL)
			fmt.Fprintf(w, "      events:  %s%s (timeout=%ds, %s)\n", strings.Join(target.Events, ", "), hookMatcherPart(target.Matcher), target.Timeout, signed)
		}
		fmt.Fprintln(w)
	}
	return nil
}

// Matcher suffix of a list line, empty without a matcher
func hookMatcherPart(matcher string) string {
	if matcher == "" {
		return ""
	}
	return " matcher=" + pyrepr.Repr(matcher)
}

// Drift 1 when the script changed since approval, 0 when unchanged, -1 when either mtime is unknown
func hookMtimeDrift(spec *ShellHookSpec, entry map[string]any) (int, string, string, error) {
	now, _ := ShellHookScriptMtimeISO(spec.Command)
	rawAt := entry["script_mtime_at_approval"]
	at := pyrepr.Str(rawAt)
	if now == "" || !config.PyTruthy(rawAt) {
		return -1, at, now, nil
	}
	atText, ok := rawAt.(string)
	if !ok {
		return 0, at, now, fmt.Errorf("'>' not supported between instances of 'str' and '%s'", pyrepr.TypeName(rawAt))
	}
	if now > atText {
		return 1, at, now, nil
	}
	if now == atText {
		return 0, at, now, nil
	}
	return -1, at, now, nil
}

// Synthetic kwargs matching the real invoke_hook call sites, fed through the production payload serializer
func defaultHookPayload(event string) (map[string]any, bool) {
	toolArgs := func() map[string]any { return map[string]any{"command": "echo hello"} }
	api := func() map[string]any {
		return map[string]any{"session_id": "test-session", "task_id": "test-task", "platform": "cli",
			"model": "claude-sonnet-4-6", "provider": "anthropic", "base_url": "https://api.anthropic.com",
			"api_mode": "anthropic_messages", "api_call_count": 1}
	}
	aux := func() map[string]any {
		return map[string]any{"aux_task": "title_generation", "session_id": "test-session", "task_id": "test-task",
			"turn_id": "test-turn", "api_request_id": "aux-0123abcd", "platform": "cli", "model": "claude-haiku-4-5",
			"provider": "anthropic", "base_url": "https://api.anthropic.com", "api_mode": "anthropic_messages",
			"api_call_count": 1, "retry_count": 0, "streaming": false}
	}
	merge := func(base map[string]any, extra map[string]any) map[string]any {
		for k, v := range extra {
			base[k] = v
		}
		return base
	}
	switch event {
	case "pre_tool_call":
		return map[string]any{"tool_name": "terminal", "args": toolArgs(), "session_id": "test-session",
			"task_id": "test-task", "tool_call_id": "test-call"}, true
	case "post_tool_call":
		return map[string]any{"tool_name": "terminal", "args": toolArgs(), "session_id": "test-session",
			"task_id": "test-task", "tool_call_id": "test-call", "result": `{"output": "hello"}`, "duration_ms": 42}, true
	case "pre_llm_call":
		return map[string]any{"session_id": "test-session", "user_message": "What is the weather?",
			"conversation_history": []any{}, "is_first_turn": true, "model": "gpt-4", "platform": "cli"}, true
	case "post_llm_call":
		return map[string]any{"session_id": "test-session", "model": "gpt-4", "platform": "cli"}, true
	case "pre_verify":
		return map[string]any{"session_id": "test-session", "platform": "cli", "model": "gpt-4", "coding": true,
			"attempt": 0, "final_response": "All done — the change is applied.", "changed_paths": []any{"src/app.tsx"}}, true
	case "on_session_start", "on_session_finalize", "on_session_reset":
		return map[string]any{"session_id": "test-session"}, true
	case "on_session_end":
		return map[string]any{"session_id": "test-session", "task_id": "test-task", "turn_id": "test-turn",
			"completed": true, "failed": false, "interrupted": false, "turn_exit_reason": "text_response(stop)",
			"model": "gpt-4", "platform": "cli"}, true
	case "on_session_delete":
		return map[string]any{"session_id": "test-session", "reason": "delete", "profile": "default"}, true
	case "pre_api_request":
		return merge(api(), map[string]any{"message_count": 4, "tool_count": 12, "approx_input_tokens": 2048,
			"request_char_count": 8192, "max_tokens": 4096}), true
	case "post_api_request":
		return merge(api(), map[string]any{"api_duration": 1.234, "started_at": 1756000000.0, "ended_at": 1756000001.234,
			"first_chunk_at": 1756000000.512, "finish_reason": "stop", "message_count": 4, "response_model": "claude-sonnet-4-6",
			"usage": map[string]any{"input_tokens": 2048, "output_tokens": 512}, "context_length": 200000,
			"assistant_content_chars": 1200, "assistant_tool_call_count": 0, "moa_references": nil}), true
	case "pre_auxiliary_call":
		return merge(aux(), map[string]any{"message_count": 2, "tool_count": 0, "approx_input_tokens": 256,
			"request_char_count": 1024, "max_tokens": 64}), true
	case "post_auxiliary_call":
		return merge(aux(), map[string]any{"api_duration": 0.42, "started_at": 1756000000.0, "ended_at": 1756000000.42,
			"finish_reason": "stop", "response_model": "claude-haiku-4-5",
			"usage": map[string]any{"input_tokens": 256, "output_tokens": 12}, "assistant_content_chars": 40,
			"assistant_tool_call_count": 0, "error": nil, "error_type": nil}), true
	case "subagent_stop":
		return map[string]any{"parent_session_id": "parent-sess", "child_role": nil,
			"child_summary": "Synthetic summary for hooks test", "child_status": "completed",
			"tool_call_history": []any{map[string]any{"tool_name": "write_file",
				"tool_input":  map[string]any{"argument_keys": []any{"content", "path"}, "targets": map[string]any{"path": "notes/report.txt"}},
				"input_bytes": 128, "output_bytes": 32, "status": "ok"}},
			"duration_ms": 1234}, true
	}
	return nil, false
}

// Fires every configured hook of an event once on a synthetic payload and prints each result
func hooksTest(ctx context.Context, args HooksArgs, w io.Writer) error {
	event := args.Event
	if !validHooks[event] {
		fmt.Fprintf(w, "Unknown event: %s\n", pyrepr.Repr(event))
		fmt.Fprintf(w, "Valid events: %s\n", strings.Join(ValidHooks(), ", "))
		return nil
	}
	payload, ok := defaultHookPayload(event)
	if !ok {
		payload = map[string]any{"session_id": "test-session"}
	}
	if args.ForTool != "" {
		payload["tool_name"] = args.ForTool
	}
	if args.PayloadFile != "" {
		custom, err := readHookPayloadFile(args.PayloadFile)
		if err != nil {
			fmt.Fprintf(w, "Error reading payload file: %s\n", err)
			return nil
		}
		if obj, isObj := custom.(*pyjson.Object); isObj {
			for _, k := range obj.Keys {
				payload[k] = obj.Vals[k]
			}
		} else {
			fmt.Fprintf(w, "Warning: %s is not a JSON object; ignoring\n", args.PayloadFile)
		}
	}
	cfg, err := loadConfig(home.Dir(ctx))
	if err != nil {
		return err
	}
	var specs []*ShellHookSpec
	for _, spec := range IterConfiguredShellHooks(cfg) {
		if spec.Event != event {
			continue
		}
		if args.ForTool != "" && (spec.Event == "pre_tool_call" || spec.Event == "post_tool_call") {
			matches, err := spec.MatchesTool(args.ForTool)
			if err != nil {
				return err
			}
			if !matches {
				continue
			}
		}
		specs = append(specs, spec)
	}
	if len(specs) == 0 {
		fmt.Fprintf(w, "No shell hooks configured for event: %s\n", event)
		if args.ForTool != "" {
			fmt.Fprintf(w, "(with matcher filter --for-tool=%s)\n", args.ForTool)
		}
		return nil
	}
	fmt.Fprintf(w, "Firing %d hook(s) for event '%s':\n\n", len(specs), event)
	for _, spec := range specs {
		fmt.Fprintf(w, "  → %s\n", spec.Command)
		result, err := RunShellHookOnce(ctx, spec, payload)
		if err != nil {
			return err
		}
		if err := printHookRunResult(w, result); err != nil {
			return err
		}
		fmt.Fprintln(w)
	}
	return nil
}

// JSON document of a payload file read as utf-8-sig, the OSError, decode or JSON error text on failure
func readHookPayloadFile(path string) (any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%s", pypath.OSErrorText(err))
	}
	text, err := pycodecs.Decode(data, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	return pyjson.Loads(text)
}

// Prints one run with its parsed directive even after an error or timeout, so fail closed and fail open differ
func printHookRunResult(w io.Writer, r *ShellHookResult) error {
	switch {
	case r.Error != "":
		fmt.Fprintf(w, "      ✗ error: %s\n", r.Error)
	case r.TimedOut:
		fmt.Fprintf(w, "      ✗ timed out after %ss\n", pyrepr.Float(r.ElapsedSeconds))
	default:
		rc := "None"
		if r.ReturnCode != nil {
			rc = fmt.Sprint(*r.ReturnCode)
		}
		fmt.Fprintf(w, "      exit=%s  elapsed=%ss\n", rc, pyrepr.Float(r.ElapsedSeconds))
		for _, stream := range []struct{ name, text string }{{"stdout", r.Stdout}, {"stderr", r.Stderr}} {
			if text := pystr.Strip(stream.text); text != "" {
				fmt.Fprintf(w, "      %s: %s\n", stream.name, hookTruncate(text, 400))
			}
		}
	}
	if len(r.Parsed) > 0 {
		encoded, err := pyjson.Dumps(r.Parsed)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "      parsed (Icebath wire shape): %s\n", encoded)
	} else {
		fmt.Fprintln(w, "      parsed: <none — hook contributed nothing to the dispatcher>")
	}
	return nil
}

// Text cut to n code points with an ellipsis when longer
func hookTruncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-3]) + "..."
}

// Removes every allowlist entry of a command and says how long running processes keep it
func hooksRevoke(ctx context.Context, args HooksArgs, w io.Writer) error {
	removed, err := RevokeShellHook(ctx, args.Command)
	if err != nil {
		return err
	}
	if removed == 0 {
		fmt.Fprintf(w, "No allowlist entry found for command: %s\n", args.Command)
		return nil
	}
	fmt.Fprintf(w, "Removed %d allowlist entry/entries for: %s\n", removed, args.Command)
	fmt.Fprintln(w, "Note: a running gateway stops running it within about 30 seconds (unless "+
		"--accept-hooks / ICEBATH_ACCEPT_HOOKS / hooks_auto_accept approves every hook there); "+
		"other running processes keep it until they restart.")
	return nil
}

// Checks every configured hook for executability, consent, drift and JSON output and counts the issues
func hooksDoctor(ctx context.Context, _ HooksArgs, w io.Writer) error {
	cfg, err := loadConfig(home.Dir(ctx))
	if err != nil {
		return err
	}
	specs := IterConfiguredShellHooks(cfg)
	if len(specs) == 0 {
		fmt.Fprintln(w, "No shell hooks configured — nothing to check.")
		return nil
	}
	fmt.Fprintf(w, "Checking %d configured shell hook(s)...\n\n", len(specs))
	problems := 0
	for _, spec := range specs {
		fmt.Fprintf(w, "  [%s] %s\n", spec.Event, spec.Command)
		n, err := hookDoctorOne(ctx, spec, w)
		if err != nil {
			return err
		}
		problems += n
		fmt.Fprintln(w)
	}
	if problems > 0 {
		fmt.Fprintf(w, "%d issue(s) found.  Fix before relying on these hooks.\n", problems)
	} else {
		fmt.Fprintln(w, "All shell hooks look healthy.")
	}
	return nil
}

// Issues of one hook, the JSON smoke run reserved for an allowlisted executable script
func hookDoctorOne(ctx context.Context, spec *ShellHookSpec, w io.Writer) (int, error) {
	problems := 0
	if ShellHookScriptIsExecutable(spec.Command) {
		fmt.Fprintln(w, "      ✓ script exists and is executable")
	} else {
		problems++
		fmt.Fprintln(w, "      ✗ script missing or not executable (chmod +x the file, or fix the path)")
	}
	entry := ShellHookAllowlistEntry(ctx, spec.Event, spec.Command)
	if len(entry) > 0 {
		approvedAt, ok := entry["approved_at"]
		if !ok {
			approvedAt = "?"
		}
		fmt.Fprintf(w, "      ✓ allowlisted (approved %s)\n", pyrepr.Str(approvedAt))
	} else {
		problems++
		fmt.Fprintln(w, "      ✗ not allowlisted — hook will NOT fire at runtime (run with --accept-hooks once, or confirm at the TTY prompt)")
	}
	if len(entry) > 0 && config.PyTruthy(entry["script_mtime_at_approval"]) {
		drift, at, now, err := hookMtimeDrift(spec, entry)
		if err != nil {
			return problems, err
		}
		if drift == 1 {
			problems++
			fmt.Fprintf(w, "      ⚠ script modified since approval (was %s, now %s) — review changes, then `icebath hooks revoke` + re-approve to refresh\n", at, now)
		} else if drift == 0 {
			fmt.Fprintln(w, "      ✓ script unchanged since approval")
		}
	}
	if len(entry) == 0 {
		fmt.Fprintln(w, "      ℹ skipped JSON smoke test — not allowlisted yet. Approve the hook first (via TTY prompt or --accept-hooks), then re-run `icebath hooks doctor`.")
		return problems, nil
	}
	if !ShellHookScriptIsExecutable(spec.Command) {
		return problems, nil
	}
	payload, ok := defaultHookPayload(spec.Event)
	if !ok {
		payload = map[string]any{"extra": map[string]any{}}
	}
	result, err := RunShellHookOnce(ctx, spec, payload)
	if err != nil {
		return problems, err
	}
	elapsed := pyrepr.Float(result.ElapsedSeconds)
	switch {
	case result.TimedOut:
		problems++
		fmt.Fprintf(w, "      ✗ timed out after %ss on synthetic payload (timeout=%ds)\n", elapsed, spec.Timeout)
	case result.Error != "":
		problems++
		fmt.Fprintf(w, "      ✗ execution error: %s\n", result.Error)
	default:
		rc := "None"
		if result.ReturnCode != nil {
			rc = fmt.Sprint(*result.ReturnCode)
		}
		stdout := pystr.Strip(result.Stdout)
		if stdout == "" {
			fmt.Fprintf(w, "      ✓ ran clean with empty stdout (exit=%s, %ss) — hook is observer-only\n", rc, elapsed)
		} else if _, err := pyjson.Loads(stdout); err == nil {
			fmt.Fprintf(w, "      ✓ produced valid JSON on synthetic payload (exit=%s, %ss)\n", rc, elapsed)
		} else {
			problems++
			fmt.Fprintf(w, "      ✗ stdout was not valid JSON (exit=%s, %ss): %s\n", rc, elapsed, hookTruncate(stdout, 120))
		}
	}
	return problems, nil
}
