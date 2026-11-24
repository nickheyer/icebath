//go:build !icebath_core && !icebath_noextras

package gatewayrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/state"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	pathutil "github.com/nickheyer/icebath/pkg/utils/path"
	timeutil "github.com/nickheyer/icebath/pkg/utils/time"
)

// Kanban calls of the dispatcher tick, the kanban package functions and the live board listing
var (
	dispatcherBoardSlugs         = kanbanBoardSlugs
	kanbanDecomposeListTriageIDs = kanban.DecomposeListTriageIDs
	kanbanDecomposeTask          = kanban.DecomposeTask
)

var corruptDBMarkers = []string{"file is not a database", "database disk image is malformed"}

// Kanban dispatch settings read once at boot
type dispatcherSettings struct {
	interval                float64
	maxSpawn                *int
	maxInProgress           *int
	failureLimit            int64
	staleTimeoutSeconds     int64
	reconcileOrphans        bool
	defaultAssignee         *string
	maxInProgressPerProfile *int
}

// Reads and logs the dispatcher settings of the profile in ctx
func resolveDispatcherSettings(ctx context.Context) dispatcherSettings {
	maxSpawn := positiveSetting(ctx, settings.KanbanMaxSpawn)
	maxInProgress := positiveSetting(ctx, settings.KanbanMaxInProgress)
	effectiveMaxInProgress := kanban.ResolveMaxInProgress(maxInProgress)
	if maxInProgress == nil && effectiveMaxInProgress != nil {
		pkgLog().Info(fmt.Sprintf("kanban dispatcher: no in-progress limit set; using the memory-derived limit %d", *effectiveMaxInProgress))
	}
	var defaultAssignee *string
	if assignee := strings.TrimSpace(settings.KanbanDefaultAssignee.Get(ctx)); assignee != "" {
		defaultAssignee = &assignee
		pkgLog().Info(fmt.Sprintf("kanban dispatcher: default_assignee=%s (unassigned ready tasks "+
			"will route to this profile)", strconv.Quote(assignee)))
	}
	return dispatcherSettings{
		interval:                max(settings.KanbanDispatchIntervalSeconds.Get(ctx).Seconds(), 1.0),
		maxSpawn:                maxSpawn,
		maxInProgress:           effectiveMaxInProgress,
		failureLimit:            int64(settings.KanbanFailureLimit.Get(ctx)),
		staleTimeoutSeconds:     int64(max(0, settings.KanbanDispatchStaleTimeoutSeconds.Get(ctx).Seconds())),
		reconcileOrphans:        settings.KanbanReconcileOrphans.Get(ctx),
		defaultAssignee:         defaultAssignee,
		maxInProgressPerProfile: positiveSetting(ctx, settings.KanbanMaxInProgressPerProfile),
	}
}

// Database fingerprint of a board, its resolved path with stat fields when the file exists
type boardDBFingerprint struct {
	resolved string
	hasStat  bool
	mtimeNs  int64
	size     int64
}

// Quarantine record of a board whose database looked corrupt
type corruptBoardEntry struct {
	fingerprint boardDBFingerprint
	at          float64
}

// Per tick board work of the embedded dispatcher
type kanbanDispatcher struct {
	settings               dispatcherSettings
	disabledCorruptBoards  map[string]corruptBoardEntry
	corruptBoardRetryAfter float64
}

// Dispatcher over the resolved settings with an empty quarantine
func newKanbanDispatcher(settings dispatcherSettings) *kanbanDispatcher {
	return &kanbanDispatcher{settings: settings, disabledCorruptBoards: map[string]corruptBoardEntry{}, corruptBoardRetryAfter: 300}
}

// Resolved database path of a board with its mtime and size when the file exists
func (d *kanbanDispatcher) boardDBFingerprint(ctx context.Context, slug string) (boardDBFingerprint, error) {
	path, err := kanban.KanbanDBPath(kanban.PinFirstBoardResolution(ctx), &slug)
	if err != nil {
		return boardDBFingerprint{}, err
	}
	resolved, err := pathutil.Resolve(pathutil.ExpandUser(path))
	if err != nil {
		resolved = path
	}
	info, err := os.Stat(path)
	if err != nil {
		return boardDBFingerprint{resolved: resolved}, nil
	}
	return boardDBFingerprint{resolved: resolved, hasStat: true, mtimeNs: info.ModTime().UnixNano(), size: info.Size()}, nil
}

