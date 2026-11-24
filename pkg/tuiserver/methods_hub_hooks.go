package tuiserver

import (
	"context"
	"errors"

	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

var hubHooksRegistry = &handlerRegistry{}

func init() {
	scopedRPC(hubHooksRegistry, tuigateway.HooksList.Name(), 5193, rpcShape{}, hooksList)
	scopedRPC(hubHooksRegistry, tuigateway.HooksChange.Name(), 5193, rpcShape{}, hooksChange)
	scopedRPC(hubHooksRegistry, tuigateway.HooksTest.Name(), 5193, rpcShape{}, hooksTest)
	scopedRPC(hubHooksRegistry, tuigateway.HooksCheck.Name(), 5193, rpcShape{}, hooksCheck)
	addLongHandlers(tuigateway.HooksTest.Name(), tuigateway.HooksCheck.Name())
	hubHooksRegistry.install()
}

// Answer for a failed hooks request: a refusal of what was asked goes back as its message, anything else as the failure it is
func hooksFailed(rid any, err error) (map[string]any, error) {
	var refused *plugins.HookRefusal
	if errors.As(err, &refused) {
		return rpcErr(rid, 4193, refused.Message, nil), nil
	}
	return nil, err
}

// Every shell hook and outbound webhook of the profile as the hooks screen shows it
func hooksListing(ctx context.Context) (tuigateway.HooksListResult, error) {
	hooks, webhooks, err := plugins.HooksOverview(ctx)
	if err != nil {
		return tuigateway.HooksListResult{}, err
	}
	out := tuigateway.HooksListResult{Hooks: make([]tuigateway.HookRow, 0, len(hooks)), Webhooks: make([]tuigateway.WebhookRow, 0, len(webhooks))}
	out.Events = plugins.ShellHookEvents()
	for _, hook := range hooks {
		out.Hooks = append(out.Hooks, tuigateway.HookRow{Event: hook.Event, Command: hook.Command, Matcher: hook.Matcher, Timeout: int64(hook.Timeout),
			FailClosed: hook.FailClosed, Allowed: hook.Allowed, ApprovedAt: hook.ApprovedAt, Changed: hook.Drift == 1, ChangedWas: hook.DriftWas, ChangedNow: hook.DriftNow})
	}
	for _, target := range webhooks {
		out.Webhooks = append(out.Webhooks, tuigateway.WebhookRow{Label: target.Label, URL: target.URL, Events: target.Events, Matcher: target.Matcher,
			Timeout: int64(target.Timeout), Signed: target.Signed})
	}
	return out, nil
}

// Shell hooks and outbound webhooks of the profile for the hooks screen
func hooksList(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	if _, err := tuigateway.HooksList.Params(raw); err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	out, err := hooksListing(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// One change to the hooks, answered with the hooks after it
func hooksChange(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.HooksChange.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	switch p.Verb {
	case "add":
		err = plugins.AddShellHook(ctx, p.Event, p.Command, p.Matcher, int(p.Timeout))
	case "remove":
		var removed int
		if removed, err = plugins.RemoveShellHook(ctx, p.Event, p.Command); err == nil && removed == 0 {
			err = &plugins.HookRefusal{Message: "no such hook: " + p.Event + " " + p.Command}
		}
	case "allow":
		err = plugins.ApproveShellHook(ctx, p.Event, p.Command)
	case "revoke":
		_, err = plugins.RevokeShellHook(ctx, p.Command)
	default:
		err = &plugins.HookRefusal{Message: "unknown hooks change: " + p.Verb}
	}
	if err != nil {
		return hooksFailed(rid, err)
	}
	out, err := hooksListing(ctx)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(out)), nil
}

// How one run of a hook's script ended, as the hooks screen shows it
func hookRun(result *plugins.ShellHookResult) (*tuigateway.HookRun, error) {
	directive, err := plugins.ShellHookDirective(result)
	if err != nil {
		return nil, err
	}
	run := &tuigateway.HookRun{Error: result.Error, TimedOut: result.TimedOut, Seconds: result.ElapsedSeconds, Stdout: result.Stdout, Stderr: result.Stderr, Directive: directive}
	if result.ReturnCode != nil {
		run.Exited, run.ExitCode = true, int64(*result.ReturnCode)
	}
	return run, nil
}

// Runs one hook on a sample event
func hooksTest(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.HooksTest.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	result, err := plugins.TestShellHook(ctx, p.Event, p.Command, p.Tool)
	if err != nil {
		return hooksFailed(rid, err)
	}
	run, err := hookRun(result)
	if err != nil {
		return nil, err
	}
	return ok(rid, tuigateway.Plain(*run)), nil
}

// Checks one hook: whether its script can run, is allowed, is unchanged and answers well
func hooksCheck(ctx context.Context, rid any, raw map[string]any, _ *tuihost.Session) (map[string]any, error) {
	p, err := tuigateway.HooksCheck.Params(raw)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil), nil
	}
	check, err := plugins.CheckShellHook(ctx, p.Event, p.Command)
	if err != nil {
		return hooksFailed(rid, err)
	}
	out := tuigateway.HooksCheckResult{Executable: check.Executable, Allowed: check.Allowed, ApprovedAt: check.ApprovedAt, Compared: check.DriftChecked,
		Changed: check.DriftChecked && check.Drift == 1, ChangedWas: check.DriftWas, ChangedNow: check.DriftNow, Output: check.Output, Problems: int64(check.Problems)}
	if check.Run != nil {
		if out.Run, err = hookRun(check.Run); err != nil {
			return nil, err
		}
	}
	return ok(rid, tuigateway.Plain(out)), nil
}
