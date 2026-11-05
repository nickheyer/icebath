package cron

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

// Result is what a Runner returns for one agent run of a job
type Result struct {
	FinalResponse    string
	Completed        bool
	Failed           bool
	Error            string
	TurnExitReason   string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	APICalls         int
	Model            string
	SessionID        string
}

// Runner executes the agent turn of one job, implemented by the agent package
type Runner interface {
	Run(ctx context.Context, job *Job, prompt string) (Result, error)
}

// Target is one resolved delivery destination
type Target struct {
	Platform     string
	ChatID       string
	ThreadID     string
	ResolvedFrom string
}

// SendResult reports one target send
type SendResult struct {
	Delivered  bool
	Unverified bool
	Suppressed bool
	MessageID  string
	Error      string
	Warnings   []string
}

// Deliverer sends composed job output to resolved targets, implemented by the gateway package
type Deliverer interface {
	Send(ctx context.Context, job *Job, target Target, content string, media []string, forFailure bool) SendResult
	HomeTarget(platform string) (chatID, threadID string, ok bool)
	KnownPlatform(platform string) bool
	ConnectedPlatforms() []string
}

// Registers the cronjob_manage tool once the store, scheduler and tool action modules exist
func init() {
	tools.Register(cronjobManageToolDef())
}

// Store, scheduler and tick entry points the remaining modules must provide
var (
	_ = func(home string, now func() time.Time) *Store { return openCronStore(home, now) }
	_ = func(s *Store, runner Runner, deliverer Deliverer) *Scheduler {
		return newScheduler(s, runner, deliverer)
	}
	_ = func(ctx context.Context, sch *Scheduler) (int, error) { return schedulerTick(ctx, sch) }
	_ = func(s *Store) *Ledger { return openExecutionsLedger(s) }
	_ = func(s *Store) *IncidentStore { return openIncidents(s) }
	_ = func(s *Store) *Notepad { return openNotepad(s) }
	_ = func(s *Store) *DeliveryQueue { return openDeliveryQueue(s) }
	_ = func(s *Store, job *Job) MonitorOutcome { return checkMonitor(s, job) }
	_ = func(prompt, script string) error { return checkGatewayLifecycle(prompt, script) }
	_ = func(prompt string) string { return scanCronPrompt(prompt) }
	_ = func(s *Store, job *Job, extra string) (string, error) { return buildJobPrompt(s, job, extra) }
	_ = func(s *Store, job *Job, content string, forFailure bool) []Target {
		return resolveDeliveryTargets(s, job, forFailure)
	}
	_ = func() []AutomationBlueprint { return listBlueprints() }
	_ = func(s *Store) []Suggestion { return listPendingSuggestions(s) }
	_ = func(s *Store, job *Job) [][2]string { return cronListRows(s, job) }
	_ = func(s *Store) []string { return cronStatusLines(s) }
	_ = func(arg string) []string { return cronCommandLines(arg) }
	_ = func(fn func() bool, interval time.Duration) *ScheduledHandle { return schedulePeriodic(fn, interval) }
	_ = func(s *Store, snapshotID string) map[string]any {
		return restoreCronPromptFieldsIfDegraded(s, snapshotID)
	}
	_ = func() Provider { return newChronosProvider() }
)
