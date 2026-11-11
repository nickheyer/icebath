package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/kanban"
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
	for slept < interval && r.running {
		if err := asyncioSleep(ctx, min(1.0, interval-slept)); err != nil {
			return err
		}
		slept += 1.0
	}
	return nil
}

// Polls kanban notify subscriptions and delivers terminal task events to their chats
func (r *GatewayRunner) kanbanNotifierWatcher(ctx context.Context, interval float64) error {
	kanbanCfg := map[string]any{}
	cfg, err := config.LoadContext(ctx, home.Dir(ctx), nil)
	if err == nil {
		kanbanCfg, err = kanbanSection(cfg)
	}
	if err != nil {
		runLog().Warn(fmt.Sprintf("kanban notifier: cannot load config (%s); continuing enabled", pyrepr.Str(err)))
		kanbanCfg = map[string]any{}
	}
	if enabled, ok := kanbanCfg["notify_in_gateway"]; ok && !pyrepr.Truthy(enabled) {
		runLog().Info("kanban notifier: disabled via config kanban.notify_in_gateway=false")
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
	for r.running {
		err := catchPanic(func() error {
			gcDue := pythread.Monotonic() >= gcNextAt
			retention := int64(30)
			if gcDue {
				gcNextAt = pythread.Monotonic() + kanbanGCIntervalSeconds
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
			runLog().Warn(fmt.Sprintf("kanban notifier tick failed: %s", pyrepr.Str(err)))
		}
		if err := r.sleepBetweenTicks(ctx, interval); err != nil {
			return err
		}
	}
	return nil
}

// Calls one kanban notify operation for a subscription on its board under pin first resolution
func (r *GatewayRunner) kanbanSubOp(ctx context.Context, board string, op string, sub *pyjson.Object, extra map[string]int64) error {
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
func (r *GatewayRunner) kanbanAdvance(ctx context.Context, sub *pyjson.Object, cursor int64, board string) error {
	return r.kanbanSubOp(ctx, board, "advance_notify_cursor", sub, map[string]int64{"new_cursor": cursor})
}

// Removes the subscription
func (r *GatewayRunner) kanbanUnsub(ctx context.Context, sub *pyjson.Object, board string) error {
	return r.kanbanSubOp(ctx, board, "remove_notify_sub", sub, nil)
}

// Undoes a claimed notification cursor after a send failure
func (r *GatewayRunner) kanbanRewind(ctx context.Context, sub *pyjson.Object, claimedCursor, oldCursor int64, board string) error {
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
		staged[pypath.Basename(p)] = true
	}
	for _, p := range prosePaths {
		if !staged[pypath.Basename(p)] {
			rawPaths = append(rawPaths, p)
		}
	}
	var candidates []string
	seen := map[string]bool{}
	for _, path := range rawPaths {
		expanded := ""
		if path != "" {
			expanded = pypath.Expanduser(path)
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
		if kanbanImageExts[strings.ToLower(pypath.Suffix(p))] {
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
			runLog().Warn(fmt.Sprintf("kanban notifier: image batch upload failed: %s", pyrepr.Str(err)))
		}
	}
	for _, path := range otherPaths {
		err := catchPanic(func() error {
			if kanbanVideoExts[strings.ToLower(pypath.Suffix(path))] {
				_, err := adapter.SendVideo(ctx, chatID, path, "", "", metadata, nil)
				return err
			}
			_, err := adapter.SendDocument(ctx, chatID, path, "", "", "", metadata, nil)
			return err
		})
		if err != nil {
			runLog().Warn(fmt.Sprintf("kanban notifier: artifact upload (%s) failed: %s", path, pyrepr.Str(err)))
		}
	}
	return nil
}

// Whether an event payload is a mapping
func isPayloadMapping(v any) bool {
	switch v.(type) {
	case *pyjson.Object, map[string]any:
		return true
	}
	return false
}

// Boot state of the embedded dispatcher, its live config loader and the kanban config read at boot
type kanbanDispatcherBootState struct {
	loadConfig func() (config.Tree, error)
	kanbanCfg  map[string]any
}

// Resolves config and the singleton lock, nil when the dispatcher must not run
func (r *GatewayRunner) kanbanDispatcherBoot(ctx context.Context) (*kanbanDispatcherBootState, error) {
	loadConfig := func() (config.Tree, error) { return config.LoadContext(ctx, home.Dir(ctx), nil) }
	envOverride := pystr.Lower(pystr.Strip(os.Getenv("ICEBATH_KANBAN_DISPATCH_IN_GATEWAY")))
	switch envOverride {
	case "0", "false", "no", "off":
		runLog().Info("kanban dispatcher: disabled via ICEBATH_KANBAN_DISPATCH_IN_GATEWAY env")
		return nil, nil
	}
	cfg, err := loadConfig()
	if err != nil {
		runLog().Warn(fmt.Sprintf("kanban dispatcher: cannot load config (%s); disabled", pyrepr.Str(err)))
		return nil, nil
	}
	kanbanCfg, err := kanbanSection(cfg)
	if err != nil {
		return nil, err
	}
	if enabled, ok := kanbanCfg["dispatch_in_gateway"]; ok && !pyrepr.Truthy(enabled) {
		runLog().Info("kanban dispatcher: disabled via config kanban.dispatch_in_gateway=false")
		return nil, nil
	}

	r.kanbanDispatcherLockMu.Lock()
	r.kanbanDispatcherLockHandle = nil
	r.kanbanDispatcherLockMu.Unlock()
	lockPath := pypath.PathJoin(kanban.KanbanHome(), "kanban", ".dispatcher.lock")
	handle, lockState := acquireSingletonLock(lockPath)
	switch lockState {
	case singletonLockContended:
		runLog().Info(fmt.Sprintf("kanban dispatcher: another gateway already holds the dispatcher "+
			"lock (%s); this gateway will NOT dispatch.", lockPath))
		return nil, nil
	case singletonLockHeld:
		r.kanbanDispatcherLockMu.Lock()
		r.kanbanDispatcherLockHandle = handle
		r.kanbanDispatcherLockMu.Unlock()
		runLog().Info(fmt.Sprintf("kanban dispatcher: holding singleton dispatcher lock (%s)", lockPath))
	default:
		runLog().Warn(fmt.Sprintf("kanban dispatcher: advisory lock unavailable at %s; proceeding "+
			"on config control alone.", lockPath))
	}
	return &kanbanDispatcherBootState{loadConfig: loadConfig, kanbanCfg: kanbanCfg}, nil
}

// Embedded kanban dispatcher ticking every dispatch interval while the gateway runs
func (r *GatewayRunner) kanbanDispatcherWatcher(ctx context.Context) error {
	boot, err := r.kanbanDispatcherBoot(ctx)
	if err != nil || boot == nil {
		return err
	}
	settings := resolveDispatcherSettings(boot.kanbanCfg)
	interval := settings.interval

	if err := asyncioSleep(ctx, 5); err != nil {
		return err
	}

	badTicks := 0
	lastWarnAt := int64(0)
	var results []boardDispatch
	dispatcher := newKanbanDispatcher(settings)

	runLog().Info(fmt.Sprintf("kanban dispatcher: embedded in gateway (interval=%.1fs)", interval))
	for r.running {
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
			runLog().Error("kanban dispatcher: zombie reaper failed", "exc_info", err)
		} else if len(pids) > 0 {
			items := make([]any, len(pids))
			for i, p := range pids {
				items[i] = int64(p)
			}
			runLog().Info(fmt.Sprintf("kanban dispatcher: reaped %d zombie worker(s), pids=%s", len(pids), pyrepr.Repr(items)))
		}

		err = catchPanic(func() error {
			if !kanbanDispatchAllowed(ctx) {
				badTicks = 0
			} else {
				adEnabled, adPerTick, err := resolveAutoDecomposeSettings(boot.loadConfig)
				if err != nil {
					return err
				}
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
				runLog().Warn(fmt.Sprintf("kanban dispatcher stuck: ready queue non-empty for "+
					"%d consecutive ticks but 0 workers spawned.%s Check "+
					"profile health (venv, PATH, credentials) and "+
					"`icebath kanban list --status ready`.", badTicks, tail))
				lastWarnAt = now
			}
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				runLog().Debug("kanban dispatcher: cancelled")
				r.releaseKanbanDispatcherLock()
				return ctx.Err()
			}
			runLog().Error("kanban dispatcher: unexpected watcher error", "exc_info", err)
		}

		if err := r.sleepBetweenTicks(ctx, interval); err != nil {
			return err
		}
	}
	r.releaseKanbanDispatcherLock()
	return nil
}
