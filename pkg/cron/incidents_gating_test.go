package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
)

// Gating job record of the incident cases, delivered to a real slack lane
func gatingJobRecord() map[string]any {
	return map[string]any{
		"id": "incident-gating-test", "name": "incident gating test", "prompt": "hello", "enabled": true,
		"state": "scheduled", "schedule": map[string]any{"kind": "interval", "minutes": 5, "display": "every 5m"},
		"deliver": "slack:alerts", "model": "test-model", "provider": nil, "provider_snapshot": "openrouter", "base_url": nil,
	}
}

// Agent double whose conversation raises the configured error
type raisingCronAgent struct {
	*fakeCronAgent
	message string
	called  *bool
}

func (a *raisingCronAgent) RunConversation(context.Context, string, string) (map[string]any, error) {
	*a.called = true
	return nil, errors.New(a.message)
}

// Store under a fresh home with the gating job saved, the owner bound and the provider resolving to openrouter
func incidentGatingStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStore(t)
	t.Setenv("ICEBATH_MODEL", "test-cron-default-model")
	resetDeliver(t)
	registerHomePlatforms(t, "slack")
	seedJobs(t, s, gatingJobRecord())
	resolved := func(context.Context, string, string, string, string, string) (*provider.Runtime, error) {
		return &provider.Runtime{APIKey: "test-key", BaseURL: "https://example.invalid/v1", Provider: "openrouter", APIMode: "chat_completions"}, nil
	}
	prevRun, prevPreflight := resolveRuntimeProviderSeam, preflightResolveRuntime
	resolveRuntimeProviderSeam, preflightResolveRuntime = resolved, resolved
	t.Cleanup(func() { resolveRuntimeProviderSeam, preflightResolveRuntime = prevRun, prevPreflight })
	installCronOwner(t)
	return s
}

// Runs one failing run_one_job tick and, when settling, drains the queue marking each failure ping alerted
func tickFailing(t *testing.T, s *Store, errText string, settle bool) {
	t.Helper()
	writePinConfig(t, s, "platforms:\n  slack:\n    enabled: true\n    token: xoxb-test\n")
	called := false
	installFakeAgent(t, func(context.Context, AgentOptions) (CronAgent, error) {
		return &raisingCronAgent{fakeCronAgent: &fakeCronAgent{}, message: errText, called: &called}, nil
	})
	raw, err := json.Marshal(gatingJobRecord())
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{}
	if err := json.Unmarshal(raw, job); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := runOneJob(ctx, s, job, runOneJobOptions{}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("the agent never ran")
	}
	if !settle {
		return
	}
	if _, err := openDeliveryQueue(s).Drain(ctx, func(queued *Job, _ string, forFailure bool) (*string, error) {
		if forFailure {
			s.markIncidentAlerted(queuedFailureIncidentID(queued))
		}
		return nil, nil
	}, 100); err != nil {
		t.Fatal(err)
	}
}

// Failure pings the scheduler handed to the durable delivery queue
func queuedDeliveries(t *testing.T, s *Store) int {
	t.Helper()
	q := openDeliveryQueue(s)
	if _, err := os.Stat(q.path); err != nil {
		return 0
	}
	n := 0
	if err := q.transaction(context.Background(), false, func(conn state.SQLiteConn) error {
		return conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM deliveries").Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func mustListIncidents(t *testing.T, inc *IncidentStore) []*Incident {
	t.Helper()
	rows, err := inc.ListIncidents(nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestRepeatFailureAlertsOnceThenRemindsAfterCooldown(t *testing.T) {
	s := incidentGatingStore(t)
	inc := openIncidents(s)
	tickFailing(t, s, "repeat boom", true)
	tickFailing(t, s, "repeat boom", true)
	if n := queuedDeliveries(t, s); n != 1 {
		t.Fatalf("an alerted signature must not re-ping on every run, deliveries = %d", n)
	}
	rows := mustListIncidents(t, inc)
	if len(rows) != 1 || rows[0].State != "alerted" || rows[0].AlertedAt == nil || *rows[0].AlertedAt == "" {
		t.Fatalf("rows = %+v", rows)
	}
	if stored := mustGet(t, s, "incident-gating-test"); deref(stored.LastStatus) != "error" {
		t.Fatalf("the withheld run is still recorded, last_status = %v", deref(stored.LastStatus))
	}
	stale := FormatISO(s.clk.now().Add(-7 * time.Hour))
	if err := inc.transaction(func(ctx context.Context, conn state.SQLiteConn) error {
		_, err := conn.ExecContext(ctx, "UPDATE cron_incidents SET alerted_at=?", stale)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tickFailing(t, s, "repeat boom", true)
	tickFailing(t, s, "repeat boom", true)
	if n := queuedDeliveries(t, s); n != 2 {
		t.Fatalf("one reminder after the cooldown, not one per run, deliveries = %d", n)
	}
	s.resolveIncidentsForRecoveredJob(mustGet(t, s, "incident-gating-test"))
	tickFailing(t, s, "repeat boom", true)
	if n := queuedDeliveries(t, s); n != 3 {
		t.Fatalf("deliveries = %d", n)
	}
	if n := incidentCount(t, inc, nil); n != 1 {
		t.Fatalf("incidents = %d", n)
	}
}

func TestAckSuppressesAlertUntilSignatureChanges(t *testing.T) {
	s := incidentGatingStore(t)
	inc := openIncidents(s)
	tickFailing(t, s, "boom signature A", false)
	if n := queuedDeliveries(t, s); n != 1 {
		t.Fatalf("deliveries = %d", n)
	}
	rows := mustListIncidents(t, inc)
	if len(rows) != 1 || rows[0].State != "detected" {
		t.Fatalf("rows = %+v", rows)
	}
	id := rows[0].ID
	if ok, err := inc.AckIncident(id); err != nil || !ok {
		t.Fatalf("ack = %v %v", ok, err)
	}
	tickFailing(t, s, "boom signature A", false)
	if n := queuedDeliveries(t, s); n != 1 {
		t.Fatalf("acked signature must not re-ping, deliveries = %d", n)
	}
	if got := mustIncident(t, inc, id); got.State != "closed" {
		t.Fatalf("state = %s", got.State)
	}
	tickFailing(t, s, "boom signature B", false)
	if n := queuedDeliveries(t, s); n != 2 {
		t.Fatalf("changed signature must re-alert, deliveries = %d", n)
	}
	if n := incidentCount(t, inc, nil); n != 2 {
		t.Fatalf("incidents = %d", n)
	}
}

func TestAckHoldsWhenOnlyAMeasuredDurationDiffers(t *testing.T) {
	s := incidentGatingStore(t)
	inc := openIncidents(s)
	stall := "Cron job 'x' idle for %ds (limit 600s) — last activity: waiting for tool"
	tickFailing(t, s, fmt.Sprintf(stall, 601), false)
	if n := queuedDeliveries(t, s); n != 1 {
		t.Fatalf("deliveries = %d", n)
	}
	rows := mustListIncidents(t, inc)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	first := rows[0].ID
	if ok, err := inc.AckIncident(first); err != nil || !ok {
		t.Fatalf("ack = %v %v", ok, err)
	}
	tickFailing(t, s, fmt.Sprintf(stall, 604), false)
	if n := queuedDeliveries(t, s); n != 1 {
		t.Fatalf("acked stall must not re-ping on a new idle reading, deliveries = %d", n)
	}
	rows = mustListIncidents(t, inc)
	if len(rows) != 1 || rows[0].ID != first {
		t.Fatalf("rows = %+v", rows)
	}
}
