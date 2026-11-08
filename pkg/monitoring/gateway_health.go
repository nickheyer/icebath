package monitoring

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// GatewayMetric is one exported gauge with its value and string attributes
type GatewayMetric struct {
	Name       string
	Value      any
	Attributes map[string]string
}

// Predicate true when the text holds any of the needles
func containsAny(needles ...string) func(string) bool {
	return func(text string) bool {
		for _, n := range needles {
			if strings.Contains(text, n) {
				return true
			}
		}
		return false
	}
}

// Stable opaque sha256 key of a source id that never exports the id itself
func safeInstanceID(raw any) string {
	if !pyrepr.Truthy(raw) {
		raw = "unknown"
	}
	sum := sha256.Sum256(pystr.EncodeUTF8Replace(pyrepr.Str(raw)))
	return "sha256:" + hex.EncodeToString(sum[:])[:24]
}

var (
	runningPlatformStates = []string{"running", "connected", "ok", "ready"}
	fatalStateList        = []string{"fatal", "degraded", "error", "failed"}
	fatalPlatformStates   = stateSet(fatalStateList)
	knownGatewayStates    = stateSet(runningPlatformStates, fatalStateList, []string{"starting", "draining", "stopping", "stopped", "startup_failed", "unknown"})
	knownPlatformStates   = stateSet(runningPlatformStates, fatalStateList, []string{"connecting", "disconnected", "disabled", "paused", "retrying", "unknown"})
)

// Membership set of every state in the groups
func stateSet(groups ...[]string) map[string]bool {
	out := map[string]bool{}
	for _, g := range groups {
		for _, s := range g {
			out[s] = true
		}
	}
	return out
}

type gatewayErrorRule struct {
	match func(string) bool
	label string
}

var gatewayErrorRules = []gatewayErrorRule{
	{containsAny("auth", "token", "unauthorized", "forbidden", "401", "403"), "auth_failed"},
	{func(s string) bool { return strings.Contains(s, "rate") && strings.Contains(s, "limit") }, "rate_limited"},
	{containsAny("timeout", "timed out"), "timeout"},
	{containsAny("network", "connection", "dns", "socket", "connect call failed", "failed to connect", "cannot connect",
		"unreachable", "name resolution"), "network_error"},
	{containsAny("config", "missing", "invalid"), "invalid_config"},
	{containsAny("startup"), "startup_failed"},
	{containsAny("fatal"), "platform_fatal"},
}

// Python str of a value, the default when the value is falsy
func strOr(raw any, def string) string {
	if pyrepr.Truthy(raw) {
		return pyrepr.Str(raw)
	}
	return def
}

// Bounded class of free form gateway error text, unknown when no rule matches
func ClassifyGatewayError(raw any) string {
	s := pystr.Lower(strOr(raw, ""))
	for _, rule := range gatewayErrorRules {
		if rule.match(s) {
			return rule.label
		}
	}
	return "unknown"
}

// Bounded operational class of free form shutdown text, nil when there is no reason to classify
func ClassifyExitReason(raw, state any, restartRequested bool) *string {
	label := func(s string) *string { return &s }
	if restartRequested {
		return label("restart_requested")
	}
	stateName := pystr.Lower(strOr(state, ""))
	if raw == nil && stateName != "startup_failed" {
		return nil
	}
	classified := ClassifyGatewayError(raw)
	if stateName == "startup_failed" {
		if classified != "unknown" {
			return label(classified)
		}
		return label("startup_failed")
	}
	text := pystr.Lower(strOr(raw, ""))
	if strings.Contains(text, "signal") || strings.Contains(text, "sigterm") || strings.Contains(text, "sigint") {
		return label("signal")
	}
	if stateName == "stopped" && (strings.Contains(text, "shutdown") || strings.Contains(text, "stop")) {
		return label("planned_stop")
	}
	return label(classified)
}

