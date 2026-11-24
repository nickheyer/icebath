package monitoring

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
)

// Word of a switch in the status lines
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// Prints the gateway monitoring status
func CmdMonitoring(ctx context.Context, h string, ns map[string]any, w, errw io.Writer) (int, error) {
	action := "status"
	if v := anyutil.String(ns["monitoring_action"]); anyutil.IsTrue(ns["monitoring_action"]) {
		action = v
	}
	sctx := home.WithOverride(ctx, h)
	if action != "status" {
		fmt.Fprintf(errw, "Unknown monitoring action: %s\n", action)
		return 2, nil
	}
	fmt.Fprintln(w, "Gateway monitoring")
	health := "disabled"
	if settings.MonitoringGatewayHealthExportEnabled.Get(sctx) {
		health = "enabled"
	}
	fmt.Fprintf(w, "  Health export:  %s (monitoring.gateway_health_export.enabled)\n", health)
	if settings.MonitoringGatewayHealthExportEnabled.Get(sctx) {
		fmt.Fprintf(w, "    Metrics:            %s (interval %gs)\n", onOff(settings.MonitoringGatewayHealthExportMetricsEnabled.Get(sctx)), settings.MonitoringGatewayHealthExportExportIntervalSeconds.Get(sctx).Seconds())
		fmt.Fprintf(w, "    Diagnostic events:  %s\n", onOff(settings.MonitoringGatewayHealthExportDiagnosticEventsEnabled.Get(sctx)))
		fmt.Fprintf(w, "    Warning/error logs: %s (interval %gs)\n", onOff(settings.MonitoringGatewayHealthExportWarningErrorEventsEnabled.Get(sctx)), settings.MonitoringGatewayHealthExportLogsExportIntervalSeconds.Get(sctx).Seconds())
		fmt.Fprintln(w, "    Content safety:     always on (rendered messages are never exported; not configurable)")
	}
	if endpoint := settings.MonitoringExportOtlpEndpoint.Get(sctx); settings.MonitoringExportOtlpEnabled.Get(sctx) && endpoint != "" {
		fmt.Fprintf(w, "  OTLP endpoint:  %s\n", endpoint)
	} else {
		fmt.Fprintln(w, "  OTLP endpoint:  not configured (monitoring.export.otlp)")
	}
	fmt.Fprintf(w, "  OTel SDK:       %s\n", strings.TrimSpace(OTelSDKModule+" "+hostinfo.ModuleVersion(OTelSDKModule)))
	fmt.Fprintln(w, "\n  Scope: gateway service health + redacted diagnostics only.")
	fmt.Fprintln(w, "  No prompts, messages, tool args/results, usage analytics, or traces.")
	return 0, nil
}
