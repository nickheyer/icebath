package cron

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	cancelMarkerTTLSeconds = 24 * 3600
	defaultCancelReason    = "cancelled on request (icebath cron cancel)"
	lostOwnershipReason    = "cron fire ownership was lost"
)

// Directory of per execution cancel markers in the store home
func (s *Store) cancelsDir() string { return filepath.Join(s.cronDir, "cancels") }

// Marker file of one execution, an error for an id that is not a plain file name
func (s *Store) cancelMarkerPath(executionID string) (string, error) {
	id := pystr.Strip(executionID)
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return "", fmt.Errorf("Not a usable execution id: %s", pyrepr.StrRepr(id))
	}
	return filepath.Join(s.cancelsDir(), id+".cancel"), nil
}

// Removes markers older than the TTL, ignoring every filesystem error
func (s *Store) pruneStaleCancelMarkers(now *float64) {
	at := float64(time.Now().UnixNano()) / 1e9
	if now != nil {
		at = *now
	}
	cutoff := at - cancelMarkerTTLSeconds
	entries, err := os.ReadDir(s.cancelsDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		path := filepath.Join(s.cancelsDir(), e.Name())
		if info, err := os.Stat(path); err == nil && float64(info.ModTime().UnixNano())/1e9 < cutoff {
			os.Remove(path)
		}
	}
}

// Records a cancel for one execution, the first recorded reason winning over later requests
func (s *Store) requestCancel(executionID string, reason *string) (string, error) {
	path, err := s.cancelMarkerPath(executionID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		text := defaultCancelReason
		if reason != nil && *reason != "" {
			text = *reason
		}
		if err := publishFirst(path, []byte(pystr.Strip(text))); err != nil {
			return "", err
		}
	}
	s.pruneStaleCancelMarkers(nil)
	return path, nil
}

// Creates path holding body unless it exists, through a hard linked private temp or an exclusive create
func publishFirst(path string, body []byte) error {
	linked := func() bool {
		tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
		if err != nil {
			return false
		}
		defer os.Remove(tmp.Name())
		_, werr := tmp.Write(body)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			return false
		}
		if err := os.Link(tmp.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return false
		}
		return true
	}()
	if linked {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, werr := f.Write(body)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
}

