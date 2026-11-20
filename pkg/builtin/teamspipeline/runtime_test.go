package teamspipeline

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Webhook adapter double holding the bound scheduler and the tasks it was asked to track
type fakeWebhookAdapter struct {
	gateway.PlatformAdapter
	mu              sync.Mutex
	scheduler       gateway.NotificationScheduler
	backgroundTasks []*gateway.AdapterTask
}

func (a *fakeWebhookAdapter) SetNotificationScheduler(scheduler gateway.NotificationScheduler) {
	a.scheduler = scheduler
}

func (a *fakeWebhookAdapter) TrackBackgroundTask(task *gateway.AdapterTask) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.backgroundTasks = append(a.backgroundTasks, task)
}

// Waits for every tracked task like gathering the adapter background tasks
func (a *fakeWebhookAdapter) gather(t *testing.T) {
	t.Helper()
	a.mu.Lock()
	tasks := append([]*gateway.AdapterTask(nil), a.backgroundTasks...)
	a.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for _, task := range tasks {
		for !task.Done() {
			if time.Now().After(deadline) {
				t.Fatal("background task still running")
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// Gateway runner double with the adapters, config and runtime slot the binding touches
type fakeGatewayHost struct {
	adapters     map[gateway.Platform]gateway.PlatformAdapter
	config       *gateway.GatewayConfig
	runtime      any
	runtimeError *string
}

func (h *fakeGatewayHost) RunnerConfig() *gateway.GatewayConfig { return h.config }
func (h *fakeGatewayHost) Adapters() *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	adapters := pydict.New[gateway.Platform, gateway.PlatformAdapter]()
	for platform, adapter := range h.adapters {
		adapters.Set(platform, adapter)
	}
	return adapters
}
func (h *fakeGatewayHost) TeamsPipelineRuntime() any                    { return h.runtime }
func (h *fakeGatewayHost) SetTeamsPipelineRuntime(runtime any)          { h.runtime = runtime }
func (h *fakeGatewayHost) SetTeamsPipelineRuntimeError(message *string) { h.runtimeError = message }

// Pipeline double whose accept persists nothing and whose processing blocks until released
type fakePipeline struct {
	mu            sync.Mutex
	notifications []*pyjson.Object
	release       chan struct{}
}

func (p *fakePipeline) CreateJobFromNotification(notification *pyjson.Object) (*TeamsMeetingPipelineJob, error) {
	return &TeamsMeetingPipelineJob{JobID: "job-" + get(notification, "id").(string), Status: "received"}, nil
}

func (p *fakePipeline) RunJob(_ context.Context, jobOrID any) (*TeamsMeetingPipelineJob, error) {
	<-p.release
	p.mu.Lock()
	defer p.mu.Unlock()
	p.notifications = append(p.notifications, obj("id", strings.TrimPrefix(jobOrID.(string), "job-")))
	return nil, nil
}

func (p *fakePipeline) seen() []*pyjson.Object {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*pyjson.Object(nil), p.notifications...)
}

func useRuntimeBuilder(t *testing.T, build func(ctx context.Context, host GatewayRuntimeHost) (PipelineRuntime, error)) {
	t.Helper()
	prev := runtimeBuildPipeline
	runtimeBuildPipeline = build
	t.Cleanup(func() { runtimeBuildPipeline = prev })
}

func teamsPlatformConfig(extra map[string]any) *gateway.PlatformConfig {
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled, cfg.Extra = true, extra
	return cfg
}

func TestRuntimeConfigUsesExistingTeamsPlatformSettings(t *testing.T) {
	gatewayConfig := &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{
		gateway.Platform("teams"): teamsPlatformConfig(map[string]any{
			"delivery_mode": "graph", "team_id": "team-1", "channel_id": "channel-1",
			"meeting_pipeline": map[string]any{"transcript_min_chars": int64(120), "notion": map[string]any{"enabled": true, "database_id": "db-1"}},
		}),
	}}

	runtimeConfig, err := BuildPipelineRuntimeConfig(gatewayConfig)
	if err != nil {
		t.Fatal(err)
	}

	if got := get(runtimeConfig, "transcript_min_chars"); got != int64(120) {
		t.Fatalf("transcript_min_chars %v", got)
	}
	if notion, _ := get(runtimeConfig, "notion").(map[string]any); notion["database_id"] != "db-1" {
		t.Fatalf("notion %v", get(runtimeConfig, "notion"))
	}
	delivery, _ := get(runtimeConfig, "teams_delivery").(*pyjson.Object)
	want := map[string]any{"enabled": true, "mode": "graph", "team_id": "team-1", "channel_id": "channel-1"}
	if delivery == nil || !reflect.DeepEqual(delivery.Vals, want) {
		t.Fatalf("teams_delivery %v", get(runtimeConfig, "teams_delivery"))
	}
}

func TestBindGatewayRuntimeAttachesScheduler(t *testing.T) {
	adapter := &fakeWebhookAdapter{}
	pipeline := &fakePipeline{release: make(chan struct{})}
	host := &fakeGatewayHost{adapters: map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformMSGraphWebhook: adapter},
		config: &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}}
	useRuntimeBuilder(t, func(context.Context, GatewayRuntimeHost) (PipelineRuntime, error) { return pipeline, nil })

	bound, err := BindGatewayRuntime(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}

	if !bound {
		t.Fatal("not bound")
	}
	if host.runtime != PipelineRuntime(pipeline) {
		t.Fatalf("runtime %v", host.runtime)
	}
	if adapter.scheduler == nil {
		t.Fatal("no scheduler")
	}

	notification := obj("id", "notif-1")
	accepted := make(chan error, 1)
	go func() { accepted <- adapter.scheduler(context.Background(), notification, &gateway.MessageEvent{}) }()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("scheduler waited for the processing pipeline")
	}
	if seen := pipeline.seen(); len(seen) != 0 {
		t.Fatalf("notifications before release %v", seen)
	}
	close(pipeline.release)
	adapter.gather(t)
	if seen := pipeline.seen(); len(seen) != 1 || !pyjson.Equal(seen[0], notification) {
		t.Fatalf("notifications %v", seen)
	}
}

