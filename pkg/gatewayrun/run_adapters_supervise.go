package gatewayrun

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Supervision class attributes of the runner with their hermes values
const (
	defaultMaxSupervisedRestarts        = 5
	defaultSupervisedHealthySecs        = 300
	defaultReconnectWatcherSlowRetrySec = 300
	defaultMaxSlowWatcherRespawns       = 6
)

// Fans a normalised reaction event out to the hook registry, a failing emit only logged
func (r *GatewayRunner) handleReactionEvent(ctx context.Context, hookCtx map[string]any) {
	eventName := "reaction:added"
	if name := hookCtx["event_name"]; pyrepr.Truthy(name) {
		eventName = pyrepr.Str(name)
	}
	if err := r.hooks.emit(ctx, eventName, hookCtx); err != nil {
		logSuppressedExc(slog.LevelDebug, err, "[Gateway] reaction hook emit failed")
	}
}

// Registers a task in the background set, removed again once it finishes
func (r *GatewayRunner) retainBackgroundTask(task *gatewayTask) *gatewayTask {
	r.taskSetsMu.Lock()
	if r.backgroundTasks == nil {
		r.backgroundTasks = map[*gatewayTask]struct{}{}
	}
	tasks := r.backgroundTasks
	r.taskSetsMu.Unlock()
	return r.trackTaskIn(tasks, task)
}

// Registers a task in a lifecycle set with self removal on completion
func (r *GatewayRunner) trackTaskIn(tasks map[*gatewayTask]struct{}, task *gatewayTask) *gatewayTask {
	r.taskSetsMu.Lock()
	tasks[task] = struct{}{}
	r.taskSetsMu.Unlock()
	task.addDoneCallback(func(*gatewayFuture) {
		r.taskSetsMu.Lock()
		delete(tasks, task)
		r.taskSetsMu.Unlock()
	})
	return task
}

// Marks a clean exit with its reason and wakes the shutdown waiter
func (r *GatewayRunner) requestCleanExit(reason string) {
	r.exitCleanly = true
	r.exitReason = &reason
	r.shutdownEvent.Set()
}

// Capped exponential respawn delay of a supervised task
func supervisedBackoff(attempt int) float64 {
	return math.Min(60, math.Pow(2, float64(min(attempt, 6))))
}

// Hooks of a supervised task, a tracker fired on every spawn and an owner told when supervision gives up
type supervisedHooks struct {
	onSpawn  func(task *gatewayTask)
	onGiveUp func(name string)
}

// Sleeps for seconds or until ctx ends, the context error when it ended first
func sleepCtx(ctx context.Context, seconds float64) error {
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Launches a long lived supervised background task that logs its death and respawns with capped backoff
func (r *GatewayRunner) spawnSupervised(factory func(ctx context.Context) error, name string, restart bool, attempt int, hooks supervisedHooks) *gatewayTask {
	started := pythread.Monotonic()
	task := spawnGatewayTask(context.Background(), name, func(taskCtx context.Context) (any, error) {
		return nil, factory(taskCtx)
	})
	r.taskSetsMu.Lock()
	if r.supervisedWatchers == nil {
		r.supervisedWatchers = map[*gatewayTask]struct{}{}
	}
	r.supervisedWatchers[task] = struct{}{}
	r.taskSetsMu.Unlock()
	r.retainBackgroundTask(task)
	if hooks.onSpawn != nil {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					runLog().Debug(fmt.Sprintf("on_spawn callback for %s raised", name), "error", rec)
				}
			}()
			hooks.onSpawn(task)
		}()
	}
	task.addDoneCallback(func(done *gatewayFuture) {
		r.taskSetsMu.Lock()
		delete(r.backgroundTasks, task)
		r.taskSetsMu.Unlock()
		if done.cancelled() {
			return
		}
		_, err := done.wait(context.Background())
		if err == nil {
			return
		}
		runLog().Error(fmt.Sprintf("Supervised task %s died: %s", name, pyrepr.ExceptionRepr(err)), "exc_info", err)
		if !(restart && r.running.Load()) {
			return
		}
		effective := attempt
		if pythread.Monotonic()-started >= float64(r.supervisedHealthySecs) {
			effective = 0
		}
		if effective >= r.maxSupervisedRestarts {
			runLog().Error(fmt.Sprintf("Supervised task %s died %d times in rapid succession "+
				"(each within %ds of restart) — giving up restarts", name, effective, r.supervisedHealthySecs))
			if hooks.onGiveUp != nil {
				func() {
					defer func() {
						if rec := recover(); rec != nil {
							runLog().Debug(fmt.Sprintf("on_give_up callback for %s raised", name), "error", rec)
						}
					}()
					hooks.onGiveUp(name)
				}()
			}
			return
		}
		backoff := supervisedBackoff(effective)
		r.retainBackgroundTask(spawnGatewayTask(context.Background(), name+"-respawn", func(respawnCtx context.Context) (any, error) {
			if err := sleepCtx(respawnCtx, backoff); err != nil {
				return nil, err
			}
			if r.running.Load() {
				r.spawnSupervised(factory, name, restart, effective+1, hooks)
			}
			return nil, nil
		}))
	})
	return task
}

// Whether the task is a permanent supervised watcher the idle check ignores
func (r *GatewayRunner) isSupervisedWatcher(task *gatewayTask) bool {
	r.taskSetsMu.Lock()
	defer r.taskSetsMu.Unlock()
	_, ok := r.supervisedWatchers[task]
	return ok
}

// Normalized policy when every adapter is down, exit unless the config asks to stay alive
func (r *GatewayRunner) onAllAdaptersDown() string {
	if r.config == nil {
		return "exit"
	}
	value := r.config.OnAllAdaptersDown
	for _, policy := range gateway.OnAllAdaptersDownPolicies {
		if value == policy {
			return value
		}
	}
	return "exit"
}
