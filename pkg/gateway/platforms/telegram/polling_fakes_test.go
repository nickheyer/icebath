package telegram

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Request pool double recording shutdown, initialize and rebuild calls
type fakeRequest struct {
	mu           sync.Mutex
	shutdowns    int
	initializes  int
	shutdownFn   func(ctx context.Context) error
	initializeFn func(ctx context.Context) error
	clientOpen   bool
	rebuilt      int
	orphanFn     func(ctx context.Context) error
}

func (r *fakeRequest) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.shutdowns++
	fn := r.shutdownFn
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

func (r *fakeRequest) Initialize(ctx context.Context) error {
	r.mu.Lock()
	r.initializes++
	fn := r.initializeFn
	r.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

func (r *fakeRequest) RebuildClient() (func(ctx context.Context) error, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.clientOpen {
		return nil, false, nil
	}
	r.rebuilt++
	return r.orphanFn, true, nil
}

func (r *fakeRequest) counts() (int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.shutdowns, r.initializes
}

// Updater double with scripted start and stop behaviour
type fakeUpdater struct {
	mu          sync.Mutex
	running     bool
	starts      int
	stops       int
	startFn     func(ctx context.Context, drop bool, cb func(error)) error
	stopFn      func(ctx context.Context) error
	disarmFound bool
	drops       []bool
}

func (u *fakeUpdater) Running() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.running
}

func (u *fakeUpdater) StartPolling(ctx context.Context, drop bool, cb func(error)) error {
	u.mu.Lock()
	u.starts++
	u.drops = append(u.drops, drop)
	fn := u.startFn
	u.mu.Unlock()
	if fn != nil {
		return fn(ctx, drop, cb)
	}
	return nil
}

func (u *fakeUpdater) Stop(ctx context.Context) error {
	u.mu.Lock()
	u.stops++
	fn := u.stopFn
	u.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return nil
}

func (u *fakeUpdater) DisarmRetryLoop() (bool, bool) { return u.disarmFound, u.disarmFound }

func (u *fakeUpdater) startCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.starts
}

// Bot double answering getMe and getWebhookInfo through scripted functions
type fakePollingBot struct {
	mu        sync.Mutex
	getMeN    int
	getMeFn   func(ctx context.Context) (*models.User, error)
	webhookFn func(ctx context.Context) (*models.WebhookInfo, error)
	deletes   int
}

func (b *fakePollingBot) GetMe(ctx context.Context) (*models.User, error) {
	b.mu.Lock()
	b.getMeN++
	fn := b.getMeFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx)
	}
	return &models.User{ID: 1, Username: "bot"}, nil
}

func (b *fakePollingBot) GetWebhookInfo(ctx context.Context) (*models.WebhookInfo, error) {
	if b.webhookFn != nil {
		return b.webhookFn(ctx)
	}
	return &models.WebhookInfo{}, nil
}

func (b *fakePollingBot) DeleteWebhook(ctx context.Context, drop bool) (bool, error) {
	b.mu.Lock()
	b.deletes++
	b.mu.Unlock()
	return true, nil
}

func (b *fakePollingBot) getMeCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getMeN
}

// Application double wiring the bot, updater and request doubles
type fakePollingApp struct {
	bot       *fakePollingBot
	updater   *fakeUpdater
	polling   *fakeRequest
	general   *fakeRequest
	shutdowns int
}

func (a *fakePollingApp) PollingBot() pollingBot {
	if a.bot == nil {
		return nil
	}
	return a.bot
}

func (a *fakePollingApp) Updater() pollingUpdater {
	if a.updater == nil {
		return nil
	}
	return a.updater
}

func (a *fakePollingApp) Requests() (requestPool, requestPool) {
	var polling, general requestPool
	if a.polling != nil {
		polling = a.polling
	}
	if a.general != nil {
		general = a.general
	}
	return polling, general
}

func (a *fakePollingApp) Shutdown(context.Context) error {
	a.shutdowns++
	return nil
}

// Mock application of the hermes tests, a running updater and explicit polling and general requests
func newMockApp() *fakePollingApp {
	return &fakePollingApp{bot: &fakePollingBot{}, updater: &fakeUpdater{running: true}, polling: &fakeRequest{},
		general: &fakeRequest{}}
}

// Adapter double recording fatal errors, runtime statuses, notifications and tracked tasks
type fakePollingHost struct {
	mu        sync.Mutex
	fatalCode *string
	fatalMsg  string
	retryable bool
	connected bool
	degraded  int
	statuses  []string
	notifies  int
	tasks     []*gateway.AdapterTask
	identity  *models.User
}

func (h *fakePollingHost) Name() string { return "Telegram" }

func (h *fakePollingHost) HasFatalError() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fatalCode != nil
}

func (h *fakePollingHost) FatalErrorCode() *string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fatalCode
}

func (h *fakePollingHost) SetFatalError(_ context.Context, code, message string, retryable bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.fatalCode, h.fatalMsg, h.retryable = &code, message, retryable
}

func (h *fakePollingHost) IsConnected() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.connected
}

func (h *fakePollingHost) MarkDegraded(context.Context) {
	h.mu.Lock()
	h.degraded++
	h.mu.Unlock()
}

func (h *fakePollingHost) WriteRuntimeStatusSafe(_ context.Context, statusContext string, _ map[string]any) {
	h.mu.Lock()
	h.statuses = append(h.statuses, statusContext)
	h.mu.Unlock()
}

func (h *fakePollingHost) NotifyFatalError(context.Context) error {
	h.mu.Lock()
	h.notifies++
	h.mu.Unlock()
	return nil
}

func (h *fakePollingHost) TrackBackgroundTask(task *gateway.AdapterTask) {
	h.mu.Lock()
	h.tasks = append(h.tasks, task)
	h.mu.Unlock()
}

func (h *fakePollingHost) NoteBotIdentity(user *models.User) {
	h.mu.Lock()
	h.identity = user
	h.mu.Unlock()
}

func (h *fakePollingHost) pendingTasks() []*gateway.AdapterTask {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []*gateway.AdapterTask
	for _, t := range h.tasks {
		if !t.Done() {
			out = append(out, t)
		}
	}
	return out
}

func (h *fakePollingHost) notifyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.notifies
}

// Lifecycle over a fake host with zero backoff, like the hermes tests patching asyncio.sleep
func newTestLifecycle(t *testing.T, app pollingApp) (*pollingLifecycle, *fakePollingHost) {
	t.Helper()
	host := &fakePollingHost{}
	p := newPollingLifecycle(host)
	p.timings.networkBaseDelay, p.timings.networkMaxDelay = 0, 0
	p.timings.conflictRetryDelayBase, p.timings.conflictRetryDelayFactor = 0, 0
	p.app = app
	t.Cleanup(func() {
		for _, task := range host.pendingTasks() {
			task.Cancel()
		}
		p.mu.Lock()
		tasks := []*gateway.AdapterTask{p.errorTask, p.verifierTask}
		p.mu.Unlock()
		for _, task := range tasks {
			if task != nil {
				task.Cancel()
			}
		}
	})
	return p, host
}

// Records progress for the current generation and waits for its verifier to settle
func completeCurrentPollingGeneration(t *testing.T, p *pollingLifecycle) {
	t.Helper()
	p.mu.Lock()
	verifier, generation := p.verifierTask, p.generation
	p.mu.Unlock()
	p.recordPollingProgress(context.Background(), generation)
	if verifier != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = verifier.Wait(ctx)
	}
}

// Blocks until ctx ends, a hung close or shutdown
func hang(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
