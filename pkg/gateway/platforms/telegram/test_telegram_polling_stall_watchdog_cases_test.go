package telegram

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Polling lifecycle whose last getUpdates round trip completed stalledSeconds ago with a running updater
func stalledLifecycle(t *testing.T, stalledSeconds float64) (*pollingLifecycle, *fakePollingHost) {
	t.Helper()
	p, host := newTestLifecycle(t, newMockApp())
	last := monotonicNow() - stalledSeconds
	p.lastProgress = &last
	p.generation = 1
	return p, host
}

// Waits for the recovery task the check scheduled and returns it
func awaitRecovery(t *testing.T, p *pollingLifecycle) *gateway.AdapterTask {
	t.Helper()
	p.mu.Lock()
	task := p.errorTask
	p.mu.Unlock()
	if task == nil {
		t.Fatal("expected a recovery task")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := task.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestRecentProgressDoesNotEscalate(t *testing.T) {
	p, _ := stalledLifecycle(t, 1)
	p.checkPollingStall(context.Background())
	if p.errorTask != nil {
		t.Fatal("recent progress must not escalate")
	}
}

func TestStalledLongPollHandsOffToSupervisor(t *testing.T) {
	p, host := stalledLifecycle(t, 400)
	p.checkPollingStall(context.Background())
	awaitRecovery(t, p)
	if code := host.FatalErrorCode(); code == nil || *code != "telegram_network_error" || !host.retryable || host.notifyCount() != 1 {
		t.Fatal("a confirmed stall must hand the adapter to the supervisor")
	}
}

func TestGenerationWithNoProgressEverUsesGenerationAge(t *testing.T) {
	p, host := stalledLifecycle(t, 0)
	started := monotonicNow() - 400
	p.lastProgress, p.generationStarted = nil, &started
	p.checkPollingStall(context.Background())
	awaitRecovery(t, p)
	if !host.HasFatalError() {
		t.Fatal("generation age past the stall timeout must escalate")
	}
}

func TestStallIgnoredWhileRecoveryInFlight(t *testing.T) {
	p, _ := stalledLifecycle(t, 400)
	inflight := gateway.SpawnAdapterTask(context.Background(), hang)
	defer inflight.Cancel()
	p.errorTask = inflight
	p.checkPollingStall(context.Background())
	if p.errorTask != inflight {
		t.Fatal("in flight recovery must keep ownership")
	}
}

func TestStallCheckSkippedInWebhookMode(t *testing.T) {
	p, _ := stalledLifecycle(t, 400)
	p.webhookMode = true
	p.checkPollingStall(context.Background())
	if p.errorTask != nil {
		t.Fatal("webhook mode has no long poll to watch")
	}
}
