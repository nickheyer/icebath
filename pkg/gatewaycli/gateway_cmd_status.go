package gatewaycli

import (
	"context"
	"fmt"
	"io"
	"sort"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewayrun"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/profiles/manage"
)

// Operator wording of the out of loop watchdog exit reasons
var watchdogExitReasons = map[string]string{
	"loop_liveness_watchdog": fmt.Sprintf("event loop stopped dispatching (housekeeping, cron and the kanban dispatcher froze); "+
		"the liveness watchdog exited with code %d for the supervisor to restart it", gateway.GatewayServiceRestartExitCode),
	"shutdown_watchdog": "shutdown drain wedged; the shutdown watchdog forced the exit (see logs/gateway-shutdown-watchdog.log)",
}

// Summary of the persisted gateway runtime health, fatal platforms and stale, draining or failed states
func runtimeHealthLines(ctx context.Context) ([]string, error) {
	state := gateway.ReadRuntimeStatus("")
	if state == nil || state.Len() == 0 {
		return nil, nil
	}
	gatewayState := state.GetOr("gateway_state", nil)
	exitReason := state.GetOr("exit_reason", nil)
	lines := []string{}
	if platforms, ok := state.GetOr("platforms", nil).(*pyjson.Object); ok {
		for _, name := range platforms.Keys {
			pdata, _ := platforms.Vals[name].(*pyjson.Object)
			if pdata == nil || pdata.GetOr("state", nil) != "fatal" {
				continue
			}
			message := "unknown error"
			if v := pdata.GetOr("error_message", nil); pyrepr.Truthy(v) {
				message = pyrepr.Str(v)
			}
			lines = append(lines, fmt.Sprintf("⚠ %s: %s", name, message))
		}
	}
	stateText, _ := gatewayState.(string)
	switch stateText {
	case "running", "degraded", "starting", "draining":
		if gateway.RuntimeStatusIsStale(state, gateway.RuntimeStatusStaleTTL) {
			if !gateway.RuntimeStatusPidIsLive(state) {
				lines = append(lines, fmt.Sprintf("⚠ Stale gateway_state.json: recorded state '%s' but the "+
					"recorded process is gone (likely an ungraceful shutdown)", stateText))
				return lines, nil
			}
			age, ok, err := gateway.RuntimeStatusHeartbeatAgeS(state)
			if err != nil {
				return nil, err
			}
			if stateText != "draining" && ok {
				lines = append(lines, fmt.Sprintf("⚠ Gateway heartbeat stale: housekeeping has not refreshed gateway_state.json for %d s "+
					"(event loop or housekeeping wedged; pid %s alive) — restart the gateway", age, pyrepr.Str(state.GetOr("pid", nil))))
			}
		}
	}
	reasonText := ""
	if pyrepr.Truthy(exitReason) {
		reasonText = pyrepr.Str(exitReason)
	}
	switch {
	case stateText == "startup_failed" && reasonText != "":
		lines = append(lines, "⚠ Last startup issue: "+reasonText)
	case stateText == "degraded" && reasonText != "":
		described := reasonText
		if key, isStr := exitReason.(string); isStr {
			if known, ok := watchdogExitReasons[key]; ok {
				described = known
			}
		}
		lines = append(lines, "⚠ Gateway exited degraded: "+described)
	case stateText == "draining":
		action := "shutdown"
		if pyrepr.Truthy(state.GetOr("restart_requested", nil)) {
			action = "restart"
		}
		count, err := gateway.ParseActiveAgents(state.GetOr("active_agents", nil))
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("⏳ Gateway draining for %s (%s active agent(s))", action, pyrepr.Str(count)))
		if work, ok := state.GetOr("active_work", nil).([]any); ok && len(work) > 0 {
			for _, u := range work {
				if _, isDict := u.(*pyjson.Object); !isDict {
					continue
				}
				text, err := DescribeActiveWorkUnit(ctx, u, "")
				if err != nil {
					return nil, err
				}
				lines = append(lines, "     • "+text)
			}
		}
	case stateText == "stopped" && reasonText != "":
		lines = append(lines, "⚠ Last shutdown reason: "+reasonText)
	}
	return lines, nil
}

