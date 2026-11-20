package gateway

import (
	"context"
	"time"
)

// Physical turn worker a stop must see exit, done once its executor thread returned
type TurnWorkerHandle interface {
	WorkerDone() bool
}

// Registers a turn worker and its agent holder on the active authority, nil when the runner serves none
func TrackTurnWorker(ctx context.Context, runner any, worker TurnWorkerHandle, holder sideAgentInterruptible) (*SessionAuthority, error) {
	a, err := activeAuthority(ctx, runner)
	if err != nil || a == nil {
		return nil, err
	}
	physicalWorkers(a)
	a.mu.Lock()
	if a.turnWorkers == nil {
		a.turnWorkers = map[TurnWorkerHandle]sideAgentInterruptible{}
	}
	a.turnWorkers[worker] = holder
	a.mu.Unlock()
	return a, nil
}

// Live turn workers of the authority with their holders, pruning every worker whose thread exited
func physicalWorkers(a *SessionAuthority) []sideAgentInterruptible {
	a.mu.Lock()
	defer a.mu.Unlock()
	var holders []sideAgentInterruptible
	for worker, holder := range a.turnWorkers {
		if worker.WorkerDone() {
			delete(a.turnWorkers, worker)
			continue
		}
		holders = append(holders, holder)
	}
	return holders
}

// Stops every turn, hard interrupts each live worker and side agent and cancels cron runs
func StopAuthorityWork(a *SessionAuthority) {
	StopAuthorityTurns(a.Runner, a, true, nil)
	holders := physicalWorkers(a)
	for _, holder := range sideAgents(a) {
		interruptible, _ := holder.(sideAgentInterruptible)
		holders = append(holders, interruptible)
	}
	for _, holder := range holders {
		if holder == nil {
			continue
		}
		if agent := holder.HolderAgent(); agent != nil {
			requestHardInterruptWith(agent, "Profile runtime is stopping", "")
		}
	}
	a.mu.Lock()
	cancels := make([]interface{ Set() }, 0, len(a.cronCancellations))
	for _, cancel := range a.cronCancellations {
		cancels = append(cancels, cancel)
	}
	a.mu.Unlock()
	for _, cancel := range cancels {
		cancel.Set()
	}
}

// Stops the authority's work until none is left, TimeoutError once timeout seconds pass first
func JoinAuthorityWork(ctx context.Context, a *SessionAuthority, timeout float64) error {
	deadline := time.Now().Add(time.Duration(timeout * float64(time.Second)))
	for {
		StopAuthorityWork(a)
		if len(physicalWorkers(a)) == 0 && len(executingSessions(a)) == 0 && len(mutationTasks(a)) == 0 && len(sideAgents(a)) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return &PyError{Kind: "TimeoutError", Msg: "Profile workers did not stop; ownership retained"}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Done signals of every task family the authority owns directly, deduplicated in order
func AuthorityTasks(a *SessionAuthority) []<-chan struct{} {
	var tasks []<-chan struct{}
	seen := map[<-chan struct{}]bool{}
	add := func(done <-chan struct{}) {
		if done != nil && !seen[done] {
			seen[done] = true
			tasks = append(tasks, done)
		}
	}
	a.mu.Lock()
	for _, live := range a.Sessions {
		if live != nil && live.drain != nil {
			add(live.drain)
		}
	}
	for task := range a.botReceiptTasks {
		add(task.done)
	}
	a.mu.Unlock()
	for _, task := range mutationTasks(a) {
		add(task.done)
	}
	return tasks
}
