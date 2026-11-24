//go:build !icebath_core && !icebath_noextras

package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/settings"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

var (
	kanbanImageExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true}
	kanbanVideoExts = map[string]bool{".mp4": true, ".mov": true, ".avi": true, ".mkv": true, ".webm": true, ".3gp": true}
)

const (
	kanbanGCIntervalSeconds = 3600.0
	kanbanHealthWindow      = 6
)

// Sleep of the kanban watcher loops, a context aware timer
var asyncioSleep = sleepCtx

// Dispatcher lock and notifier failure counts a runner keeps for its kanban watchers
type runnerKanbanState struct {
	kanbanDispatcherLockMu     sync.Mutex
	kanbanDispatcherLockHandle *os.File
	kanbanSubFailCounts        map[kanbanSubKey]int
}

// Binds the kanban slash command, watchers and idle probe of the runner
func init() {
	kanbanCommand, kanbanBusyProbe = (*GatewayRunner).runKanbanCommand, kanbanTaskBusy
	kanbanWatchers = func(r *GatewayRunner) []runnerWatcher {
		return []runnerWatcher{{"kanban_notifier_watcher", func(c context.Context) error { return r.kanbanNotifierWatcher(c, 5.0) }},
			{"kanban_dispatcher_watcher", r.kanbanDispatcherWatcher}}
	}
}

// Runs fn and turns a panic into its error the way an except Exception clause catches a raise
func catchPanic(fn func() error) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			if e, ok := rec.(error); ok {
				err = e
				return
			}
			err = fmt.Errorf("%v", rec)
		}
	}()
	return fn()
}

// Whether this gateway holds the singleton dispatcher lock
func (r *GatewayRunner) ownsKanbanDispatcherLock() bool {
	r.kanbanDispatcherLockMu.Lock()
	defer r.kanbanDispatcherLockMu.Unlock()
	return r.kanbanDispatcherLockHandle != nil
}

// Clears notifier visible lock ownership before releasing the OS lock
func (r *GatewayRunner) releaseKanbanDispatcherLock() {
	r.kanbanDispatcherLockMu.Lock()
	handle := r.kanbanDispatcherLockHandle
	r.kanbanDispatcherLockHandle = nil
	r.kanbanDispatcherLockMu.Unlock()
	releaseSingletonLock(handle)
}

// Sleeps interval floored to one second in one second slices so stop never waits a full interval
func (r *GatewayRunner) sleepBetweenTicks(ctx context.Context, interval float64) error {
	interval = max(interval, 1.0)
	slept := 0.0
	for slept < interval && r.running.Load() {
		if err := asyncioSleep(ctx, min(1.0, interval-slept)); err != nil {
			return err
		}
		slept += 1.0
	}
	return nil
}

