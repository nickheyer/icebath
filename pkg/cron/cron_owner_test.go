package cron

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Replaces only the canonical transport with the production owner execution, as the conftest cron_owner fixture does
func installCronOwner(t *testing.T) {
	t.Helper()
	prev := runCanonicalJobSeam
	runCanonicalJobSeam = func(s *Store, ctx context.Context, job *Job, extra *string, _ CancelSource, executionID *string) (jobRunOutcome, error) {
		db, err := state.Acquire(ctx, home.StatePath(s.home))
		if err != nil {
			return jobRunOutcome{}, err
		}
		defer state.ReleaseOrClose(db)
		identity := uuid4Hex()
		ref := gateway.SessionRef{ProfileID: "test-profile", SessionID: "cron-" + identity}
		if err := db.CreateSession(state.NewSession{ID: ref.SessionID, Source: "cron"}); err != nil {
			return jobRunOutcome{}, err
		}
		epoch, err := db.BeginRuntimeEpoch(ctx, "test")
		if err != nil {
			return jobRunOutcome{}, err
		}
		authority := gateway.NewSessionAuthority(ownerTestRunner{}, "test-profile", "test", db, epoch)
		owner := "cron-owner"
		authority.Sessions[ref.SessionID] = gateway.NewLiveSession(&gateway.SessionSource{UserID: &owner}, "route")
		requestID := identity
		if executionID != nil && *executionID != "" {
			requestID = *executionID
		}
		text := ""
		var extraValue any
		if extra != nil {
			text, extraValue = *extra, *extra
		}
		row := pyjson.ObjectOf("admission_id", identity, "request_id", requestID, "principal_id", owner,
			"payload", pyjson.ObjectOf("text", text))
		raw, err := json.Marshal(job)
		if err != nil {
			return jobRunOutcome{}, err
		}
		cronJob, err := pyjson.Loads(string(raw))
		if err != nil {
			return jobRunOutcome{}, err
		}
		request, err := pyjson.Dumps(pyjson.ObjectOf("cron_job", cronJob, "extra_prompt", extraValue, "request_id", requestID))
		if err != nil {
			return jobRunOutcome{}, err
		}
		previous, hadPrevious := gateway.CurrentCronExecution(ctx)
		_, execErr := gateway.CronExecute(ctx, authority, ref, row, &gateway.LocalSessionPolicy{RequestJSON: request})
		saved, pending := authority.PendingResults[identity]
		if execErr != nil && !pending {
			return jobRunOutcome{}, execErr
		}
		if current, had := gateway.CurrentCronExecution(ctx); had != hadPrevious || current.ExecutionID != previous.ExecutionID {
			t.Error("the owner execution leaked its cron identity past execute")
		}
		if n := authority.CronCancellations(); n != 0 {
			t.Errorf("owner execution left %d cron cancellations", n)
		}
		resultValue, _ := saved.Get("result")
		result, _ := resultValue.(*pyjson.Object)
		flags, _ := result.Get("cron_job_flags")
		if err := applyJobFlags(job, flags); err != nil {
			return jobRunOutcome{}, err
		}
		cronResult, _ := result.Get("cron_result")
		return outcomeFromResult(cronResult)
	}
	t.Cleanup(func() { runCanonicalJobSeam = prev })
}