// Prints the recent health block when the runtime state has anything to report
func printRuntimeHealth(ctx context.Context, w io.Writer) error {
	lines, err := runtimeHealthLines(ctx)
	if err != nil || len(lines) == 0 {
		return err
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Recent gateway health:")
	for _, line := range lines {
		fmt.Fprintf(w, "  %s\n", line)
	}
	return nil
}

// Warns when a gateway process runs for this profile while its service is not active
func printGatewayProcessMismatch(ctx context.Context, w io.Writer, snapshot GatewayRuntimeSnapshot) {
	if !snapshot.HasProcessServiceMismatch() {
		return
	}
	fmt.Fprintln(w)
	pidsLine := "  PID(s): " + FormatGatewayPids(snapshot.GatewayPIDs, -1)
	if gateway.LaunchdUnsupportedMarkerExists(ctx) {
		printLines(w, "⚠ Gateway is running as a detached fallback process — launchd cannot supervise it", pidsLine,
			"  Auto-start at login and auto-restart on crash are NOT available.", "  Stop it with: icebath gateway stop")
		return
	}
	printLines(w, "⚠ Gateway process is running for this profile, but the service is not active", pidsLine,
		"  This is usually a manual foreground/tmux/nohup run, so `icebath gateway`",
		"  can refuse to start another copy until this process stops.")
}

// Prints the remedy when the boot guard kept an unset default gateway standalone
func printMultiplexStandaloneReason(ctx context.Context, w io.Writer) {
	printLines(w, gateway.RecordedStandaloneWarningLines(ctx)...)
}

// Prints callback URLs of inbound port platforms the live multiplexer serves for secondary profiles
func printServedIngressURLs(w io.Writer, profile string) {
	urls, err := gateway.ServedProfileIngressURLs(profile)
	if err != nil || urls == nil || urls.Len() == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Inbound callback URLs on the shared listener:")
	names := append([]string{}, urls.Keys...)
	sort.Strings(names)
	for _, name := range names {
		perPlatform := map[string]string{}
		if obj, ok := urls.Vals[name].(*pyjson.Object); ok {
			for _, k := range obj.Keys {
				perPlatform[k] = pyrepr.Str(obj.Vals[k])
			}
		}
		indent := "  "
		if profile == "" {
			indent = "  " + name + "/"
		}
		printLines(w, gateway.FormatIngressURLLines(perPlatform, indent)...)
	}
}

// Prints the shared ingress platforms this served profile enabled that only the default profile runs
func printUnservedSharedIngress(w io.Writer, profile string) {
	unserved, err := gateway.ServedProfileUnservedPlatforms(profile)
	if err != nil || unserved == nil || unserved.Len() == 0 {
		return
	}
	fmt.Fprintln(w)
	names := append([]string{}, unserved.Keys...)
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  ⚠ %s: %s\n", name, pyrepr.Str(unserved.Vals[name]))
	}
	fmt.Fprintln(w, "  Enable it on the default profile (shared ingress serves every profile), or disable it here.")
}

// Prints other profiles' running gateways under the status report
func printOtherProfilesGatewayStatus(ctx context.Context, w io.Writer) {
	current := profiles.GetActiveProfileName(ctx)
	procs, err := FindProfileGatewayProcesses(ctx, nil, false)
	if err != nil {
		return
	}
	others := []ProfileGatewayProcess{}
	for _, p := range procs {
		if p.Profile != current {
			others = append(others, p)
		}
	}
	if len(others) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Other profiles:")
	for _, p := range others {
		fmt.Fprintf(w, "  ✓ %-16s — PID %d\n", p.Profile, p.PID)
	}
}

// Prints the migrate preflight duplicate credential findings as status warnings
func PrintDuplicateCredentialWarnings(ctx context.Context, w io.Writer) {
	lines, err := gatewayrun.DuplicateCredentialFindings(ctx)
	if err != nil || len(lines) == 0 {
		return
	}
	fmt.Fprintln(w)
	for _, line := range lines {
		fmt.Fprintf(w, "⚠ %s\n", line)
	}
}

// Lists every profile and whether its gateway runs
func gatewayList(ctx context.Context, w io.Writer) error {
	list, err := manage.ListProfiles(ctx, false)
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(w, "No profiles found.")
		return nil
	}
	current := profiles.GetActiveProfileName(ctx)
	fmt.Fprintln(w, "Gateways:")
	for _, prof := range list {
		marker := "✗"
		if prof.GatewayRunning {
			marker = "✓"
		}
		label := prof.Name
		if prof.Name == current {
			label += " (current)"
		}
		line := fmt.Sprintf("  %s %-24s", marker, label)
		if prof.GatewayRunning {
			pid, ok, err := gateway.GetRunningPid(pypath.PathJoin(prof.Path, "gateway.pid"), false)
			switch {
			case err == nil && ok && pid != 0:
				line += fmt.Sprintf(" — PID %d", pid)
			default:
				name := prof.Name
				served, err := gateway.NamedProfileServedByRunningMultiplexerCtx(ctx, &name)
				if err != nil {
					return err
				}
				if served {
					line += " — served by the default multiplexer"
				}
			}
		} else {
			line += " — not running"
		}
		fmt.Fprintln(w, line)
	}
	return nil
}
