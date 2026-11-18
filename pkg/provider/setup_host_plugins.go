package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Context whose plugin choice prompts the host answers, a backed out prompt giving the plugin no choice
func bindHostedPluginAsker(ctx context.Context, ask SetupAsker) context.Context {
	return plugins.BindAsker(ctx, func(question string, labels []string) (string, bool) {
		picked := askHostedChoice(ask, question, labels, 0, "", nil, -1)
		if picked < 0 {
			return "", false
		}
		return labels[picked], true
	})
}

// Signs in to an OAuth plugin provider inside the TUI the way auth add does, true when the sign in finished
func hostedPluginSignIn(r *ModelFlowRun, ask SetupAsker, providerID, name string, profile Profile) bool {
	flowSay(r.Out, fmt.Sprintf("  ⚠ Not signed in to %s.", name))
	device := profile.StartDeviceLogin != nil && profile.PollDeviceLogin != nil && profile.SaveDeviceLogin != nil
	if !device && pluginMissingAuthHandlerError(providerID, "add") != nil {
		flowSay(r.Out, fmt.Sprintf("  The %s plugin declares %s sign in but ships no auth_handler, so Icebath cannot sign in to it.",
			name, registryAuthType(profile)))
		return false
	}
	if askHostedChoice(ask, name+" sign in", []string{"Sign in to " + name + " in the browser", "Cancel"}, 0, "", nil, -1) != 0 {
		fmt.Fprintln(r.Out, flowNoChange)
		return false
	}
	env := r.Env
	env.Ctx = r.ctx()
	var err error
	if device {
		err = hostedProfileDeviceLogin(env.Ctx, r, name, profile)
	} else {
		err = withPluginSignInEnv(env, func() error {
			return (&authCLI{ctx: env.Ctx, env: env, ns: map[string]any{"provider": providerID}, out: r.Out, errw: r.Out}).add()
		})
	}
	switch {
	case env.Ctx.Err() != nil || isSignInCancelled(err):
		fmt.Fprintln(r.Out, "Sign-in was cancelled.")
		return false
	case err != nil:
		reportHostedSignIn(r.Out, err, name)
		return false
	}
	return true
}

// Copy of a profile device login that ended refused or expired, keyed by the status its poll reported
var deviceLoginEndings = map[string]string{"denied": "Authorization was denied.", "expired": "Device code expired before approval"}

// Python int of a device login number, a string parsed like int() and any other shape refused
func deviceLoginInt(v any) (int, error) {
	if s, ok := v.(string); ok {
		return parsePyInt(s)
	}
	if n, ok := anyInt(v); ok {
		return n, nil
	}
	return 0, fmt.Errorf("a device login value is %s, expected a whole number", errchain.Kind(v))
}

// Required field of a device login answer, a KeyError when the profile left it out
func deviceLoginField(login map[string]any, key string) (any, error) {
	v, ok := login[key]
	if !ok {
		return nil, fmt.Errorf("the device login reply has no %s", key)
	}
	return v, nil
}

// Device code sign in through the profile's hooks, polled at its pace until it ends or the flow stops
func hostedProfileDeviceLogin(ctx context.Context, r *ModelFlowRun, name string, profile Profile) error {
	started, err := profile.StartDeviceLogin(ctx)
	if err != nil {
		return err
	}
	login := hostedMapOrNil(started)
	if len(login) == 0 {
		return authExit(fmt.Sprintf("Provider %s does not support device-code flow", name))
	}
	fields := map[string]any{}
	for _, key := range []string{"user_code", "verification_url", "expires_in", "interval"} {
		if fields[key], err = deviceLoginField(login, key); err != nil {
			return err
		}
	}
	expiresIn, err := deviceLoginInt(fields["expires_in"])
	if err != nil {
		return err
	}
	interval, err := deviceLoginInt(fields["interval"])
	if err != nil {
		return err
	}
	interval = max(1, interval)
	if err := printDeviceCodeInstructions(r.Out, hostedStr(fields["verification_url"]), hostedStr(fields["user_code"]), !isRemoteSession(), "—", true); err != nil {
		return err
	}
	fmt.Fprintln(r.Out, "Waiting for approval...")
	state := login["state"]
	deadline := time.Now().Add(time.Duration(expiresIn) * time.Second)
	cancelled := func() bool { return ctx.Err() != nil }
	result := map[string]any{}
	for time.Now().Before(deadline) {
		if cancellableSleep(time.Duration(interval)*time.Second, cancelled) != nil {
			return &SignInCancelledError{ExitCode: 130, HasCode: true}
		}
		polled, err := profile.PollDeviceLogin(ctx, state)
		if err != nil {
			return err
		}
		result = hostedMap(polled)
		if next, ok := result["state"]; ok {
			state = next
		}
		if hostedStr(result["status"]) != "pending" {
			break
		}
		if truthy(result["interval"]) {
			if interval, err = deviceLoginInt(result["interval"]); err != nil {
				return err
			}
		}
	}
	if cancelled() {
		return &SignInCancelledError{ExitCode: 130, HasCode: true}
	}
	status := hostedStr(result["status"])
	if status == "" || status == "pending" {
		status = "expired"
	}
	if status != "approved" {
		message := hostedStr(result["message"])
		if message == "" {
			message = deviceLoginEndings[status]
		}
		if message == "" {
			message = "Sign-in failed"
		}
		return authExit(message)
	}
	if err := profile.SaveDeviceLogin(ctx, result["credential"]); err != nil {
		return err
	}
	fmt.Fprintf(r.Out, "Signed in to %s.\n", name)
	return nil
}

// Row of the TUI preset list that builds or edits a preset where a terminal runs moa configure
const hostedMoAConfigureRow = "Configure a preset (pick its reference and aggregator models)"

// Builds or edits the named preset as moa configure does, then reopens the preset list with it saved
func hostedMoAConfigure(r *ModelFlowRun) error {
	name, ok := flowAsk(r.Out, "  Preset name to create or edit: ", flowAskOptions{CancelMsg: optStr(flowNoChange)})
	if !ok {
		return nil
	}
	if name == "" {
		fmt.Fprintln(r.Out, flowNoChange)
		return nil
	}
	cfg, err := r.loadConfig()
	if err != nil {
		return err
	}
	if err := moaCmdConfigure(r.ctx(), r.Out, cfg, MoAArgs{Command: "configure", Name: name}); err != nil {
		return err
	}
	if cfg, err = r.loadConfig(); err != nil {
		return err
	}
	r.Config = cfg
	return modelFlowMoA(r)
}
