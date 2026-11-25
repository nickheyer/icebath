package plugins

import (
	"context"
	"strings"

	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// HookView is one configured shell hook with its consent state
type HookView struct {
	Event, Command, Matcher string
	Timeout                 int
	FailClosed              bool
	// On the allowlist, so it runs
	Allowed bool
	// When it was allowed, empty when the allowlist does not say
	ApprovedAt string
	// 1 when the script changed since it was allowed, 0 when it did not, -1 when that cannot be told
	Drift              int
	DriftWas, DriftNow string
}

// WebhookView is one outbound webhook target
type WebhookView struct {
	Label, URL, Matcher string
	Events              []string
	Timeout             int
	Signed              bool
}

// HookCheck is what checking one configured hook found
type HookCheck struct {
	Executable bool
	Allowed    bool
	// When it was allowed, a question mark when the allowlist does not say
	ApprovedAt string
	// Whether the allowlist holds a script time to compare, with Drift as in HookView when it does
	DriftChecked       bool
	Drift              int
	DriftWas, DriftNow string
	// Run on a sample event, nil for a hook that is not allowed or cannot run
	Run *ShellHookResult
	// What a finished run printed: empty, json or invalid
	Output string
	// How many of the findings are problems
	Problems int
}

// HookRefusal is a change to the hooks refused for what was asked
type HookRefusal struct{ Message string }

func (e *HookRefusal) Error() string { return e.Message }

// ShellHookEvents is every event a shell hook may be set on
func ShellHookEvents() []string {
	var events []string
	for _, event := range ValidHooks() {
		if !shellUnsupportedHooks[event] {
			events = append(events, event)
		}
	}
	return events
}

// HooksOverview is every configured shell hook with its consent state and every outbound webhook target of the home in the context
func HooksOverview(ctx context.Context) ([]HookView, []WebhookView, error) {
	specs := IterConfiguredShellHooks(ctx)
	outbound, err := IterConfiguredOutboundWebhooks(ctx)
	if err != nil {
		return nil, nil, err
	}
	hooks := make([]HookView, 0, len(specs))
	var approved map[[2]string]bool
	if len(specs) > 0 {
		approved = AllowlistedShellHookPairs(ctx)
	}
	for _, spec := range specs {
		view := HookView{Event: spec.Event, Command: spec.Command, Matcher: spec.Matcher, Timeout: spec.Timeout, FailClosed: spec.FailClosed,
			Allowed: approved[[2]string{spec.Event, spec.Command}], Drift: -1}
		if view.Allowed {
			if entry := ShellHookAllowlistEntry(ctx, spec.Event, spec.Command); len(entry) > 0 && anyutil.IsTrue(entry["approved_at"]) {
				view.ApprovedAt = anyutil.String(entry["approved_at"])
				if view.Drift, view.DriftWas, view.DriftNow, err = hookMtimeDrift(spec, entry); err != nil {
					return nil, nil, err
				}
			}
		}
		hooks = append(hooks, view)
	}
	targets := make([]WebhookView, 0, len(outbound))
	for _, target := range outbound {
		targets = append(targets, WebhookView{Label: target.Label(), URL: target.URL, Matcher: target.Matcher, Events: target.Events,
			Timeout: target.Timeout, Signed: target.Secret != ""})
	}
	return hooks, targets, nil
}

// Configured hook of an event and command in the home in the context, nil when the config holds none
func configuredShellHook(ctx context.Context, event, command string) (*ShellHookSpec, error) {
	for _, spec := range IterConfiguredShellHooks(ctx) {
		if spec.Event == event && spec.Command == command {
			return spec, nil
		}
	}
	return nil, nil
}

// Sample payload of an event for a test run, naming the tool when one is given
func sampleHookPayload(event, forTool string) map[string]any {
	payload, ok := defaultHookPayload(event)
	if !ok {
		payload = map[string]any{"session_id": "test-session"}
	}
	if forTool != "" {
		payload["tool_name"] = forTool
	}
	return payload
}

// ApproveShellHook puts a configured hook on the allowlist with the time of its script now, so it runs
func ApproveShellHook(ctx context.Context, event, command string) error {
	spec, err := configuredShellHook(ctx, event, command)
	if err != nil {
		return err
	}
	if spec == nil {
		return &HookRefusal{Message: "no such hook: " + event + " " + command}
	}
	return recordShellHookApproval(ctx, spec.Event, spec.Command)
}

// TestShellHook runs a configured hook once on a sample event, naming the tool when one is given
func TestShellHook(ctx context.Context, event, command, forTool string) (*ShellHookResult, error) {
	spec, err := configuredShellHook(ctx, event, command)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, &HookRefusal{Message: "no such hook: " + event + " " + command}
	}
	return RunShellHookOnce(ctx, spec, sampleHookPayload(event, forTool))
}

