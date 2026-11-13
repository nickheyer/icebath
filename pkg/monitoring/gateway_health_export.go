package monitoring

import (
	"context"
	"fmt"
	"log/slog"
	"math/big"
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

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/logging"
)

// Every gauge the runtime snapshot can emit, a metric missing here is never exported
var observableMetricNames = []string{
	"hermes.gateway.up", "hermes.gateway.state", "hermes.gateway.active_agents", "hermes.gateway.busy",
	"hermes.gateway.drainable", "hermes.gateway.restart_requested", "hermes.gateway.background_work",
	"hermes.gateway.background_delegations", "hermes.platform.up", "hermes.platform.degraded",
	"hermes.cron.scheduler.heartbeat_age_seconds", "hermes.cron.scheduler.last_success_age_seconds",
	"hermes.cron.scheduler.catch_up_occurrences", "hermes.cron.jobs.enabled", "hermes.cron.jobs.running",
	"hermes.cron.jobs.overdue", "hermes.cron.store.writable", "hermes.cron.store.skipped_runs",
}

// Gauges whose observations are seconds as floats, every other gauge observing integers
var floatGaugeNames = map[string]bool{
	"hermes.cron.scheduler.heartbeat_age_seconds":    true,
	"hermes.cron.scheduler.last_success_age_seconds": true,
}