// Real pipeline whose processing is replaced by a recorder that blocks until released
type claimRecordingPipeline struct {
	*TeamsMeetingPipeline
	mu      sync.Mutex
	runs    []string
	release chan struct{}
}

func (p *claimRecordingPipeline) RunJob(_ context.Context, jobOrID any) (*TeamsMeetingPipelineJob, error) {
	p.mu.Lock()
	p.runs = append(p.runs, jobOrID.(string))
	p.mu.Unlock()
	<-p.release
	return nil, nil
}

func TestDuplicateNoIDNotificationsInOneBatchRunTheJobOnce(t *testing.T) {
	adapter := &fakeWebhookAdapter{}
	store, err := NewTeamsPipelineStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	real, err := NewTeamsMeetingPipeline(&scriptGraph{}, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := &claimRecordingPipeline{TeamsMeetingPipeline: real, release: make(chan struct{})}
	useRuntimeBuilder(t, func(context.Context, GatewayRuntimeHost) (PipelineRuntime, error) { return pipeline, nil })
	host := &fakeGatewayHost{adapters: map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformMSGraphWebhook: adapter},
		config: &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}}
	if bound, err := BindGatewayRuntime(context.Background(), host); err != nil || !bound {
		t.Fatalf("bound %v err %v", bound, err)
	}
	notification := obj("changeType", "created", "resource", "communications/onlineMeetings/m1", "resourceData", obj("id", "m1"))
	for range 2 {
		if err := adapter.scheduler(context.Background(), notification.Copy(), &gateway.MessageEvent{}); err != nil {
			t.Fatal(err)
		}
	}
	close(pipeline.release)
	adapter.gather(t)
	pipeline.mu.Lock()
	defer pipeline.mu.Unlock()
	if len(pipeline.runs) != 1 {
		t.Fatalf("runs %v", pipeline.runs)
	}
}

func TestBuildPipelineRuntimeSkipsSenderWhenAdapterLayerIsUnavailable(t *testing.T) {
	host := &fakeGatewayHost{config: &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{
		gateway.Platform("teams"): teamsPlatformConfig(map[string]any{"delivery_mode": "graph", "team_id": "team-1", "channel_id": "channel-1"}),
	}}}
	prevClient, prevImport := runtimeGraphClient, importTeamsSummaryWriter
	t.Cleanup(func() { runtimeGraphClient, importTeamsSummaryWriter = prevClient, prevImport })
	runtimeGraphClient = func() (graphClient, error) { return &scriptGraph{}, nil }
	importTeamsSummaryWriter = func() (func(*gateway.PlatformConfig) SummarySink, error) {
		return nil, pyError("ImportError", "cannot import name 'register' from 'plugins.platforms.teams.adapter' (unknown location)")
	}
	t.Setenv("MSGRAPH_WEBHOOK_STORE_PATH", filepath.Join(t.TempDir(), "teams-pipeline-store.json"))

	runtime, err := BuildPipelineRuntime(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}

	if runtime.TeamsSender != nil {
		t.Fatalf("teams sender %v", runtime.TeamsSender)
	}
}