// Polls kanban notify subscriptions and delivers terminal task events to their chats
func (r *GatewayRunner) kanbanNotifierWatcher(ctx context.Context, interval float64) error {
	if !settings.KanbanNotifyInGateway.Get(ctx) {
		pkgLog().Info("kanban notifier: turned off in settings")
		return nil
	}

	if r.kanbanSubFailCounts == nil {
		r.kanbanSubFailCounts = map[kanbanSubKey]int{}
	}
	subFailCounts := r.kanbanSubFailCounts
	notifierProfile := r.kanbanNotifierProfile
	if notifierProfile == "" {
		notifierProfile = r.activeProfileName(ctx)
	}
	r.kanbanNotifierProfile = notifierProfile

	if err := asyncioSleep(ctx, 5); err != nil {
		return err
	}

	gcNextAt := 0.0
	for r.running.Load() {
		err := catchPanic(func() error {
			gcDue := timeutil.Monotonic() >= gcNextAt
			retention := int64(30)
			if gcDue {
				gcNextAt = timeutil.Monotonic() + kanbanGCIntervalSeconds
				retention = gcRetentionDays(ctx)
			}
			deliveries, err := awaitThread(ctx, func() ([]*kanbanDelivery, error) {
				return notifierCollect(ctx, r, notifierProfile, gcDue, retention)
			})
			if err != nil {
				return err
			}
			for _, d := range deliveries {
				if err := newKanbanNotification(ctx, r, d, subFailCounts).deliver(ctx); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pkgLog().Warn(fmt.Sprintf("kanban notifier tick failed: %s", anyutil.String(err)))
		}
		if err := r.sleepBetweenTicks(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}

// Calls one kanban notify operation for a subscription on its board under pin first resolution
func (r *GatewayRunner) kanbanSubOp(ctx context.Context, board string, op string, sub *jsonutil.Object, extra map[string]int64) error {
	pinned := kanban.PinFirstBoardResolution(ctx)
	var boardArg *string
	if board != "" {
		boardArg = &board
	}
	conn, err := kanbanConnect(pinned, "", boardArg)
	if err != nil {
		return err
	}
	defer conn.Close()
	taskID, platform, chatID := subText(sub, "task_id"), subText(sub, "platform"), subText(sub, "chat_id")
	thread := subTruthyText(sub, "thread_id")
	switch op {
	case "advance_notify_cursor":
		return kanban.AdvanceNotifyCursor(pinned, conn, taskID, platform, chatID, &thread, extra["new_cursor"])
	case "remove_notify_sub":
		_, err := kanban.RemoveNotifySub(pinned, conn, taskID, platform, chatID, &thread)
		return err
	case "rewind_notify_cursor":
		_, err := kanban.RewindNotifyCursor(pinned, conn, taskID, platform, chatID, &thread, extra["claimed_cursor"], extra["old_cursor"])
		return err
	case "record_notify_ping":
		return kanban.RecordNotifyPing(pinned, conn, taskID, platform, chatID, &thread, extra["event_id"])
	}
	return fmt.Errorf("module 'icebath_cli.kanban_db_notify' has no attribute '%s'", op)
}

// Moves the subscription cursor to cursor
func (r *GatewayRunner) kanbanAdvance(ctx context.Context, sub *jsonutil.Object, cursor int64, board string) error {
	return r.kanbanSubOp(ctx, board, "advance_notify_cursor", sub, map[string]int64{"new_cursor": cursor})
}

// Removes the subscription
func (r *GatewayRunner) kanbanUnsub(ctx context.Context, sub *jsonutil.Object, board string) error {
	return r.kanbanSubOp(ctx, board, "remove_notify_sub", sub, nil)
}

// Undoes a claimed notification cursor after a send failure
func (r *GatewayRunner) kanbanRewind(ctx context.Context, sub *jsonutil.Object, claimedCursor, oldCursor int64, board string) error {
	return r.kanbanSubOp(ctx, board, "rewind_notify_cursor", sub, map[string]int64{"claimed_cursor": claimedCursor, "old_cursor": oldCursor})
}

// Uploads the artifact files a completed task references, logging each upload failure
func (r *GatewayRunner) deliverKanbanArtifacts(ctx context.Context, adapter gateway.PlatformAdapter, chatID string, metadata map[string]any, eventPayload any, task *kanban.Task) error {
	var rawPaths, prosePaths []string
	if isPayloadMapping(eventPayload) {
		{
			if raw, ok := kanbanPayloadGet(eventPayload, "artifacts").([]any); ok {
				for _, item := range raw {
					if s, ok := item.(string); ok {
						rawPaths = append(rawPaths, s)
					}
				}
			}
			if summary, ok := kanbanPayloadGet(eventPayload, "summary").(string); ok && summary != "" {
				found, _ := gateway.ExtractLocalFiles(ctx, summary)
				prosePaths = append(prosePaths, found...)
			}
		}
	}
	if task != nil && task.Result != nil && *task.Result != "" {
		found, _ := gateway.ExtractLocalFiles(ctx, *task.Result)
		prosePaths = append(prosePaths, found...)
	}
	staged := map[string]bool{}
	for _, p := range rawPaths {
		staged[pathutil.Name(p)] = true
	}
	for _, p := range prosePaths {
		if !staged[pathutil.Name(p)] {
			rawPaths = append(rawPaths, p)
		}
	}
	var candidates []string
	seen := map[string]bool{}
	for _, path := range rawPaths {
		expanded := ""
		if path != "" {
			expanded = pathutil.ExpandUser(path)
		}
		if expanded == "" || seen[expanded] {
			continue
		}
		if info, err := os.Stat(expanded); err == nil && info.Mode().IsRegular() {
			seen[expanded] = true
			candidates = append(candidates, expanded)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	candidates, err := gateway.FilterLocalDeliveryPaths(ctx, candidates, "")
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}
	var imagePaths, otherPaths []string
	for _, p := range candidates {
		if kanbanImageExts[strings.ToLower(pathutil.Suffix(p))] {
			imagePaths = append(imagePaths, p)
		} else {
			otherPaths = append(otherPaths, p)
		}
	}
	if len(imagePaths) > 0 {
		batch := make([]gateway.ImageRef, 0, len(imagePaths))
		for _, p := range imagePaths {
			batch = append(batch, gateway.ImageRef{URL: "file://" + pyurl.Quote(p, "/"), Alt: ""})
		}
		if err := catchPanic(func() error {
			_, err := adapter.SendMultipleImages(ctx, chatID, batch, metadata, 0.0)
			return err
		}); err != nil {
			pkgLog().Warn(fmt.Sprintf("kanban notifier: image batch upload failed: %s", anyutil.String(err)))
		}
	}
	for _, path := range otherPaths {
		err := catchPanic(func() error {
			if kanbanVideoExts[strings.ToLower(pathutil.Suffix(path))] {
				_, err := adapter.SendVideo(ctx, chatID, path, "", "", metadata, nil)
				return err
			}
			_, err := adapter.SendDocument(ctx, chatID, path, "", "", "", metadata, nil)
			return err
		})
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("kanban notifier: artifact upload (%s) failed: %s", path, anyutil.String(err)))
		}
	}
	return nil
}

// Whether an event payload is a mapping
func isPayloadMapping(v any) bool {
	switch v.(type) {
	case *jsonutil.Object, map[string]any:
		return true
	}
	return false
}

// Boot state of the embedded dispatcher
type kanbanDispatcherBootState struct{}

// Resolves the setting and the singleton lock, nil when the dispatcher must not run
func (r *GatewayRunner) kanbanDispatcherBoot(ctx context.Context) (*kanbanDispatcherBootState, error) {
	if !settings.KanbanDispatchInGateway.Get(ctx) {
		pkgLog().Info("kanban dispatcher: turned off in settings")
		return nil, nil
	}

	r.kanbanDispatcherLockMu.Lock()
	r.kanbanDispatcherLockHandle = nil
	r.kanbanDispatcherLockMu.Unlock()
	lockPath := pathutil.Join(kanban.KanbanHome(), "kanban", ".dispatcher.lock")
	handle, lockState := acquireSingletonLock(lockPath)
	switch lockState {
	case singletonLockContended:
		pkgLog().Info(fmt.Sprintf("kanban dispatcher: another gateway already holds the dispatcher "+
			"lock (%s); this gateway will NOT dispatch.", lockPath))
		return nil, nil
	case singletonLockHeld:
		r.kanbanDispatcherLockMu.Lock()
		r.kanbanDispatcherLockHandle = handle
		r.kanbanDispatcherLockMu.Unlock()
		pkgLog().Info(fmt.Sprintf("kanban dispatcher: holding singleton dispatcher lock (%s)", lockPath))
	default:
		pkgLog().Warn(fmt.Sprintf("kanban dispatcher: advisory lock unavailable at %s; proceeding "+
			"on config control alone.", lockPath))
	}
	return &kanbanDispatcherBootState{}, nil
}

// Embedded kanban dispatcher ticking every dispatch interval while the gateway runs
func (r *GatewayRunner) kanbanDispatcherWatcher(ctx context.Context) error {
	boot, err := r.kanbanDispatcherBoot(ctx)
	if err != nil || boot == nil {
		return err
	}
	dispatch := resolveDispatcherSettings(ctx)
	interval := dispatch.interval

	if err := asyncioSleep(ctx, 5); err != nil {
		return err
	}

	badTicks := 0
	lastWarnAt := int64(0)
	var results []boardDispatch
	dispatcher := newKanbanDispatcher(dispatch)

	pkgLog().Info(fmt.Sprintf("kanban dispatcher: embedded in gateway (interval=%.1fs)", interval))
	for r.running.Load() {
		pids, err := toThreadProcessService(ctx, func(context.Context) ([]int, error) {
			var pids []int
			err := catchPanic(func() error {
				pids = kanban.ReapWorkerZombies()
				return nil
			})
			return pids, err
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			pkgLog().Error("kanban dispatcher: zombie reaper failed", "exc_info", err)
		} else if len(pids) > 0 {
			items := make([]any, len(pids))
			for i, p := range pids {
				items[i] = int64(p)
			}
			pkgLog().Info(fmt.Sprintf("kanban dispatcher: reaped %d zombie worker(s), pids=%s", len(pids), anyutil.Quote(items)))
		}

		err = catchPanic(func() error {
			if !kanbanDispatchAllowed(ctx) {
				badTicks = 0
			} else {
				adEnabled, adPerTick := resolveAutoDecomposeSettings(ctx)
				if adEnabled {
					if _, err := toThreadProcessService(ctx, func(fresh context.Context) (int, error) {
						return dispatcher.autoDecomposeTick(fresh, adPerTick)
					}); err != nil {
						return err
					}
				}
				results, err = toThreadProcessService(ctx, dispatcher.tickOnce)
				if err != nil {
					return err
				}
				anySpawned := logSpawnResults(results)
				readyPending, err := toThreadProcessService(ctx, dispatcher.readyNonempty)
				if err != nil {
					return err
				}
				if readyPending && !anySpawned {
					badTicks++
				} else {
					badTicks = 0
				}
			}
			now := time.Now().Unix()
			if badTicks >= kanbanHealthWindow && now-lastWarnAt >= 300 {
				held := make([]*kanban.DispatchResult, 0, len(results))
				for _, bd := range results {
					held = append(held, bd.result)
				}
				reason := kanban.DescribeSuppression(held)
				tail := ""
				if reason != "" {
					tail = fmt.Sprintf(" Last tick held back: %s.", reason)
				}
				pkgLog().Warn(fmt.Sprintf("kanban dispatcher stuck: ready queue non-empty for "+
					"%d consecutive ticks but 0 workers spawned.%s Check "+
					"profile health (venv, PATH, credentials) and "+
					"`icebath kanban list --status ready`.", badTicks, tail))
				lastWarnAt = now
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				pkgLog().Debug("kanban dispatcher: cancelled")
				r.releaseKanbanDispatcherLock()
				return ctx.Err()
			}
			pkgLog().Error("kanban dispatcher: unexpected watcher error", "exc_info", err)
		}

		if err := r.sleepBetweenTicks(ctx, interval); err != nil {
			return err
		}
	}
	r.releaseKanbanDispatcherLock()
	return nil
}