// Lowercased state when allowed, otherwise unknown
func boundedState(raw any, allowed map[string]bool) string {
	state := pystr.Lower(strOr(raw, "unknown"))
	if allowed[state] {
		return state
	}
	return "unknown"
}

// Bounded state that keeps an absent value nil instead of unknown
func optionalState(raw any, allowed map[string]bool) *string {
	if raw == nil {
		return nil
	}
	s := boundedState(raw, allowed)
	return &s
}

// True for the int conversion errors Python callers catch as TypeError or ValueError
func intConversionRejected(err error) bool {
	var pe *pyint.Error
	return errors.As(err, &pe) && (pe.Kind == "TypeError" || pe.Kind == "ValueError")
}

// Positive Python int of a pid value, nil when absent, unparseable or not positive
func coercePID(raw any) (any, error) {
	n, err := pyint.Of(raw)
	if err != nil {
		if intConversionRejected(err) {
			return nil, nil
		}
		return nil, err
	}
	if n.Sign() > 0 {
		return pyint.Native(n), nil
	}
	return nil, nil
}

// Runtime status contract parse of active_agents, assigned by the gateway package
var GatewayParseActiveAgents func(raw any) (any, error)

// Active agents through the gateway contract, else a non negative int with zero for unparseable values
func parseActiveAgents(raw any) (any, error) {
	if n, ok := gatewayParseActiveAgents(raw); ok {
		return n, nil
	}
	n, err := pyint.Of(raw)
	if err != nil {
		if intConversionRejected(err) {
			return 0, nil
		}
		return nil, err
	}
	if n.Sign() < 0 {
		return 0, nil
	}
	return pyint.Native(n), nil
}

// Gateway contract result, false when the contract is absent, fails or panics
func gatewayParseActiveAgents(raw any) (n any, ok bool) {
	defer func() {
		if recover() != nil {
			n, ok = nil, false
		}
	}()
	parse := GatewayParseActiveAgents
	if parse == nil {
		return nil, false
	}
	n, err := parse(raw)
	return n, err == nil
}

// Object value itself, an empty object for anything that is not a dict
func dictOrEmpty(raw any) *pyjson.Object {
	if o, ok := raw.(*pyjson.Object); ok && o != nil {
		return o
	}
	return pyjson.NewObject()
}

// Platforms mapping of a runtime status payload, empty when absent or malformed
func platformsOf(runtime *pyjson.Object) *pyjson.Object {
	if runtime == nil {
		return pyjson.NewObject()
	}
	v, _ := runtime.Get("platforms")
	return dictOrEmpty(v)
}

// Active profile name, default when unset or unreadable
func safeProfile() (name string) {
	defer func() {
		if recover() != nil {
			name = "default"
		}
	}()
	if name = profiles.GetActiveProfileName(context.Background()); name == "" {
		return "default"
	}
	return name
}

// Release base version of this build, unknown when unreadable
func safeVersion() (version string) {
	defer func() {
		if recover() != nil {
			version = "unknown"
		}
	}()
	info, err := updaterecord.GetVersionInfo()
	if err != nil {
		return "unknown"
	}
	return info.BaseVersion
}

