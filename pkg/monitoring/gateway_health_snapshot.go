package monitoring

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
)

// GatewayHealthSnapshot holds the gauges and events one runtime status read produces
type GatewayHealthSnapshot struct {
	Metrics []GatewayMetric
	Events  []Event
}

var (
	runningStateSet  = stateSet(runningPlatformStates)
	supervisionModes = stateSet([]string{"systemd", "s6", "container", "launchd", "manual", "unknown"})
	sourceLoggerRe   = regexp.MustCompile(`^gateway(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)

	// Runtime status contract derivations, assigned by the gateway package
	GatewayDeriveBusy      func(gatewayRunning bool, gatewayState, activeAgents any) (bool, error)
	GatewayDeriveDrainable func(gatewayRunning bool, gatewayState any) bool
)

func healthLog() *slog.Logger {
	return slog.Default().With("logger", "agent.monitoring.gateway_health")
}

// Bounded source controlled gateway logger name for the OTLP scope, nil for any other name
func SourceLoggerForExport(name any) *string {
	value := ""
	if pyrepr.Truthy(name) {
		value = pyrepr.Str(name)
	}
	if len([]rune(value)) <= 128 && sourceLoggerRe.MatchString(value) {
		return &value
	}
	return nil
}

// Redacted metric attribute value cut to limit characters, unknown when empty or unredactable
func safeMetricValue(raw any, limit int) string {
	return RedactBoundedWith(raw, limit, "unknown", "unknown")
}

// Subsystem of a gateway logger, the relay and each platform module named apart
func SubsystemForLogger(loggerName string) string {
	parts := strings.Split(loggerName, ".")
	if len(parts) >= 2 && parts[0] == "gateway" && parts[1] == "relay" {
		return "platform.relay"
	}
	if len(parts) >= 3 && parts[0] == "gateway" && parts[1] == "platforms" && parts[2] != "" {
		return "platform." + parts[2]
	}
	if strings.HasPrefix(loggerName, "gateway.platforms") {
		return "platform"
	}
	return "gateway"
}

// Platform named by a platform subsystem, nil for every other subsystem
func PlatformForSubsystem(subsystem string) *string {
	rest, ok := strings.CutPrefix(subsystem, "platform.")
	if !ok || rest == "" {
		return nil
	}
	return &rest
}

// Gateway busy flag through the status contract, else running with a positive active agent count
func deriveGatewayBusy(gatewayRunning bool, gatewayState string, activeAgents any, running bool) (bool, error) {
	if derive := GatewayDeriveBusy; derive != nil {
		if busy, err := derive(gatewayRunning, gatewayState, activeAgents); err == nil {
			return busy, nil
		}
	}
	if !running {
		return false, nil
	}
	n, err := parseActiveAgents(activeAgents)
	if err != nil {
		return false, err
	}
	count, _ := pyint.Of(n)
	return count != nil && count.Sign() > 0, nil
}

// Gateway drainable flag through the status contract, else whether it is running
func deriveGatewayDrainable(gatewayRunning bool, gatewayState string, running bool) bool {
	if derive := GatewayDeriveDrainable; derive != nil {
		return derive(gatewayRunning, gatewayState)
	}
	return running
}

// Converts gateway_state.json compatible runtime state into service health gauges and events
func BuildGatewayHealthSnapshot(runtime *pyjson.Object, gatewayRunning bool, profile, installID, version, supervisionMode string) (GatewayHealthSnapshot, error) {
	if runtime == nil {
		runtime = pyjson.NewObject()
	}
	gatewayState := boundedState(runtime.GetOr("gateway_state", nil), knownGatewayStates)
	activeAgents, err := parseActiveAgents(runtime.GetOr("active_agents", 0))
	if err != nil {
		return GatewayHealthSnapshot{}, err
	}
	running := gatewayRunning && gatewayState == "running"
	busy, err := deriveGatewayBusy(gatewayRunning, gatewayState, activeAgents, running)
	if err != nil {
		return GatewayHealthSnapshot{}, err
	}
	drainable := deriveGatewayDrainable(gatewayRunning, gatewayState, running)
	platforms := platformsOf(runtime)
	mode := strings.ToLower(pyrepr.Str(pyrepr.Or(supervisionMode, "unknown")))
	if !supervisionModes[mode] {
		mode = "unknown"
	}
	base := map[string]string{
		"service.instance.id":     safeInstanceID(installID),
		"service.version":         safeMetricValue(version, 64),
		"hermes.supervision_mode": mode,
	}
	metric := func(name string, value any, extra ...string) GatewayMetric {
		attrs := make(map[string]string, len(base)+len(extra)/2)
		for k, v := range base {
			attrs[k] = v
		}
		for i := 0; i+1 < len(extra); i += 2 {
			attrs[extra[i]] = safeMetricValue(extra[i+1], 128)
		}
		return GatewayMetric{Name: name, Value: value, Attributes: attrs}
	}
	flag := func(b bool) int {
		if b {
			return 1
		}
		return 0
	}
	metrics := []GatewayMetric{
		metric("hermes.gateway.up", flag(gatewayRunning)),
		metric("hermes.gateway.active_agents", activeAgents),
		metric("hermes.gateway.busy", flag(busy)),
		metric("hermes.gateway.drainable", flag(drainable)),
		metric("hermes.gateway.restart_requested", flag(pyrepr.Truthy(runtime.GetOr("restart_requested", nil)))),
		metric("hermes.gateway.state", 1, "hermes.gateway.state", gatewayState),
	}
	fatalCount := 0
	var events []Event
	for _, platform := range platforms.Keys {
		pdata := dictOrEmpty(platforms.Vals[platform])
		state := boundedState(pdata.GetOr("state", nil), knownPlatformStates)
		errorCode := ClassifyGatewayError(pyrepr.Or(pdata.GetOr("error_code", nil), pdata.GetOr("error_message", nil)))
		degraded := fatalPlatformStates[state]
		fatalCount += flag(degraded)
		metrics = append(metrics,
			metric("hermes.platform.up", flag(runningStateSet[state]), "hermes.platform", platform, "hermes.platform.state", state),
			metric("hermes.platform.degraded", flag(degraded), "hermes.platform", platform, "hermes.platform.state", state, "hermes.error_code", errorCode))
		if degraded {
			ev := NewGatewayDiagnosticEvent("platform.fatal", "platform."+platform)
			ev.Platform, ev.ErrorCode, ev.ErrorClass = new(platform), new(errorCode), errorCode
			ev.Profile, ev.Version = new(profile), new(version)
			if state == "fatal" {
				ev.Severity = "error"
			}
			events = append(events, ev)
		}
	}
	pid, err := coercePID(runtime.GetOr("pid", nil))
	if err != nil {
		return GatewayHealthSnapshot{}, err
	}
	health := NewGatewayHealthEvent("gateway.health_snapshot")
	health.GatewayState, health.ActiveAgents, health.GatewayBusy, health.GatewayDrainable = new(gatewayState), activeAgents, busy, drainable
	health.PlatformCount, health.FatalPlatformCount = len(platforms.Keys), fatalCount
	health.Profile, health.InstallID, health.Version, health.SupervisionMode, health.PID = new(profile), new(installID), new(version), new(supervisionMode), pid
	return GatewayHealthSnapshot{Metrics: metrics, Events: append([]Event{health}, events...)}, nil
}

// GatewayDiagnosticLogHandler bridges gateway warning and error records into allowlisted diagnostic events
type GatewayDiagnosticLogHandler struct {
	Profile string
	Version string
}

func (h *GatewayDiagnosticLogHandler) Level() int   { return logging.LevelWarning }
func (h *GatewayDiagnosticLogHandler) Close() error { return nil }

// Emits a diagnostic event for a gateway logger record at warning or above
func (h *GatewayDiagnosticLogHandler) Handle(_ context.Context, rec *logging.Record) bool {
	defer func() {
		if r := recover(); r != nil {
			healthLog().Debug(fmt.Sprintf("gateway diagnostic emit failed: %v", r))
		}
	}()
	if rec.Levelno < logging.LevelWarning || !(rec.Name == "gateway" || strings.HasPrefix(rec.Name, "gateway.")) {
		return true
	}
	subsystem := SubsystemForLogger(rec.Name)
	errorClass := ClassifyGatewayError(rec.Msg)
	severity := strings.ToLower(rec.Levelname)
	ev := NewGatewayDiagnosticEvent("gateway.log."+severity, subsystem)
	ev.SourceLogger, ev.Platform = SourceLoggerForExport(rec.Name), PlatformForSubsystem(subsystem)
	ev.ErrorClass, ev.ErrorCode, ev.Profile, ev.Version, ev.Severity = errorClass, new(errorClass), new(h.Profile), new(h.Version), severity
	GetEmitter().Emit(ev)
	return true
}
