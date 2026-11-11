package teamspipeline

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/teams"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Teams platform settings mirrored into the teams_delivery config of the pipeline
var deliveryKeys = []string{"incoming_webhook_url", "access_token", "team_id", "channel_id", "chat_id"}

// Pipeline surface the webhook scheduler drives, the durable accept and the detached run
type PipelineRuntime interface {
	CreateJobFromNotification(notification *pyjson.Object) (*TeamsMeetingPipelineJob, error)
	RunJob(ctx context.Context, jobOrID any) (*TeamsMeetingPipelineJob, error)
}

// Gateway runner surface the binding reads and writes, its config, live adapters and bound runtime slot
type GatewayRuntimeHost interface {
	RunnerConfig() *gateway.GatewayConfig
	Adapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]
	TeamsPipelineRuntime() any
	SetTeamsPipelineRuntime(runtime any)
	SetTeamsPipelineRuntimeError(message *string)
}

// Graph webhook adapter surface that takes the notification scheduler
type notificationSchedulerAdapter interface {
	SetNotificationScheduler(scheduler gateway.NotificationScheduler)
}

// Adapter that tracks detached tasks for cancellation at shutdown
type backgroundTaskTracker interface {
	TrackBackgroundTask(task *gateway.AdapterTask)
}

var (
	_ notificationSchedulerAdapter = (*gateway.MSGraphWebhookAdapter)(nil)
	_ backgroundTaskTracker        = (*gateway.MSGraphWebhookAdapter)(nil)
)

// Graph client, runtime builder and Teams summary writer import the binding uses, swapped by tests
var (
	runtimeGraphClient   = buildGraphClient
	runtimeBuildPipeline = func(ctx context.Context, host GatewayRuntimeHost) (PipelineRuntime, error) {
		pipeline, err := BuildPipelineRuntime(ctx, host)
		if err != nil {
			return nil, err
		}
		return pipeline, nil
	}
	importTeamsSummaryWriter = func() (func(platformConfig *gateway.PlatformConfig) SummarySink, error) {
		return func(platformConfig *gateway.PlatformConfig) SummarySink {
			writer := teams.NewTeamsSummaryWriter(platformConfig, nil, nil)
			return SinkFn(func(ctx context.Context, payload *TeamsMeetingSummaryPayload, config, existingRecord *pyjson.Object) (*pyjson.Object, error) {
				return writer.WriteSummary(ctx, teamsSummaryContent(payload), config, existingRecord)
			})
		}, nil
	}
)

// Fields of a summary payload the Teams writer renders, a nil payload or meeting reference reading as absent
func teamsSummaryContent(payload *TeamsMeetingSummaryPayload) teams.SummaryPayload {
	if payload == nil {
		return teams.SummaryPayload{}
	}
	content := teams.SummaryPayload{Title: payload.Title, Summary: payload.Summary, KeyDecisions: payload.KeyDecisions,
		ActionItems: payload.ActionItems, Risks: payload.Risks}
	if payload.MeetingRef != nil {
		content.MeetingID = payload.MeetingRef.MeetingID
	}
	return content
}

// Whether the delivery mode has its target, a webhook URL or a chat or team channel pair
func teamsDeliveryIsConfigured(teamsExtra map[string]any, teamsDelivery *pyjson.Object) bool {
	pick := func(key string) any { return or(get(teamsDelivery, key), teamsExtra[key]) }
	switch pystr.Lower(strField(or(get(teamsDelivery, "mode"), pick("delivery_mode"), ""))) {
	case "incoming_webhook":
		return pyrepr.Truthy(pick("incoming_webhook_url"))
	case "graph":
		return pyrepr.Truthy(pick("chat_id")) || (pyrepr.Truthy(pick("team_id")) && pyrepr.Truthy(pick("channel_id")))
	}
	return false
}

// Pipeline config from teams.extra.meeting_pipeline with the Teams delivery targets of an enabled platform mirrored in
func BuildPipelineRuntimeConfig(gatewayConfig *gateway.GatewayConfig) (*pyjson.Object, error) {
	teamsConfig := gatewayConfig.Platforms[gateway.Platform("teams")]
	teamsExtra := map[string]any{}
	if teamsConfig != nil {
		for k, v := range teamsConfig.Extra {
			teamsExtra[k] = v
		}
	}
	pipelineConfig, err := configDict(teamsExtra["meeting_pipeline"])
	if err != nil {
		return nil, err
	}
	if teamsConfig == nil || !teamsConfig.Enabled {
		return pipelineConfig, nil
	}
	teamsDelivery, err := configDict(get(pipelineConfig, "teams_delivery"))
	if err != nil {
		return nil, err
	}
	if deliveryMode := strField(teamsExtra["delivery_mode"]); deliveryMode != "" {
		teamsDelivery.Set("mode", deliveryMode)
	}
	for _, key := range deliveryKeys {
		switch value := teamsExtra[key].(type) {
		case nil:
		case string:
			if value != "" {
				teamsDelivery.Set(key, value)
			}
		default:
			if name := pyrepr.TypeName(value); name == "list" || name == "dict" || name == "set" {
				return nil, pyError("TypeError", "unhashable type: '"+name+"'")
			}
			teamsDelivery.Set(key, value)
		}
	}
	if teamsDelivery.Len() > 0 {
		teamsDelivery.Set("enabled", teamsDeliveryIsConfigured(teamsExtra, teamsDelivery))
		pipelineConfig.Set("teams_delivery", teamsDelivery)
	}
	return pipelineConfig, nil
}