// ShellHookDirective is the answer a run handed back in the wire shape, empty when it answered nothing
func ShellHookDirective(result *ShellHookResult) (string, error) {
	if len(result.Parsed) == 0 {
		return "", nil
	}
	return jsonutil.Encode(result.Parsed, jsonutil.Spaced(), jsonutil.ASCII())
}

// What checking one hook finds: whether its script can run, whether it is allowed and unchanged, and how a run on a sample event of an allowed, runnable hook ends
func checkShellHook(ctx context.Context, spec *ShellHookSpec) (*HookCheck, error) {
	check := &HookCheck{Executable: ShellHookScriptIsExecutable(spec.Command), Drift: -1}
	if !check.Executable {
		check.Problems++
	}
	entry := ShellHookAllowlistEntry(ctx, spec.Event, spec.Command)
	check.Allowed = len(entry) > 0
	if !check.Allowed {
		check.Problems++
		return check, nil
	}
	approvedAt, ok := entry["approved_at"]
	if !ok || approvedAt == nil {
		approvedAt = "?"
	}
	check.ApprovedAt = anyutil.String(approvedAt)
	if anyutil.IsTrue(entry["script_mtime_at_approval"]) {
		var err error
		if check.Drift, check.DriftWas, check.DriftNow, err = hookMtimeDrift(spec, entry); err != nil {
			return check, err
		}
		check.DriftChecked = true
		if check.Drift == 1 {
			check.Problems++
		}
	}
	if !check.Executable {
		return check, nil
	}
	payload, ok := defaultHookPayload(spec.Event)
	if !ok {
		payload = map[string]any{"extra": map[string]any{}}
	}
	result, err := RunShellHookOnce(ctx, spec, payload)
	if err != nil {
		return check, err
	}
	check.Run = result
	switch stdout := strings.TrimSpace(result.Stdout); {
	case result.TimedOut, result.Error != "":
		check.Problems++
	case stdout == "":
		check.Output = "empty"
	default:
		if _, err := jsonutil.Decode(stdout); err == nil {
			check.Output = "json"
		} else {
			check.Output = "invalid"
			check.Problems++
		}
	}
	return check, nil
}

// CheckShellHook checks a configured hook of the home in the context
func CheckShellHook(ctx context.Context, event, command string) (*HookCheck, error) {
	spec, err := configuredShellHook(ctx, event, command)
	if err != nil {
		return nil, err
	}
	if spec == nil {
		return nil, &HookRefusal{Message: "no such hook: " + event + " " + command}
	}
	return checkShellHook(ctx, spec)
}

// AddShellHook appends a hook to the hooks of an event in the user config, the tools it is for and the seconds it may take left out when not given
func AddShellHook(ctx context.Context, event, command, matcher string, timeout int) error {
	command, matcher = strings.TrimSpace(command), strings.TrimSpace(matcher)
	switch {
	case !validHooks[event] || shellUnsupportedHooks[event]:
		return &HookRefusal{Message: "unknown hook event: " + event}
	case command == "":
		return &HookRefusal{Message: "a hook needs a command"}
	case matcher != "" && !shellHookToolEvents[event]:
		return &HookRefusal{Message: "only hooks before and after a tool call may name tools"}
	case timeout < 0 || timeout > ShellHookMaxTimeoutSeconds:
		return &HookRefusal{Message: "a hook may take at most " + anyutil.String(int64(ShellHookMaxTimeoutSeconds)) + " seconds"}
	}
	entry := map[string]any{"command": command}
	if matcher != "" {
		entry["matcher"] = matcher
	}
	if timeout > 0 {
		entry["timeout"] = timeout
	}
	list, _ := settings.Hooks.Get(ctx)[event].([]any)
	_, err := settings.Hooks.SetEntry(ctx, event, append(append([]any{}, list...), entry))
	return err
}

// RemoveShellHook takes every hook running the command out of the hooks of an event in the user config and reports how many went
func RemoveShellHook(ctx context.Context, event, command string) (int, error) {
	list, _ := settings.Hooks.Get(ctx)[event].([]any)
	kept := make([]any, 0, len(list))
	for _, raw := range list {
		if text, _ := asMap(raw)["command"].(string); strings.TrimSpace(text) != command {
			kept = append(kept, raw)
		}
	}
	removed := len(list) - len(kept)
	if removed == 0 {
		return 0, nil
	}
	var err error
	if len(kept) == 0 {
		_, err = settings.Hooks.DeleteEntry(ctx, event)
	} else {
		_, err = settings.Hooks.SetEntry(ctx, event, kept)
	}
	if err != nil {
		return 0, err
	}
	return removed, nil
}