// A cancel marker waits for the execution
func (s *Store) cancelRequested(executionID string) bool {
	path, err := s.cancelMarkerPath(executionID)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// Removes the execution's marker once its run has wound down
func (s *Store) clearCancel(executionID string) {
	if path, err := s.cancelMarkerPath(executionID); err == nil {
		os.Remove(path)
	}
}

// CancelSource is a cancellation flag a run polls, with the cause it reports when set
type CancelSource interface {
	IsSet() bool
	Set()
	Reason() string
}

// CombinedCancelEvent is set when any source is set and sets every source
type CombinedCancelEvent struct{ events []CancelSource }

// Combines the non nil sources
func NewCombinedCancelEvent(events ...CancelSource) *CombinedCancelEvent {
	c := &CombinedCancelEvent{}
	for _, e := range events {
		if e != nil {
			c.events = append(c.events, e)
		}
	}
	return c
}

// Any source is set
func (c *CombinedCancelEvent) IsSet() bool {
	for _, e := range c.events {
		if e.IsSet() {
			return true
		}
	}
	return false
}

// Sets every source
func (c *CombinedCancelEvent) Set() {
	for _, e := range c.events {
		e.Set()
	}
}

// Cause from the first set source, empty when none is set
func (c *CombinedCancelEvent) Reason() string {
	for _, e := range c.events {
		if e.IsSet() {
			return e.Reason()
		}
	}
	return ""
}

// CancelRequest views one execution's cancel marker, set only by the requesting process
type CancelRequest struct {
	s           *Store
	executionID string
	reason      *string
}

// View of the execution's marker in the store home
func (s *Store) newCancelRequest(executionID string) *CancelRequest {
	return &CancelRequest{s: s, executionID: executionID}
}

// Marker exists
func (r *CancelRequest) IsSet() bool { return r.s.cancelRequested(r.executionID) }

// Refuses a local set, the requesting process owns this source
func (r *CancelRequest) Set() {
	panic("a cancel request is set by the requesting process, not the run")
}

// Recorded reason of the marker, the default when it is empty or unreadable
func (r *CancelRequest) Reason() string {
	if r.reason == nil {
		reason := defaultCancelReason
		if path, err := r.s.cancelMarkerPath(r.executionID); err == nil {
			if data, err := os.ReadFile(path); err == nil {
				if recorded := pystr.Strip(strings.TrimPrefix(string(data), "\ufeff")); recorded != "" {
					reason = recorded
				}
			}
		}
		r.reason = &reason
	}
	return *r.reason
}

// Human facing cause for a cancelled run, the ownership wording when no source carries one
func cancelReason(event any) string {
	if src, ok := event.(interface{ Reason() string }); ok {
		if reason := src.Reason(); reason != "" {
			return reason
		}
	}
	return lostOwnershipReason
}

// Books an operator cancelled run as cancelled in the job and the ledger without a failure alert
func (s *Store) finishCancelledRun(ctx context.Context, job *Job, executionID string, fireOwner *string) error {
	reason := s.newCancelRequest(executionID).Reason()
	removed, err := s.selfRemovalDeliveryAllowed(ctx, nil, job.ID)
	if err != nil {
		return err
	}
	if !removed {
		status := "cancelled"
		o := MarkOptions{Error: strPtr("Cancelled: " + reason), Status: &status, ExpectedFireOwner: fireOwner,
			LadderRung: unreachableRetryIsRetryRun(s, job)}
		if _, err := s.markJobRun(ctx, job.ID, false, o); err != nil {
			return err
		}
	}
	_, err = openExecutionsLedger(s).Finish(ctx, executionID, false, &reason, nil, true)
	return err
}

// Closes an attempt whose fire claim vanished, cancelled when a marker waits, failed otherwise
func (s *Store) finishLostExecution(ctx context.Context, executionID, errText string) (map[string]any, error) {
	l := openExecutionsLedger(s)
	if s.cancelRequested(executionID) {
		reason := s.newCancelRequest(executionID).Reason()
		return l.Finish(ctx, executionID, false, &reason, nil, true)
	}
	return l.Finish(ctx, executionID, false, &errText, nil, false)
}

// Fence that knows whether an operator cancelled the run
type operatorCancelFence interface{ operatorCancelled() bool }

// Result of run, or false once an operator cancel stopped the run and was booked, teardown on every exit
func runJobUnlessCancelled[T any](ctx context.Context, s *Store, run func() (T, error), job *Job, teardown func(),
	fence operatorCancelFence, executionID string, fireOwner *string) (T, bool, error) {
	var zero T
	finished := false
	defer func() {
		if !finished {
			teardown()
		}
	}()
	result, err := run()
	finished = true
	if err != nil {
		teardown()
		if !fence.operatorCancelled() {
			return zero, false, err
		}
		return zero, false, s.finishCancelledRun(ctx, job, executionID, fireOwner)
	}
	if !fence.operatorCancelled() {
		return result, true, nil
	}
	teardown()
	return zero, false, s.finishCancelledRun(ctx, job, executionID, fireOwner)
}

// Requests cancellation of the job's live run, a no op answer when nothing is in flight
func cancelRun(s *Store, jobID, reason string) (*pyjson.Object, error) {
	out := pyjson.NewObject()
	out.Set("success", true)
	record, err := openExecutionsLedger(s).Cancellable(context.Background(), jobID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		out.Set("cancelled", false)
		out.Set("execution_id", nil)
		out.Set("detail", "No run of this job is in flight — nothing to cancel.")
		return out, nil
	}
	var why *string
	if reason != "" {
		why = &reason
	}
	if _, err := s.requestCancel(recText(record, "id"), why); err != nil {
		return nil, err
	}
	out.Set("cancelled", true)
	out.Set("execution_id", record["id"])
	out.Set("status", record["status"])
	out.Set("pid", record["pid"])
	out.Set("detail", "Cancel requested; the run stops at its next cancel check.")
	return out, nil
}

// Stops the job's live run when there is one, leaving its schedule untouched
func (s *Store) cancelJobRun(jobID, reason string) (*pyjson.Object, error) {
	return cancelRun(s, jobID, reason)
}