// Pipeline over the env Graph client and the durable store, with the Teams summary writer when delivery is enabled
func BuildPipelineRuntime(ctx context.Context, host GatewayRuntimeHost) (*TeamsMeetingPipeline, error) {
	gatewayConfig := host.RunnerConfig()
	teamsConfig := gatewayConfig.Platforms[gateway.Platform("teams")]
	pipelineConfig, err := BuildPipelineRuntimeConfig(gatewayConfig)
	if err != nil {
		return nil, err
	}
	var teamsSender SummarySink
	if teamsConfig != nil && teamsConfig.Enabled {
		enabled, err := orDictGet(get(pipelineConfig, "teams_delivery"), "enabled")
		if err != nil {
			return nil, err
		}
		if pyrepr.Truthy(enabled) {
			newWriter, err := importTeamsSummaryWriter()
			switch {
			case err == nil:
				teamsSender = newWriter(teamsConfig)
			case pyrepr.ExceptionName(err) == "ImportError":
				slog.Debug("TeamsSummaryWriter unavailable; Teams outbound delivery remains disabled until the adapter layer is present.")
			default:
				return nil, err
			}
		}
	}
	client, err := runtimeGraphClient()
	if err != nil {
		return nil, err
	}
	store, err := NewTeamsPipelineStore(resolveTeamsPipelineStorePath(ctx, ""))
	if err != nil {
		return nil, err
	}
	pipeline, err := NewTeamsMeetingPipeline(client, store, pipelineConfig)
	if err != nil {
		return nil, err
	}
	pipeline.TeamsSender = teamsSender
	return pipeline, nil
}

// Attaches the pipeline to the msgraph webhook adapter, true when bound and false without an adapter or runtime
func BindGatewayRuntime(ctx context.Context, host GatewayRuntimeHost) (bool, error) {
	adapter, _ := host.Adapters().Get(gateway.PlatformMSGraphWebhook)
	if adapter == nil {
		return false, nil
	}
	if host.TeamsPipelineRuntime() != nil {
		return true, nil
	}
	setScheduler := func(scheduler gateway.NotificationScheduler) error {
		target, ok := adapter.(notificationSchedulerAdapter)
		if !ok {
			return pyError("AttributeError", fmt.Sprintf("'%s' object has no attribute 'set_notification_scheduler'", pyrepr.TypeName(adapter)))
		}
		target.SetNotificationScheduler(scheduler)
		return nil
	}
	runtime, err := runtimeBuildPipeline(ctx, host)
	if err != nil {
		message := pyrepr.ExceptionMessage(err)
		host.SetTeamsPipelineRuntimeError(&message)
		slog.Warn(fmt.Sprintf("Teams pipeline runtime unavailable: %s. Installing a drop-scheduler "+
			"so Graph notifications ack cleanly without piling up unbound.", message))
		drop := func(_ context.Context, notification *pyjson.Object, _ *gateway.MessageEvent) error {
			slog.Debug(fmt.Sprintf("Dropping Graph notification because runtime is unavailable: id=%s resource=%s",
				pyrepr.Str(get(notification, "id")), pyrepr.Str(get(notification, "resource"))))
			return nil
		}
		return false, setScheduler(drop)
	}
	tracker, _ := adapter.(backgroundTaskTracker)
	var mu sync.Mutex
	scheduled := map[string]bool{}
	schedule := func(sctx context.Context, notification *pyjson.Object, _ *gateway.MessageEvent) error {
		mu.Lock()
		job, err := runtime.CreateJobFromNotification(notification)
		if err != nil || job.Status != "received" || scheduled[job.JobID] {
			mu.Unlock()
			return err
		}
		scheduled[job.JobID] = true
		mu.Unlock()
		task := gateway.SpawnAdapterTask(sctx, func(tctx context.Context) error {
			defer func() {
				mu.Lock()
				delete(scheduled, job.JobID)
				mu.Unlock()
			}()
			_, err := runtime.RunJob(tctx, job.JobID)
			return err
		})
		if tracker != nil {
			tracker.TrackBackgroundTask(task)
		}
		return nil
	}
	if err := setScheduler(schedule); err != nil {
		return false, err
	}
	host.SetTeamsPipelineRuntime(runtime)
	host.SetTeamsPipelineRuntimeError(nil)
	return true, nil
}