func TestBindGatewayRuntimeDropsNotificationsWhenTheRuntimeCannotBeBuilt(t *testing.T) {
	adapter := &fakeWebhookAdapter{}
	host := &fakeGatewayHost{adapters: map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformMSGraphWebhook: adapter},
		config: &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}}
	useRuntimeBuilder(t, func(context.Context, GatewayRuntimeHost) (PipelineRuntime, error) {
		return nil, pyError("ValueError", "MSGRAPH_TENANT_ID is required.")
	})

	bound, err := BindGatewayRuntime(context.Background(), host)
	if err != nil || bound {
		t.Fatalf("bound %v err %v", bound, err)
	}
	if host.runtime != nil || host.runtimeError == nil || *host.runtimeError != "MSGRAPH_TENANT_ID is required." {
		t.Fatalf("runtime %v error %v", host.runtime, host.runtimeError)
	}
	if adapter.scheduler == nil {
		t.Fatal("no drop scheduler")
	}
	if err := adapter.scheduler(context.Background(), obj("id", "notif-9", "resource", "r"), &gateway.MessageEvent{}); err != nil {
		t.Fatal(err)
	}
	if len(adapter.backgroundTasks) != 0 {
		t.Fatalf("tasks %d", len(adapter.backgroundTasks))
	}

	if bound, err := BindGatewayRuntime(context.Background(), &fakeGatewayHost{adapters: map[gateway.Platform]gateway.PlatformAdapter{}}); err != nil || bound {
		t.Fatalf("bound without adapter %v err %v", bound, err)
	}
	host.runtime = &fakePipeline{}
	if bound, err := BindGatewayRuntime(context.Background(), host); err != nil || !bound {
		t.Fatalf("rebind %v err %v", bound, err)
	}
}

// Adapter over the real base adapter whose shutdown cancels its tracked background tasks
type baseWebhookAdapter struct {
	*gateway.BasePlatformAdapter
	scheduler gateway.NotificationScheduler
}

func (a *baseWebhookAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *baseWebhookAdapter) Disconnect(context.Context) error            { return nil }
func (a *baseWebhookAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{}, nil
}
func (a *baseWebhookAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}
func (a *baseWebhookAdapter) SetNotificationScheduler(scheduler gateway.NotificationScheduler) {
	a.scheduler = scheduler
}

// Pipeline double whose run waits for cancellation and reports how it ended
type cancellablePipeline struct {
	started chan struct{}
	ended   chan error
}

func (p *cancellablePipeline) CreateJobFromNotification(notification *pyjson.Object) (*TeamsMeetingPipelineJob, error) {
	return &TeamsMeetingPipelineJob{JobID: "job-" + get(notification, "id").(string), Status: "received"}, nil
}

func (p *cancellablePipeline) RunJob(ctx context.Context, _ any) (*TeamsMeetingPipelineJob, error) {
	close(p.started)
	<-ctx.Done()
	p.ended <- ctx.Err()
	return nil, ctx.Err()
}

func TestBindGatewayRuntimeRunsJobsAsAdapterBackgroundTasks(t *testing.T) {
	adapter := &baseWebhookAdapter{}
	adapter.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformMSGraphWebhook, adapter)
	pipeline := &cancellablePipeline{started: make(chan struct{}), ended: make(chan error, 1)}
	useRuntimeBuilder(t, func(context.Context, GatewayRuntimeHost) (PipelineRuntime, error) { return pipeline, nil })
	host := &fakeGatewayHost{adapters: map[gateway.Platform]gateway.PlatformAdapter{gateway.PlatformMSGraphWebhook: adapter},
		config: &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{}}}
	if bound, err := BindGatewayRuntime(context.Background(), host); err != nil || !bound {
		t.Fatalf("bound %v err %v", bound, err)
	}
	requestCtx, endRequest := context.WithCancel(context.Background())
	if err := adapter.scheduler(requestCtx, obj("id", "notif-1"), &gateway.MessageEvent{}); err != nil {
		t.Fatal(err)
	}
	endRequest()
	select {
	case <-pipeline.started:
	case <-time.After(10 * time.Second):
		t.Fatal("job never started")
	}
	select {
	case err := <-pipeline.ended:
		t.Fatalf("job ended with the webhook request: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	adapter.CancelBackgroundTasks(context.Background())
	select {
	case err := <-pipeline.ended:
		if err != context.Canceled {
			t.Fatalf("job ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("adapter shutdown left the job running")
	}
}
