package monitoring

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Every gauge the runtime snapshot can emit, a metric missing here is never exported
var observableMetricNames = []string{
	"icebath.gateway.up", "icebath.gateway.state", "icebath.gateway.active_agents", "icebath.gateway.busy",
	"icebath.gateway.drainable", "icebath.gateway.restart_requested", "icebath.gateway.background_work",
	"icebath.gateway.background_delegations", "icebath.platform.up", "icebath.platform.degraded",
	"icebath.cron.scheduler.heartbeat_age_seconds", "icebath.cron.scheduler.last_success_age_seconds",
	"icebath.cron.scheduler.catch_up_occurrences", "icebath.cron.jobs.enabled", "icebath.cron.jobs.running",
	"icebath.cron.jobs.overdue", "icebath.cron.store.writable", "icebath.cron.store.skipped_runs",
}

// Gauges whose observations are seconds as floats, every other gauge observing integers
var floatGaugeNames = map[string]bool{
	"icebath.cron.scheduler.heartbeat_age_seconds":    true,
	"icebath.cron.scheduler.last_success_age_seconds": true,
}

var (
	// Runtime status payload of the active gateway, assigned by the gateway package
	GatewayReadRuntimeStatus func() *jsonutil.Object

	// Live background work counters, assigned by the tools package
	AsyncDelegationActiveTaskCount func() int
	AsyncDelegationActiveCount     func() int
	ProcessRegistryCountRunning    func() int
)

// OTLP SDK constructors the export builds its metric and log pipelines with
type gatewayHealthSDK struct {
	newMetricExporter func(ctx context.Context, endpoint string, headers map[string]string) (sdkmetric.Exporter, error)
	newLogExporter    func(endpoint string, headers map[string]string, res *resource.Resource) *otlpLogExporter
}

var builtinGatewayHealthSDK = &gatewayHealthSDK{
	newMetricExporter: func(ctx context.Context, endpoint string, headers map[string]string) (sdkmetric.Exporter, error) {
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(endpoint)}
		if len(headers) > 0 {
			opts = append(opts, otlpmetrichttp.WithHeaders(headers))
		}
		return otlpmetrichttp.New(ctx, opts...)
	},
	newLogExporter: newOTLPLogExporter,
}

// GatewayHealthExportRuntime owns the metric provider, streamers, log handler and snapshot loop of one export
type GatewayHealthExportRuntime struct {
	Enabled        bool
	Reason         string
	Streamer       *OTLPStreamer
	MetricProvider *sdkmetric.MeterProvider
	LogHandler     *GatewayDiagnosticLogHandler
	LogStreamer    *GatewayDiagnosticLogStreamer
	stop           chan struct{}
	done           chan struct{}
}

// Stops the snapshot loop, detaches the handler, drains the emitter and closes exporters within two seconds
func (r *GatewayHealthExportRuntime) Shutdown() {
	if r.stop != nil {
		close(r.stop)
	}
	if r.done != nil {
		select {
		case <-r.done:
		case <-time.After(250 * time.Millisecond):
		}
	}
	if r.LogHandler != nil {
		logging.GetLogger("").RemoveHandler(r.LogHandler)
	}
	bus := GetEmitter()
	bus.Flush(1.0)
	if r.Streamer != nil {
		bus.Unsubscribe(r.Streamer.Subscriber())
	}
	if r.LogStreamer != nil {
		bus.Unsubscribe(r.LogStreamer.sub)
	}
	var closeables []func()
	if r.Streamer != nil {
		closeables = append(closeables, r.Streamer.Shutdown)
	}
	if r.LogStreamer != nil {
		closeables = append(closeables, r.LogStreamer.Shutdown)
	}
	if r.MetricProvider != nil {
		provider := r.MetricProvider
		closeables = append(closeables, func() { _ = provider.Shutdown(context.Background()) })
	}
	if len(closeables) > 0 {
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			for _, c := range closeables {
				func() {
					defer func() { _ = recover() }()
					c()
				}()
			}
		}()
		select {
		case <-finished:
		case <-time.After(2 * time.Second):
		}
	}
	r.Streamer, r.LogStreamer, r.MetricProvider, r.stop, r.done = nil, nil, nil, nil, nil
}

func gatewayHealthEnabled(ctx context.Context) bool {
	return settings.MonitoringGatewayHealthExportEnabled.Get(ctx) && IsOTLPEnabled(ctx)
}