// Whether a failure means the board database is corrupt
func (d *kanbanDispatcher) isCorruptBoardDBError(err error) bool {
	var corrupt *kanban.KanbanDBCorruptError
	if errors.As(err, &corrupt) {
		return true
	}
	class, ok := state.SQLiteClassOf(err)
	if !ok || class == state.SQLiteInterfaceError {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, m := range corruptDBMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// False while slug stays quarantined, else lifts and logs the quarantine
func (d *kanbanDispatcher) quarantineLifted(slug string, fingerprint boardDBFingerprint) bool {
	entry, ok := d.disabledCorruptBoards[slug]
	if !ok {
		return true
	}
	age := timeutil.Monotonic() - entry.at
	if entry.fingerprint == fingerprint && age < d.corruptBoardRetryAfter {
		return false
	}
	if entry.fingerprint == fingerprint {
		pkgLog().Info(fmt.Sprintf("kanban dispatcher: board %s database fingerprint unchanged "+
			"after %.0fs quarantine; retrying dispatch", slug, age))
	} else {
		pkgLog().Info(fmt.Sprintf("kanban dispatcher: board %s database changed; retrying dispatch", slug))
	}
	delete(d.disabledCorruptBoards, slug)
	return true
}

// One dispatch pass on a board over its own connection, nil when quarantined or failed
func (d *kanbanDispatcher) tickOnceForBoard(ctx context.Context, slug string) (*kanban.DispatchResult, error) {
	fingerprint, err := d.boardDBFingerprint(ctx, slug)
	if err != nil {
		return nil, err
	}
	if !d.quarantineLifted(slug, fingerprint) {
		return nil, nil
	}
	pinned := kanban.PinFirstBoardResolution(ctx)
	var conn state.SQLiteConn
	defer func() {
		if conn != nil {
			_ = conn.Close()
		}
	}()
	result, err := func() (*kanban.DispatchResult, error) {
		var err error
		conn, err = kanbanConnect(pinned, "", &slug)
		if err != nil {
			conn = nil
			return nil, err
		}
		s := d.settings
		return kanban.DispatchOnce(pinned, conn, kanban.DispatchOptions{
			Board: &slug, MaxSpawn: s.maxSpawn, MaxInProgress: s.maxInProgress, FailureLimit: s.failureLimit,
			StaleTimeoutSeconds: s.staleTimeoutSeconds, ReconcileOrphans: s.reconcileOrphans,
			DefaultAssignee: s.defaultAssignee, MaxInProgressPerProfile: s.maxInProgressPerProfile,
		})
	}()
	if err == nil {
		return result, nil
	}
	if d.isCorruptBoardDBError(err) {
		d.disabledCorruptBoards[slug] = corruptBoardEntry{fingerprint: fingerprint, at: timeutil.Monotonic()}
		pkgLog().Error(fmt.Sprintf("kanban dispatcher: board %s database %s is not a valid "+
			"SQLite database; pausing dispatch for this board until "+
			"the file changes, the gateway restarts, or the "+
			"quarantine timer expires. Move or restore the file, "+
			"then run `icebath kanban init` if you need a fresh board.", slug, fingerprint.resolved))
		return nil, nil
	}
	pkgLog().Error(fmt.Sprintf("kanban dispatcher: tick failed on board %s", slug), "exc_info", err)
	return nil, nil
}

// Slug and dispatch result of one board pass
type boardDispatch struct {
	slug   string
	result *kanban.DispatchResult
}

// One dispatch pass per live board
func (d *kanbanDispatcher) tickOnce(ctx context.Context) ([]boardDispatch, error) {
	slugs, err := dispatcherBoardSlugs()
	if err != nil {
		return nil, err
	}
	out := make([]boardDispatch, 0, len(slugs))
	for _, slug := range slugs {
		res, err := d.tickOnceForBoard(ctx, slug)
		if err != nil {
			return nil, err
		}
		out = append(out, boardDispatch{slug, res})
	}
	return out, nil
}

// Whether any board holds a spawnable ready task, or a spawnable review task while review dispatch is on
func (d *kanbanDispatcher) readyNonempty(ctx context.Context) (bool, error) {
	reviewProbe := kanban.ReviewDispatchEnabled(ctx)
	pinned := kanban.PinFirstBoardResolution(ctx)
	slugs, err := dispatcherBoardSlugs()
	if err != nil {
		return false, err
	}
	for _, slug := range slugs {
		found := func() bool {
			conn, err := kanbanConnect(pinned, "", &slug)
			if err != nil {
				return false
			}
			defer conn.Close()
			ready, err := kanban.HasSpawnableReady(pinned, conn)
			if err != nil {
				return false
			}
			if ready {
				return true
			}
			if !reviewProbe {
				return false
			}
			review, err := kanban.HasSpawnableReview(pinned, conn)
			return err == nil && review
		}()
		if found {
			return true, nil
		}
	}
	return false, nil
}

// Decomposes up to perTick triage tasks across every board and returns how many succeeded
func (d *kanbanDispatcher) autoDecomposeTick(ctx context.Context, perTick int) (int, error) {
	attempted, successes := 0, 0
	scoped := kanban.PinFirstBoardResolution(defaultProfileSecretScope(ctx))
	slugs, err := dispatcherBoardSlugs()
	if err != nil {
		return 0, err
	}
	for _, slug := range slugs {
		if attempted >= perTick {
			break
		}
		boardCtx := kanban.ScopedCurrentBoard(scoped, slug)
		triageIDs, err := kanbanDecomposeListTriageIDs(boardCtx, nil)
		if err != nil {
			pkgLog().Debug(fmt.Sprintf("kanban auto-decompose: list_triage_ids failed on board %s (%s)", slug, anyutil.String(err)))
			triageIDs = nil
		}
		for _, tid := range triageIDs {
			if attempted >= perTick {
				break
			}
			attempted++
			successes += decomposeOne(boardCtx, slug, tid)
		}
	}
	return successes, nil
}

// Decomposes one triage task, one on success and zero otherwise
func decomposeOne(ctx context.Context, slug, tid string) (n int) {
	author := "auto-decomposer"
	outcome, err := func() (out *kanban.DecomposeOutcome, err error) {
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("%v", rec)
			}
		}()
		return kanbanDecomposeTask(ctx, tid, &author)
	}()
	if err != nil {
		pkgLog().Error(fmt.Sprintf("kanban auto-decompose: decompose_task crashed on %s", tid), "exc_info", err)
		return 0
	}
	if !outcome.OK {
		pkgLog().Debug(fmt.Sprintf("kanban auto-decompose [%s]: %s skipped: %s", slug, tid, outcome.Reason))
		return 0
	}
	if outcome.Fanout && len(outcome.ChildIDs) > 0 {
		pkgLog().Info(fmt.Sprintf("kanban auto-decompose [%s]: %s → %d children", slug, tid, len(outcome.ChildIDs)))
	} else {
		pkgLog().Info(fmt.Sprintf("kanban auto-decompose [%s]: %s → single task (no fanout)", slug, tid))
	}
	return 1
}

// Context carrying the launch profile secret scope while multiplexing is on, ctx unchanged otherwise
func defaultProfileSecretScope(ctx context.Context) context.Context {
	if !provider.IsMultiplexActive(ctx) {
		return ctx
	}
	h := home.Dir(ctx)
	return provider.WithSecretScope(ctx, provider.OwnerSecretScope(ctx, h), h)
}

// Logs per board spawn summaries and reports whether any board spawned
func logSpawnResults(results []boardDispatch) bool {
	anySpawned := false
	for _, bd := range results {
		res := bd.result
		if res != nil && len(res.Spawned) > 0 {
			anySpawned = true
			pkgLog().Info(fmt.Sprintf("kanban dispatcher [%s]: spawned=%d reclaimed=%d "+
				"crashed=%d timed_out=%d promoted=%d auto_blocked=%d",
				bd.slug, len(res.Spawned), res.Reclaimed, len(res.Crashed), len(res.TimedOut), res.Promoted, len(res.AutoBlocked)))
		}
	}
	return anySpawned
}
