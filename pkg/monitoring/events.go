package monitoring

import (
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Event is a typed monitoring event whose dict form leads with its event name in wire order
type Event interface {
	ToDict() *pyjson.Object
}

// GatewayHealthEvent is a content free gateway health snapshot or lifecycle event
type GatewayHealthEvent struct {
	Name               string
	GatewayState       *string
	OldState           *string
	NewState           *string
	ExitReason         *string
	RestartRequested   *bool
	ActiveAgents       any
	GatewayBusy        bool
	GatewayDrainable   bool
	PlatformCount      int
	FatalPlatformCount int
	Profile            *string
	InstallID          *string
	Version            *string
	SupervisionMode    *string
	PID                any
	TsNS               int64
}

// GatewayDiagnosticEvent is a redacted gateway diagnostic event for operator owned observability
type GatewayDiagnosticEvent struct {
	Name         string
	Subsystem    string
	ErrorClass   string
	ErrorCode    *string
	Platform     *string
	OldState     *string
	NewState     *string
	Profile      *string
	Version      *string
	Severity     string
	TsNS         int64
	SourceLogger *string
}

// CronExecutionEvent is a content free durable cron execution lifecycle projection
type CronExecutionEvent struct {
	Status          string
	JobKey          string
	Source          string
	DurationMS      *int64
	DeliveryOutcome *string
	ErrorClass      *string
	TsNS            int64
}

// Health event with the dataclass defaults and the current nanosecond timestamp
func NewGatewayHealthEvent(name string) GatewayHealthEvent {
	return GatewayHealthEvent{Name: name, ActiveAgents: 0, TsNS: time.Now().UnixNano()}
}

// Diagnostic event with unknown class, warning severity and the current nanosecond timestamp
func NewGatewayDiagnosticEvent(name, subsystem string) GatewayDiagnosticEvent {
	return GatewayDiagnosticEvent{Name: name, Subsystem: subsystem, ErrorClass: "unknown", Severity: "warning", TsNS: time.Now().UnixNano()}
}

// Cron event with unknown source and the current nanosecond timestamp
func NewCronExecutionEvent(status, jobKey string) CronExecutionEvent {
	return CronExecutionEvent{Status: status, JobKey: jobKey, Source: "unknown", TsNS: time.Now().UnixNano()}
}

// Value of an optional field, None when absent
func opt[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// Ordered pairs into an object led by the event name
func eventDict(event string, kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("event", event)
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

func (e GatewayHealthEvent) ToDict() *pyjson.Object {
	return eventDict("gateway_health", "name", e.Name, "gateway_state", opt(e.GatewayState), "old_state", opt(e.OldState),
		"new_state", opt(e.NewState), "exit_reason", opt(e.ExitReason), "restart_requested", opt(e.RestartRequested),
		"active_agents", e.ActiveAgents, "gateway_busy", e.GatewayBusy, "gateway_drainable", e.GatewayDrainable,
		"platform_count", e.PlatformCount, "fatal_platform_count", e.FatalPlatformCount, "profile", opt(e.Profile),
		"install_id", opt(e.InstallID), "version", opt(e.Version), "supervision_mode", opt(e.SupervisionMode),
		"pid", e.PID, "ts_ns", e.TsNS)
}

func (e GatewayDiagnosticEvent) ToDict() *pyjson.Object {
	return eventDict("gateway_diagnostic", "name", e.Name, "subsystem", e.Subsystem, "error_class", e.ErrorClass,
		"error_code", opt(e.ErrorCode), "platform", opt(e.Platform), "old_state", opt(e.OldState), "new_state",
		opt(e.NewState), "profile", opt(e.Profile), "version", opt(e.Version), "severity", e.Severity, "ts_ns", e.TsNS,
		"source_logger", opt(e.SourceLogger))
}

func (e CronExecutionEvent) ToDict() *pyjson.Object {
	return eventDict("cron_execution", "status", e.Status, "job_key", e.JobKey, "source", e.Source, "duration_ms",
		opt(e.DurationMS), "delivery_outcome", opt(e.DeliveryOutcome), "error_class", opt(e.ErrorClass), "ts_ns", e.TsNS)
}