// Signal endpoint and resolved headers of the OTLP export, nil headers when none resolve
func exporterArgs(ctx context.Context, signal string) (string, map[string]string) {
	headers := ResolveHeaders(settings.MonitoringExportOtlpHeadersEnv.Get(ctx))
	if len(headers) == 0 {
		headers = nil
	}
	return SignalEndpoint(settings.MonitoringExportOtlpEndpoint.Get(ctx), signal), headers
}

// Supervisor running this process, systemd then s6 then container then launchd, manual otherwise
func supervisionMode() string {
	if os.Getenv("INVOCATION_ID") != "" {
		return "systemd"
	}
	if os.Getenv("S6_CMD_ARG0") != "" || os.Getenv("S6_VERSION") != "" {
		return "s6"
	}
	if os.Getenv("container") != "" {
		return "container"
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "container"
	}
	if os.Getenv("LAUNCHD_SOCKET") != "" {
		return "launchd"
	}
	return "manual"
}

// Gateway snapshot of the runtime status file, an empty status when it cannot be read
func readGatewaySnapshot(ctx context.Context) GatewayHealthSnapshot {
	runtime := jsonutil.NewObject()
	if read := GatewayReadRuntimeStatus; read != nil {
		func() {
			defer func() { _ = recover() }()
			if got := read(); got != nil && got.Len() > 0 {
				runtime = got
			}
		}()
	}
	return BuildGatewayHealthSnapshot(runtime, true, safeProfile(), otlpInstallID(ctx), safeVersion(), supervisionMode())
}

// Non negative count from a live counter, zero with a debug line when the counter is absent or panics
func liveCount(failureMsg string, read func() int) (n int) {
	if read == nil {
		pkgLog().Debug(failureMsg)
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			pkgLog().Debug(fmt.Sprintf("%s: %v", failureMsg, r))
			n = 0
		}
	}()
	return max(0, read())
}

func readBackgroundWorkCount() int {
	return liveCount("background-work async-delegation count failed", AsyncDelegationActiveTaskCount) +
		liveCount("background-work process-registry count failed", ProcessRegistryCountRunning)
}

func readBackgroundDelegationsCount() int {
	return liveCount("background-delegations count failed", AsyncDelegationActiveCount)
}

// Gateway snapshot with background work gauges on the gateway attributes and the cron gauges appended
func readRuntimeSnapshot(ctx context.Context) GatewayHealthSnapshot {
	snap := readGatewaySnapshot(ctx)
	base := map[string]string{}
	if len(snap.Metrics) > 0 {
		for k, v := range snap.Metrics[0].Attributes {
			base[k] = v
		}
	}
	snap.Metrics = append(snap.Metrics,
		GatewayMetric{Name: "icebath.gateway.background_work", Value: readBackgroundWorkCount(), Attributes: base},
		GatewayMetric{Name: "icebath.gateway.background_delegations", Value: readBackgroundDelegationsCount(), Attributes: base})
	snap.Metrics = append(snap.Metrics, BuildCronHealthSnapshot(ctx).Metrics...)
	return snap
}

// Emits the snapshot events unless diagnostic events are disabled
func emitSnapshotEvents(ctx context.Context) {
	if !settings.MonitoringGatewayHealthExportDiagnosticEventsEnabled.Get(ctx) {
		return
	}
	for _, ev := range readRuntimeSnapshot(ctx).Events {
		Emit(ev)
	}
}

// Numeric value of a gauge observation as a float
func metricFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case *float64:
		if x != nil {
			return *x, true
		}
		return 0, false
	}
	if n, ok := anyutil.Int(v); ok {
		return float64(n), true
	}
	return 0, false
}

// Numeric value of a gauge observation as an integer, a float truncated
func metricInt(v any) (int64, bool) {
	if n, ok := anyutil.Int(v); ok {
		return n, true
	}
	if f, ok := metricFloat(v); ok {
		return int64(f), true
	}
	return 0, false
}

// Attribute options of a gauge observation
func metricAttributes(attrs map[string]string) otelmetric.MeasurementOption {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		kvs = append(kvs, attribute.String(k, v))
	}
	return otelmetric.WithAttributes(kvs...)
}

// SDK resource of the merged default and runtime resource attributes for a telemetry scope
func runtimeResource(ctx context.Context, scope string) (*resource.Resource, error) {
	var kvs []attribute.KeyValue
	for k, v := range RuntimeResourceAttributes(ctx, scope) {
		kvs = append(kvs, attribute.String(k, v))
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(kvs...))
}

