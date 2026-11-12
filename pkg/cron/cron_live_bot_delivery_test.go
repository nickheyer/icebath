package cron

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Profile authority deliver door double, one admission per stable id and owner settled records
type fakeBotAuthority struct {
	mu      sync.Mutex
	records map[string]map[string]any
}

// Admits a delivery id once, answering exact retries from the committed record
func (a *fakeBotAuthority) deliver(_ context.Context, home string, _ map[string]any, message, deliveryID, _ string) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.records[deliveryID]
	if !ok {
		resolved, _ := filepath.Abs(home)
		record = map[string]any{"status": "queued", "delivery_id": deliveryID, "profile_home": resolved,
			"session_id": "local-bot", "message": message, "admission_id": "adm-" + deliveryID[:8],
			"reply": "", "error": "", "reason": ""}
		a.records[deliveryID] = record
	} else if record["message"] != message {
		return nil, errors.New("admission_conflict")
	}
	out := map[string]any{}
	for k, v := range record {
		out[k] = v
	}
	return out, nil
}

// Mailbox read of the committed record, nil when the id was never admitted
func (a *fakeBotAuthority) read(_ context.Context, _ string, deliveryID string) (map[string]any, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	record, ok := a.records[deliveryID]
	if !ok {
		return nil, nil
	}
	out := map[string]any{}
	for k, v := range record {
		out[k] = v
	}
	return out, nil
}

// First entry of the queued receipt map the job records
func firstQueuedEntry(t *testing.T, queued any) interface{ Get(string) (any, bool) } {
	t.Helper()
	obj, ok := queued.(*pyjson.Object)
	if !ok || obj.Len() == 0 {
		t.Fatalf("queued %#v", queued)
	}
	first, _ := obj.Get(obj.Keys[0])
	entry, ok := first.(interface{ Get(string) (any, bool) })
	if !ok {
		t.Fatalf("queued entry %#v", first)
	}
	return entry
}

func TestResultRecordsPendingUntilTerminalReceipt(t *testing.T) {
	s, _ := newTestStore(t)
	t.Setenv("ICEBATH_HOME", s.home)
	t.Setenv(externalWorkerEnv, "")
	useRealDelivery(t)
	authority := &fakeBotAuthority{records: map[string]map[string]any{}}
	savedFind, savedDeliver, savedRead := findCanonicalLiveOwner, deliverToLiveOwner, readDeliveryResult
	t.Cleanup(func() {
		findCanonicalLiveOwner, deliverToLiveOwner, readDeliveryResult = savedFind, savedDeliver, savedRead
	})
	findCanonicalLiveOwner = func(_ context.Context, home string) (map[string]any, error) {
		resolved, _ := filepath.Abs(home)
		return map[string]any{"profile_home": resolved, "session_id": "local-bot", "canonical": true,
			"lease_id": "authority-1", "live_session_id": "local-bot"}, nil
	}
	deliverToLiveOwner = authority.deliver
	readDeliveryResult = authority.read
	ctx := context.Background()
	job := &Job{ID: "digest", ExecutionID: strPtr("run"), Deliver: strPtr("bot-chat")}
	deliveryErr, err := s.deliverResult(ctx, job, "payload", nil, nil, false)
	if err != nil || deliveryErr != nil {
		t.Fatalf("deliver %v %v", deliveryErr, err)
	}
	queued := job.LastDeliveryQueued
	entry := firstQueuedEntry(t, queued)
	if status, _ := entry.Get("status"); status != "queued" {
		t.Fatalf("queued status %v", status)
	}
	if got := classifyDeliveryOutcome(deliveryErr, true, false, "bot-chat", false, true, queued, false); got != "queued" {
		t.Fatalf("outcome %s", got)
	}
	if len(authority.records) != 1 {
		t.Fatalf("admissions %v", authority.records)
	}
	var key string
	for k := range authority.records {
		key = k
	}
	if id, _ := entry.Get("delivery_id"); id != key {
		t.Fatalf("delivery id %v want %s", id, key)
	}
	authority.mu.Lock()
	authority.records[key]["status"], authority.records[key]["reply"] = "settled", "done"
	authority.mu.Unlock()
	if deliveryErr, err := s.deliverResult(ctx, job, "payload", nil, nil, false); err != nil || deliveryErr != nil {
		t.Fatalf("settled deliver %v %v", deliveryErr, err)
	}
	if job.LastDeliveryQueued != nil {
		t.Fatalf("settled receipt still queued %#v", job.LastDeliveryQueued)
	}
}
