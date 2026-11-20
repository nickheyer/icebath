package cron

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/state"
)

// Execution id and status of every durable delivery row
func deliveryQueueRows(t *testing.T, s *Store) [][2]string {
	t.Helper()
	var rows [][2]string
	if err := openDeliveryQueue(s).transaction(context.Background(), false, func(conn state.SQLiteConn) error {
		r, err := conn.QueryContext(context.Background(), "SELECT execution_id, status FROM deliveries")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id, status string
			if err := r.Scan(&id, &status); err != nil {
				return err
			}
			rows = append(rows, [2]string{id, status})
		}
		return r.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestRealRunLedgerAndIncidentMatchActualPresentation(t *testing.T) {
	for _, mode := range []string{"failure", "crash", "success"} {
		for _, suppress := range []bool{false, true} {
			for _, externalWorker := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s-%v-%v", mode, suppress, externalWorker), func(t *testing.T) {
					realRunPresentation(t, mode, suppress, externalWorker)
				})
			}
		}
	}
}

func realRunPresentation(t *testing.T, mode string, suppress, externalWorker bool) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	useRealDelivery(t)
	registerHomePlatforms(t, "telegram")
	writePinConfig(t, s, fmt.Sprintf("display: {suppress_warning_notifications: %v}\n", suppress))
	prevCfg, prevSend, prevRun, prevWait := loadGatewayConfig, sendToPlatform, runJobSeam, enqueueAndWait
	t.Cleanup(func() {
		loadGatewayConfig, sendToPlatform, runJobSeam, enqueueAndWait = prevCfg, prevSend, prevRun, prevWait
	})
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) {
		return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformTelegram: {Enabled: true}}}, nil
	}
	var mu sync.Mutex
	var sent []string
	sendToPlatform = func(_ context.Context, _ gateway.Platform, _ *gateway.PlatformConfig, _, text string, _ messagetools.SendOptions) (map[string]any, error) {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, text)
		return map[string]any{"success": true, "message_id": "receipt"}, nil
	}
	sentCount := func() int { mu.Lock(); defer mu.Unlock(); return len(sent) }
	runJobSeam = func(context.Context, *Store, *Job, runJobOptions) (jobRunOutcome, error) {
		if mode == "crash" {
			return jobRunOutcome{}, errors.New("isolated provider failure")
		}
		out := jobRunOutcome{Success: mode == "success", Output: "retained raw output", FinalResponse: "required result"}
		if mode != "success" {
			out.Error = strPtr("isolated provider failure")
		}
		return out, nil
	}
	job := mustCreate(t, s, CreateJobOptions{Prompt: prompt("fixture only"), Schedule: "every 1h", Deliver: prompt("telegram:fixture")})
	if externalWorker {
		execution, err := openExecutionsLedger(s).Create(ctx, job.ID, "fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		id := execution["id"].(string)
		job.ExecutionID = &id
		t.Setenv(externalWorkerEnv, id)
		enqueueAndWait = func(q *DeliveryQueue, ctx context.Context, executionID string, queued *Job, content string, forFailure bool, timeout *float64) (*string, error) {
			if _, err := q.Enqueue(ctx, executionID, queued, content, forFailure); err != nil {
				return nil, err
			}
			if n, err := s.drainDeliveryQueue(ctx, map[string]any{}, nil); err != nil || n != 1 {
				t.Fatalf("drain before wait = %d, %v", n, err)
			}
			return prevWait(q, ctx, executionID, queued, content, forFailure, timeout)
		}
	}
	if _, err := runOneJob(ctx, s, job, runOneJobOptions{}); err != nil {
		t.Fatal(err)
	}
	latest := func() map[string]any {
		row, err := openExecutionsLedger(s).Latest(ctx, job.ID)
		if err != nil || row == nil {
			t.Fatalf("latest = %v, %v", row, err)
		}
		return row
	}
	row := latest()
	expectedSuppression := suppress && mode != "success"
	if mode != "crash" {
		if n := sentCount(); n != 0 {
			t.Fatalf("sent before drain = %d", n)
		}
		if row["delivery_outcome"] != "queued" {
			t.Fatalf("delivery_outcome = %v", row["delivery_outcome"])
		}
		if n, err := s.drainDeliveryQueue(ctx, map[string]any{}, nil); err != nil || n != 1 {
			t.Fatalf("drain = %d, %v", n, err)
		}
		queued := deliveryQueueRows(t, s)
		want := "delivered"
		if expectedSuppression {
			want = "suppressed"
		}
		if len(queued) != 1 || queued[0][1] != want {
			t.Fatalf("queued = %v, want %s", queued, want)
		}
		row = latest()
	}
	wantSent, wantOutcome := 1, "delivered"
	if expectedSuppression {
		wantSent, wantOutcome = 0, "suppressed"
	}
	if n := sentCount(); n != wantSent {
		t.Fatalf("sent = %d, want %d", n, wantSent)
	}
	if row["delivery_outcome"] != wantOutcome {
		t.Fatalf("delivery_outcome = %v, want %s", row["delivery_outcome"], wantOutcome)
	}
	saved := mustGet(t, s, job.ID)
	wantStatus := "error"
	if mode == "success" {
		wantStatus = "ok"
	}
	if deref(saved.LastStatus) != wantStatus {
		t.Fatalf("last_status = %v", deref(saved.LastStatus))
	}
	if mode != "success" {
		if !strings.Contains(deref(saved.LastError), "isolated provider failure") {
			t.Fatalf("last_error = %v", deref(saved.LastError))
		}
		rows, err := openIncidents(s).ListIncidents(nil)
		if err != nil {
			t.Fatal(err)
		}
		var incident *Incident
		for _, r := range rows {
			if r.JobID == job.ID {
				incident = r
				break
			}
		}
		wantState := "alerted"
		if suppress {
			wantState = "detected"
		}
		if incident == nil || incident.State != wantState {
			t.Fatalf("incident = %+v, want %s", incident, wantState)
		}
	}
	if mode != "crash" {
		outputs, _ := filepath.Glob(filepath.Join(s.Home(), "cron", "output", job.ID, "*.md"))
		if len(outputs) == 0 {
			t.Fatal("no retained output")
		}
		text, err := os.ReadFile(outputs[0])
		if err != nil || !strings.Contains(string(text), "retained raw output") {
			t.Fatalf("output = %q, %v", text, err)
		}
	}
}