// Meter provider exporting every observable gauge periodically to the metrics endpoint
func startMetricProvider(ctx context.Context, sdk *gatewayHealthSDK) (*sdkmetric.MeterProvider, error) {
	endpoint, headers := exporterArgs(ctx, "metrics")
	exporter, err := sdk.newMetricExporter(ctx, endpoint, headers)
	if err != nil {
		return nil, err
	}
	interval := max(5*time.Second, settings.MonitoringGatewayHealthExportExportIntervalSeconds.Get(ctx))
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(interval))
	res, err := runtimeResource(ctx, "gateway_health")
	if err != nil {
		return nil, err
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	meter := provider.Meter("icebath.gateway.health")
	matching := func(name string) []GatewayMetric {
		var out []GatewayMetric
		for _, m := range readRuntimeSnapshot(ctx).Metrics {
			if m.Name == name {
				out = append(out, m)
			}
		}
		return out
	}
	for _, name := range observableMetricNames {
		if floatGaugeNames[name] {
			_, err = meter.Float64ObservableGauge(name, otelmetric.WithFloat64Callback(func(_ context.Context, o otelmetric.Float64Observer) error {
				for _, m := range matching(name) {
					if v, ok := metricFloat(m.Value); ok {
						o.Observe(v, metricAttributes(m.Attributes))
					}
				}
				return nil
			}))
		} else {
			_, err = meter.Int64ObservableGauge(name, otelmetric.WithInt64Callback(func(_ context.Context, o otelmetric.Int64Observer) error {
				for _, m := range matching(name) {
					if v, ok := metricInt(m.Value); ok {
						o.Observe(v, metricAttributes(m.Attributes))
					}
				}
				return nil
			}))
		}
		if err != nil {
			_ = provider.Shutdown(context.Background())
			return nil, err
		}
	}
	return provider, nil
}

var severityNames = map[string]logspb.SeverityNumber{
	"critical": logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, "fatal": logspb.SeverityNumber_SEVERITY_NUMBER_FATAL,
	"error": logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, "info": logspb.SeverityNumber_SEVERITY_NUMBER_INFO,
	"information": logspb.SeverityNumber_SEVERITY_NUMBER_INFO, "debug": logspb.SeverityNumber_SEVERITY_NUMBER_DEBUG,
}

// OTel severity of a diagnostic severity, warn for anything unrecognised
func severityNumber(severity any) logspb.SeverityNumber {
	text := "warning"
	if anyutil.IsTrue(severity) {
		text = anyutil.String(severity)
	}
	if n, ok := severityNames[strings.ToLower(text)]; ok {
		return n
	}
	return logspb.SeverityNumber_SEVERITY_NUMBER_WARN
}

// Allowlisted redacted diagnostic attributes, never the profile, install id or timestamp
func diagnosticLogAttributes(ev *jsonutil.Object) map[string]any {
	return allowlistedAttrs(ev, keepByKind["gateway_diagnostic"])
}

// GatewayDiagnosticLogStreamer is the emitter subscriber sending gateway diagnostic events as OTLP logs
type GatewayDiagnosticLogStreamer struct {
	processor *batchLogProcessor
	sub       *Subscriber
	mu        sync.Mutex
	exported  int
}

// Streamer batching logs to the logs endpoint under the gateway diagnostics resource
func newGatewayDiagnosticLogStreamer(ctx context.Context, sdk *gatewayHealthSDK) (*GatewayDiagnosticLogStreamer, error) {
	res, err := runtimeResource(ctx, "gateway_diagnostics")
	if err != nil {
		return nil, err
	}
	endpoint, headers := exporterArgs(ctx, "logs")
	s := &GatewayDiagnosticLogStreamer{processor: newBatchLogProcessor(sdk.newLogExporter(endpoint, headers, res))}
	s.sub = NewSubscriber(s.Call)
	return s, nil
}

