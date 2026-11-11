package dashboard

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

const cronOwnershipClaimGraceSeconds = 5.0

// Scheduler ownership of a cron run session row, nil when the row is not a cron run session
func cronRunSchedulerOwned(ctx context.Context, session *pyjson.Object, profile string) (*bool, error) {
	if source, _ := session.Get("source"); source != "cron" {
		return nil, nil
	}
	jobID := ""
	if v, _ := session.Get("cron_job_id"); pyrepr.Truthy(v) {
		jobID, _ = v.(string)
	} else if id, ok := session.Get("id"); ok {
		if sid, isStr := id.(string); isStr {
			jobID, _ = state.LegacyCronRunJob(sid)
		}
	}
	if jobID == "" {
		return nil, nil
	}
	owned := false
	if ended, _ := session.Get("ended_at"); ended != nil {
		return &owned, nil
	}
	scoped := cronOwnerHomeScope(ctx, profile)
	owned = cronRunOwnedBy(session, cronLiveInflightExecution(scoped, jobID))
	return &owned, nil
}

// Context bound to the owner profile home, unchanged for the serving profile or an unresolvable one
func cronOwnerHomeScope(ctx context.Context, profile string) context.Context {
	if profile == "" {
		return ctx
	}
	_, profileHome, err := cronProfileHome(ctx, profile)
	if err != nil {
		return ctx
	}
	if resolvedOrSelf(profileHome) == resolvedOrSelf(home.Dir(ctx)) {
		return ctx
	}
	return home.WithOverride(ctx, profileHome)
}

// Resolved path, the path itself when resolution fails
func resolvedOrSelf(p string) string {
	if resolved, err := pypath.Resolve(p); err == nil {
		return resolved
	}
	return p
}

// Claimed or running ledger attempt of the job under a live owner, nil on any failure
func cronLiveInflightExecution(ctx context.Context, jobID string) map[string]any {
	record, err := cron.LiveInflightExecutionIn(ctx, home.Dir(ctx), jobID)
	if err != nil {
		return nil
	}
	return record
}

// Whether the scheduler still owns a never closed run session through the live in flight attempt
func cronRunOwnedBy(session *pyjson.Object, inflight map[string]any) bool {
	if len(inflight) == 0 {
		return false
	}
	if ended, _ := session.Get("ended_at"); ended != nil {
		return false
	}
	if execID, _ := session.Get("cron_execution_id"); pyrepr.Truthy(execID) {
		return execID == inflight["id"]
	}
	claimedAt, claimed := cronISOToEpoch(inflight["claimed_at"])
	raw, _ := session.Get("started_at")
	if !pyrepr.Truthy(raw) {
		raw = 0
	}
	startedAt, err := pyint.Float(raw)
	if err != nil {
		return false
	}
	return claimed && startedAt >= claimedAt-cronOwnershipClaimGraceSeconds
}

// Epoch seconds of an ISO timestamp, naive values read in local time, false for anything unparsable
func cronISOToEpoch(text any) (float64, bool) {
	s, ok := text.(string)
	if !ok || pystr.Strip(s) == "" {
		return 0, false
	}
	t, aware, err := pydatetime.ParseISOFormat(pystr.Strip(s))
	if err != nil {
		return 0, false
	}
	if !aware {
		t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.Local)
	}
	return float64(t.UnixNano()) / 1e9, true
}
