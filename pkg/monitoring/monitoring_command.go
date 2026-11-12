package monitoring

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// Mapping value of a config slot, empty for anything else
func configDictOrEmpty(v any) map[string]any {
	if m, ok := asConfigMap(v); ok {
		return m
	}
	return map[string]any{}
}

// Config value or the default when the key is absent, the dict.get of the status lines
func slotOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

func onOff(v any) string {
	if pyrepr.Truthy(v) {
		return "on"
	}
	return "off"
}

// CmdMonitoring prints the gateway monitoring status, hermes_cli/main_agent_cmds.py cmd_monitoring
func CmdMonitoring(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (int, error) {
	action := "status"
	if v := pyrepr.Str(ns["monitoring_action"]); pyrepr.Truthy(ns["monitoring_action"]) {
		action = v
	}
	tree, err := config.Load(h, config.NewSecrets(h).Lookup())
	if err != nil {
		return 1, err
	}
	mon := configDictOrEmpty(tree["monitoring"])
	if action != "status" {
		fmt.Fprintf(errw, "Unknown monitoring action: %s\n", action)
		return 2, nil
	}
	gh := configDictOrEmpty(mon["gateway_health_export"])
	otlp := configDictOrEmpty(configDictOrEmpty(mon["export"])["otlp"])
	fmt.Fprintln(w, "Gateway monitoring")
	health := "disabled"
	if pyrepr.Truthy(gh["enabled"]) {
		health = "enabled"
	}
	fmt.Fprintf(w, "  Health export:  %s (monitoring.gateway_health_export.enabled)\n", health)
	if pyrepr.Truthy(gh["enabled"]) {
		fmt.Fprintf(w, "    Metrics:            %s (interval %ss)\n", onOff(slotOr(gh, "metrics_enabled", true)), pyrepr.Str(slotOr(gh, "export_interval_seconds", int64(60))))
		fmt.Fprintf(w, "    Diagnostic events:  %s\n", onOff(slotOr(gh, "diagnostic_events_enabled", true)))
		fmt.Fprintf(w, "    Warning/error logs: %s (interval %ss)\n", onOff(slotOr(gh, "warning_error_events_enabled", true)), pyrepr.Str(slotOr(gh, "logs_export_interval_seconds", int64(5))))
		fmt.Fprintln(w, "    Content safety:     always on (rendered messages are never exported; not configurable)")
	}
	endpoint := ""
	if pyrepr.Truthy(otlp["endpoint"]) {
		endpoint = pyrepr.Str(otlp["endpoint"])
	}
	if pyrepr.Truthy(otlp["enabled"]) && endpoint != "" {
		fmt.Fprintf(w, "  OTLP endpoint:  %s\n", endpoint)
	} else {
		fmt.Fprintln(w, "  OTLP endpoint:  not configured (monitoring.export.otlp)")
	}
	fmt.Fprintf(w, "  OTel SDK:       %s\n", strings.TrimSpace(OTelSDKModule+" "+hostinfo.ModuleVersion(OTelSDKModule)))
	fmt.Fprintln(w, "\n  Scope: gateway service health + redacted diagnostics only.")
	fmt.Fprintln(w, "  No prompts, messages, tool args/results, usage analytics, or traces.")
	return 0, nil
}