// Equal optional states, both absent or both the same value
func sameState(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Gateway lifecycle transition events with the startup_failed diagnostic and the exit event
func lifecycleEvents(previous, current *pyjson.Object, profile, version string) ([]Event, error) {
	var prevState any
	if previous != nil {
		prevState, _ = previous.Get("gateway_state")
	}
	curState, _ := current.Get("gateway_state")
	oldState := optionalState(prevState, knownGatewayStates)
	newState := optionalState(curState, knownGatewayStates)
	if sameState(oldState, newState) || newState == nil {
		return nil, nil
	}
	restartRaw, _ := current.Get("restart_requested")
	restartRequested := pyrepr.Truthy(restartRaw)
	health := func(name string) (Event, error) {
		exitRaw, _ := current.Get("exit_reason")
		activeRaw, ok := current.Get("active_agents")
		if !ok {
			activeRaw = 0
		}
		active, err := parseActiveAgents(activeRaw)
		if err != nil {
			return nil, err
		}
		pidRaw, _ := current.Get("pid")
		pid, err := coercePID(pidRaw)
		if err != nil {
			return nil, err
		}
		ev := NewGatewayHealthEvent(name)
		ev.GatewayState, ev.OldState, ev.NewState = newState, oldState, newState
		ev.ExitReason = ClassifyExitReason(exitRaw, *newState, restartRequested)
		ev.RestartRequested = &restartRequested
		ev.ActiveAgents = active
		ev.Profile, ev.Version = &profile, &version
		ev.PID = pid
		return ev, nil
	}
	first, err := health("gateway.lifecycle")
	if err != nil {
		return nil, err
	}
	out := []Event{first}
	if *newState == "startup_failed" {
		exitRaw, _ := current.Get("exit_reason")
		if !pyrepr.Truthy(exitRaw) {
			exitRaw = "startup_failed"
		}
		errorClass := ClassifyGatewayError(exitRaw)
		diag := NewGatewayDiagnosticEvent("gateway.startup_failed", "gateway")
		diag.ErrorClass, diag.ErrorCode = errorClass, &errorClass
		diag.Profile, diag.Version = &profile, &version
		diag.Severity = "error"
		out = append(out, diag)
	}
	if *newState == "stopped" {
		exit, err := health("gateway.exit")
		if err != nil {
			return nil, err
		}
		out = append(out, exit)
	}
	return out, nil
}

// Per platform state_change diagnostics plus platform.fatal when the new state is fatal
func platformEvents(previous, current *pyjson.Object, profile, version string) []Event {
	oldPlatforms := platformsOf(previous)
	currentPlatforms := platformsOf(current)
	var out []Event
	for _, platform := range currentPlatforms.Keys {
		pv, _ := currentPlatforms.Get(platform)
		pdata := dictOrEmpty(pv)
		ov, ok := oldPlatforms.Get(platform)
		if !ok {
			ov = pyjson.NewObject()
		}
		prev := dictOrEmpty(ov)
		prevRaw, _ := prev.Get("state")
		curRaw, _ := pdata.Get("state")
		oldState := optionalState(prevRaw, knownPlatformStates)
		newState := optionalState(curRaw, knownPlatformStates)
		if sameState(oldState, newState) || newState == nil {
			continue
		}
		codeRaw, _ := pdata.Get("error_code")
		if !pyrepr.Truthy(codeRaw) {
			codeRaw, _ = pdata.Get("error_message")
		}
		errorCode := ClassifyGatewayError(codeRaw)
		severity := "warning"
		if *newState == "fatal" || *newState == "failed" || *newState == "error" {
			severity = "error"
		}
		name := platform
		common := func(event string) GatewayDiagnosticEvent {
			diag := NewGatewayDiagnosticEvent(event, "platform."+name)
			diag.Platform, diag.ErrorCode, diag.ErrorClass = &name, &errorCode, errorCode
			diag.Profile, diag.Version, diag.Severity = &profile, &version, severity
			return diag
		}
		change := common("platform.state_change")
		change.OldState, change.NewState = oldState, newState
		out = append(out, change)
		if fatalPlatformStates[*newState] {
			out = append(out, common("platform.fatal"))
		}
	}
	return out
}

// Emits content free gateway events for a runtime status change, failing open with a debug log
func EmitRuntimeStatusTransition(previous, current *pyjson.Object) {
	defer func() {
		if r := recover(); r != nil {
			slog.Debug(fmt.Sprintf("gateway runtime status transition emit failed: %v", r))
		}
	}()
	profile, version := safeProfile(), safeVersion()
	events, err := lifecycleEvents(previous, current, profile, version)
	if err != nil {
		slog.Debug(fmt.Sprintf("gateway runtime status transition emit failed: %v", err))
		return
	}
	events = append(events, platformEvents(previous, current, profile, version)...)
	for _, ev := range events {
		Emit(ev)
	}
}