var (
	// Runtime status payload of the active gateway, assigned by the gateway package
	GatewayReadRuntimeStatus func() *pyjson.Object

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

func healthExportLog() *slog.Logger {
	return slog.Default().With("logger", "agent.monitoring.gateway_health_export")
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

func gatewayHealthConfig(cfg map[string]any) map[string]any {
	return monitoringSection(cfg, "gateway_health_export")
}

// Config value of the export section, def when the key is absent
func gatewayHealthOption(cfg map[string]any, key string, def any) any {
	if v, ok := gatewayHealthConfig(cfg)[key]; ok {
		return v
	}
	return def
}

func gatewayHealthEnabled(cfg map[string]any) bool {
	return pyrepr.Truthy(gatewayHealthConfig(cfg)["enabled"]) && IsOTLPEnabled(cfg)
}

// Signal endpoint and resolved headers of the OTLP export, nil headers when none resolve
func exporterArgs(cfg map[string]any, signal string) (string, map[string]string) {
	otlp := otlpConfig(cfg)
	headers := ResolveHeaders(otlp["headers_env"])
	if len(headers) == 0 {
		headers = nil
	}
	return SignalEndpoint(pyrepr.Str(otlp["endpoint"]), signal), headers
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
func readGatewaySnapshot(cfg map[string]any) (GatewayHealthSnapshot, error) {
	runtime := pyjson.NewObject()
	if read := GatewayReadRuntimeStatus; read != nil {
		func() {
			defer func() { _ = recover() }()
			if got := read(); got != nil && len(got.Keys) > 0 {
				runtime = got
			}
		}()
	}
	return BuildGatewayHealthSnapshot(runtime, true, safeProfile(), otlpInstallID(cfg), safeVersion(), supervisionMode())
}

// Non negative count from a live counter, zero with a debug line when the counter is absent or panics
func liveCount(failureMsg string, read func() int) (n int) {
	if read == nil {
		healthExportLog().Debug(failureMsg)
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			healthExportLog().Debug(fmt.Sprintf("%s: %v", failureMsg, r))
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
func readRuntimeSnapshot(cfg map[string]any) (GatewayHealthSnapshot, error) {
	snap, err := readGatewaySnapshot(cfg)
	if err != nil {
		return snap, err
	}
	base := map[string]string{}
	if len(snap.Metrics) > 0 {
		for k, v := range snap.Metrics[0].Attributes {
			base[k] = v
		}
	}
	snap.Metrics = append(snap.Metrics,
		GatewayMetric{Name: "hermes.gateway.background_work", Value: readBackgroundWorkCount(), Attributes: base},
		GatewayMetric{Name: "hermes.gateway.background_delegations", Value: readBackgroundDelegationsCount(), Attributes: base})
	snap.Metrics = append(snap.Metrics, BuildCronHealthSnapshot(context.Background()).Metrics...)
	return snap, nil
}

// Emits the snapshot events unless diagnostic events are disabled
func emitSnapshotEvents(cfg map[string]any) {
	if !pyrepr.Truthy(gatewayHealthOption(cfg, "diagnostic_events_enabled", true)) {
		return
	}
	snap, err := readRuntimeSnapshot(cfg)
	if err != nil {
		healthExportLog().Debug("gateway health snapshot emit failed: " + err.Error())
		return
	}
	for _, ev := range snap.Events {
		Emit(ev)
	}
}

// Python int of a config value, the conversion error for a value int rejects
func configInt(v any) (int64, error) {
	n, err := pyint.Of(v)
	if err != nil {
		return 0, err
	}
	if !n.IsInt64() {
		return 0, fmt.Errorf("OverflowError: int too large")
	}
	return n.Int64(), nil
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
	if n, ok := pyint.Instance(v); ok {
		f, _ := new(big.Float).SetInt(n).Float64()
		return f, true
	}
	return 0, false
}

// Numeric value of a gauge observation as an integer, a float truncated
func metricInt(v any) (int64, bool) {
	if n, ok := pyint.Instance(v); ok && n.IsInt64() {
		return n.Int64(), true
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
func runtimeResource(cfg map[string]any, scope string) (*resource.Resource, error) {
	var kvs []attribute.KeyValue
	for k, v := range RuntimeResourceAttributes(cfg, scope) {
		kvs = append(kvs, attribute.String(k, v))
	}
	return resource.Merge(resource.Default(), resource.NewSchemaless(kvs...))
}

// Meter provider exporting every observable gauge periodically to the metrics endpoint
func startMetricProvider(cfg map[string]any, sdk *gatewayHealthSDK) (*sdkmetric.MeterProvider, error) {
	endpoint, headers := exporterArgs(cfg, "metrics")
	exporter, err := sdk.newMetricExporter(context.Background(), endpoint, headers)
	if err != nil {
		return nil, err
	}
	seconds, err := configInt(gatewayHealthOption(cfg, "export_interval_seconds", 60))
	if err != nil {
		return nil, err
	}
	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Duration(max(5, seconds))*time.Second))
	res, err := runtimeResource(cfg, "gateway_health")
	if err != nil {
		return nil, err
	}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithResource(res))
	meter := provider.Meter("hermes.gateway.health")
	matching := func(name string) []GatewayMetric {
		snap, err := readRuntimeSnapshot(cfg)
		if err != nil {
			healthExportLog().Debug("gateway metric callback failed: " + err.Error())
			return nil
		}
		var out []GatewayMetric
		for _, m := range snap.Metrics {
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
	if pyrepr.Truthy(severity) {
		text = pyrepr.Str(severity)
	}
	if n, ok := severityNames[strings.ToLower(text)]; ok {
		return n
	}
	return logspb.SeverityNumber_SEVERITY_NUMBER_WARN
}

// Allowlisted redacted diagnostic attributes, never the profile, install id or timestamp
func diagnosticLogAttributes(ev *pyjson.Object) map[string]any {
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
func newGatewayDiagnosticLogStreamer(cfg map[string]any, sdk *gatewayHealthSDK) (*GatewayDiagnosticLogStreamer, error) {
	res, err := runtimeResource(cfg, "gateway_diagnostics")
	if err != nil {
		return nil, err
	}
	endpoint, headers := exporterArgs(cfg, "logs")
	s := &GatewayDiagnosticLogStreamer{processor: newBatchLogProcessor(sdk.newLogExporter(endpoint, headers, res))}
	s.sub = NewSubscriber(s.Call)
	return s, nil
}

// Emits one log record per gateway diagnostic event of the batch, scoped by its source logger
func (s *GatewayDiagnosticLogStreamer) Call(batch []*pyjson.Object) {
	for _, ev := range batch {
		if kind, _ := ev.Get("event"); kind != any("gateway_diagnostic") {
			continue
		}
		scope := "hermes.gateway.diagnostics"
		if src := SourceLoggerForExport(ev.GetOr("source_logger", nil)); src != nil {
			scope = *src
		}
		severity := ev.GetOr("severity", nil)
		text := "warning"
		if pyrepr.Truthy(severity) {
			text = pyrepr.Str(severity)
		}
		rec := &logspb.LogRecord{
			ObservedTimeUnixNano: uint64(time.Now().UnixNano()),
			SeverityNumber:       severityNumber(severity),
			SeverityText:         strings.ToUpper(text),
			Body:                 &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: RedactBounded("gateway diagnostic")}},
		}
		if ts, ok := pyint.Instance(ev.GetOr("ts_ns", nil)); ok && ts.IsUint64() {
			rec.TimeUnixNano = ts.Uint64()
		}
		attrs := diagnosticLogAttributes(ev)
		for _, col := range keepByKind["gateway_diagnostic"] {
			v, has := attrs["icebath."+col]
			if !has {
				continue
			}
			value, err := anyValueProto(v)
			if err != nil {
				healthExportLog().Error("Failed to encode key icebath." + col + ": " + err.Error())
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

func gatewayHealthEvent(ev *pyjson.Object) bool {
	kind, _ := ev.Get("event")
	return kind == any("gateway_health") || kind == any("cron_execution")
}

// Logs the failure, shuts the partial runtime down and returns a disabled runtime with the reason
func failGatewayHealthExport(runtime *GatewayHealthExportRuntime, level slog.Level, msg string, err error, reason string) *GatewayHealthExportRuntime {
	healthExportLog().Log(context.Background(), level, msg+": "+err.Error())
	runtime.Shutdown()
	return &GatewayHealthExportRuntime{Enabled: false, Reason: reason}
}

// Starts the gateway health export the config enables, a disabled runtime with its reason otherwise
func StartGatewayHealthExport(cfg map[string]any) *GatewayHealthExportRuntime {
	if !gatewayHealthEnabled(cfg) {
		return &GatewayHealthExportRuntime{Enabled: false, Reason: "disabled"}
	}
	metricsOn := pyrepr.Truthy(gatewayHealthOption(cfg, "metrics_enabled", true))
	diagnosticsOn := pyrepr.Truthy(gatewayHealthOption(cfg, "diagnostic_events_enabled", true))
	runtime := &GatewayHealthExportRuntime{Enabled: true, Reason: "enabled"}
	var sdk *gatewayHealthSDK
	if metricsOn || diagnosticsOn {
		sdk = builtinGatewayHealthSDK
	}
	if metricsOn && sdk != nil {
		provider, err := startMetricProvider(cfg, sdk)
		if err != nil {
			return failGatewayHealthExport(runtime, slog.LevelWarn, "gateway health OTLP metrics failed to start", err, "metrics_start_failed")
		}
		runtime.MetricProvider = provider
	}
	if diagnosticsOn && sdk != nil {
		runtime.Streamer = StartStreaming(cfg, gatewayHealthEvent)
		if runtime.Streamer == nil {
			return failGatewayHealthExport(runtime, slog.LevelDebug, "gateway diagnostic OTLP export failed to start", fmt.Errorf("gateway health span streamer did not start"), "diagnostics_start_failed")
		}
		logStreamer, err := newGatewayDiagnosticLogStreamer(cfg, sdk)
		if err != nil {
			return failGatewayHealthExport(runtime, slog.LevelDebug, "gateway diagnostic OTLP export failed to start", err, "diagnostics_start_failed")
		}
		GetEmitter().Subscribe(logStreamer.sub)
		runtime.LogStreamer = logStreamer
	}
	if diagnosticsOn && pyrepr.Truthy(gatewayHealthOption(cfg, "warning_error_events_enabled", true)) {
		handler := NewGatewayDiagnosticLogHandler(safeProfile(), safeVersion())
		logging.GetLogger("").AddHandler(handler)
		runtime.LogHandler = handler
	}
	if diagnosticsOn {
		emitSnapshotEvents(cfg)
		seconds, err := configInt(gatewayHealthOption(cfg, "logs_export_interval_seconds", 5))
		if err != nil {
			healthExportLog().Debug("gateway health snapshot thread failed to start: " + err.Error())
			return runtime
		}
		interval := time.Duration(max(5, seconds)) * time.Second
		stop, done := make(chan struct{}), make(chan struct{})
		runtime.stop, runtime.done = stop, done
		go func() {
			defer close(done)
			for {
				select {
				case <-stop:
					return
				case <-time.After(interval):
					emitSnapshotEvents(cfg)
				}
			}
		}()
	}
	return runtime
}