// Emits one log record per gateway diagnostic event of the batch, scoped by its source logger
func (s *GatewayDiagnosticLogStreamer) Call(batch []*jsonutil.Object) {
	for _, ev := range batch {
		if kind, _ := ev.Get("event"); kind != any("gateway_diagnostic") {
			continue
		}
		scope := "icebath.gateway.diagnostics"
		if src := SourceLoggerForExport(ev.ValueOr("source_logger", nil)); src != nil {
			scope = *src
		}
		severity := ev.ValueOr("severity", nil)
		text := "warning"
		if anyutil.IsTrue(severity) {
			text = anyutil.String(severity)
		}
		rec := &logspb.LogRecord{
			ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
			SeverityNumber:       severityNumber(severity),
			SeverityText:         strings.ToUpper(text),
			Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: RedactBounded("gateway diagnostic")}},
		}
		if ts, ok := anyutil.Int(ev.ValueOr("ts_ns", nil)); ok && ts >= 0 {
			rec.TimeUnixNano = uint64(ts)
		}
		attrs := diagnosticLogAttributes(ev)
		for _, col := range keepByKind["gateway_diagnostic"] {
			v, has := attrs["icebath."+col]
			if !has {
				continue
			}
			value, err := anyValueProto(v)
			if err != nil {
				pkgLog().Error("Failed to encode key icebath." + col + ": " + err.Error())
				continue
			}
			rec.Attributes = append(rec.Attributes, &commonpb.KeyValue{Key: "icebath." + col, Value: value})
		}
		s.processor.emit(otlpLogRecord{scope: scope, record: rec})
		s.mu.Lock()
		s.exported++
		s.mu.Unlock()
	}
}

// Count of log records the streamer emitted
func (s *GatewayDiagnosticLogStreamer) Exported() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exported
}

// Unsubscribes from the emitter, flushes queued records and shuts the processor down
func (s *GatewayDiagnosticLogStreamer) Shutdown() {
	GetEmitter().Unsubscribe(s.sub)
	s.processor.forceFlush()
	s.processor.close()
}

func gatewayHealthEvent(ev *jsonutil.Object) bool {
	kind, _ := ev.Get("event")
	return kind == any("gateway_health") || kind == any("cron_execution")
}

// Logs the failure, shuts the partial runtime down and returns a disabled runtime with the reason
func failGatewayHealthExport(runtime *GatewayHealthExportRuntime, level slog.Level, msg string, err error, reason string) *GatewayHealthExportRuntime {
	pkgLog().Log(context.Background(), level, msg+": "+err.Error())
	runtime.Shutdown()
	return &GatewayHealthExportRuntime{Enabled: false, Reason: reason}
}

// Starts the gateway health export the profile enables, a disabled runtime with its reason otherwise
func StartGatewayHealthExport(ctx context.Context) *GatewayHealthExportRuntime {
	if !gatewayHealthEnabled(ctx) {
		return &GatewayHealthExportRuntime{Enabled: false, Reason: "disabled"}
	}
	metricsOn := settings.MonitoringGatewayHealthExportMetricsEnabled.Get(ctx)
	diagnosticsOn := settings.MonitoringGatewayHealthExportDiagnosticEventsEnabled.Get(ctx)
	runtime := &GatewayHealthExportRuntime{Enabled: true, Reason: "enabled"}
	var sdk *gatewayHealthSDK
	if metricsOn || diagnosticsOn {
		sdk = builtinGatewayHealthSDK
	}
	if metricsOn && sdk != nil {
		provider, err := startMetricProvider(ctx, sdk)
		if err != nil {
			return failGatewayHealthExport(runtime, slog.LevelWarn, "gateway health OTLP metrics failed to start", err, "metrics_start_failed")
		}
		runtime.MetricProvider = provider
	}
	if diagnosticsOn && sdk != nil {
		runtime.Streamer = StartStreaming(ctx, gatewayHealthEvent)
		if runtime.Streamer == nil {
			return failGatewayHealthExport(runtime, slog.LevelDebug, "gateway diagnostic OTLP export failed to start", fmt.Errorf("gateway health span streamer did not start"), "diagnostics_start_failed")
		}
		logStreamer, err := newGatewayDiagnosticLogStreamer(ctx, sdk)
		if err != nil {
			return failGatewayHealthExport(runtime, slog.LevelDebug, "gateway diagnostic OTLP export failed to start", err, "diagnostics_start_failed")
		}
		GetEmitter().Subscribe(logStreamer.sub)
		runtime.LogStreamer = logStreamer
	}
	if diagnosticsOn && settings.MonitoringGatewayHealthExportWarningErrorEventsEnabled.Get(ctx) {
		handler := NewGatewayDiagnosticLogHandler(safeProfile(), safeVersion())
		logging.GetLogger("").AddHandler(handler)
		runtime.LogHandler = handler
	}
	if diagnosticsOn {
		emitSnapshotEvents(ctx)
		interval := max(5*time.Second, settings.MonitoringGatewayHealthExportLogsExportIntervalSeconds.Get(ctx))
		stop, done := make(chan struct{}), make(chan struct{})
		runtime.stop, runtime.done = stop, done
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				case <-time.After(interval):
					emitSnapshotEvents(ctx)
				}
			}
		}()
	}
	return runtime
}
