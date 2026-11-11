package gatewayrun

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

// Runner exposing what the idle predicate reads, its launch ledger in a temp home and activity ten seconds old
func idleExitRunner(t *testing.T) (*GatewayRunner, string, *state.SessionDB) {
	t.Helper()
	root := t.TempDir()
	h := filepath.Join(root, "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	db, err := state.NewSessionDB(context.Background(), filepath.Join(h, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	registry := gateway.NewSessionAuthorities(h, false)
	if err := registry.Add(h, &gateway.SessionAuthority{ProfileID: h, DB: db}, nil); err != nil {
		t.Fatal(err)
	}
	tickets, err := gateway.NewTicketStore("i", map[string]bool{h: true})
	if err != nil {
		t.Fatal(err)
	}
	r := allocGatewayRunner()
	r.running, r.draining = true, false
	r.sessionAuthorities = registry
	r.adapters = adapterDict(gateway.PlatformLocal, newRestartTestAdapter())
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	r.profileFailedPlatforms = map[string]map[gateway.Platform]*gatewayTask{}
	r.failedPlatforms = pydict.New[gateway.Platform, map[string]any]()
	r.config = gateway.DefaultGatewayConfig(context.Background())
	r.config.Platforms = map[gateway.Platform]*gateway.PlatformConfig{}
	r.sessionTicketStore = tickets
	r.sessionRuntimeDescriptor = map[string]any{"state": "ready", "capabilities": []any{"x"}}
	noteClientActivity(r)
	r.idleExitLastActivity.Store(math.Float64bits(math.Float64frombits(r.idleExitLastActivity.Load()) - 10))
	return r, h, db
}

// Admits one queued input for session s, returning the epoch and the admission id
func queueAdmission(t *testing.T, db *state.SessionDB, text string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	if err := db.CreateSession(state.NewSession{ID: "s", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	epoch, err := db.BeginRuntimeEpoch(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.AdmitSessionInput(ctx, state.SessionInput{Epoch: epoch, PrincipalID: "p", SessionID: "s", RequestID: "r", Payload: pyjson.ObjectOf("text", text)})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := row.Get("admission_id")
	return epoch, id.(string)
}

func TestQueuedWorkClientsAdaptersAndCronEachKeepItUp(t *testing.T) {
	r, h, db := idleExitRunner(t)
	ctx := context.Background()
	if got := busyReason(ctx, r, 0.2, true); got != "" {
		t.Fatalf("idle runner busy: %s", got)
	}
	epoch, admission := queueAdmission(t, db, "queued while idle")
	if got := busyReason(ctx, r, 0.2, true); !strings.Contains(got, "admissions pending") {
		t.Fatalf("queued admission: %q", got)
	}
	if _, err := db.CancelSessionInput(ctx, epoch, admission); err != nil {
		t.Fatal(err)
	}
	release := attachedClient(r)
	if got := busyReason(ctx, r, 0.2, true); !strings.Contains(got, "client(s) attached") {
		t.Fatalf("attached client: %q", got)
	}
	release()
	if got := busyReason(ctx, r, 60, true); !strings.Contains(got, "client activity") {
		t.Fatalf("recent activity: %q", got)
	}
	r.idleExitLastActivity.Store(math.Float64bits(pythread.Monotonic() - 120))
	if _, err := r.sessionTicketStore.Mint(h, "u", "interactive", "", false); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2, true); got != "client attaching" {
		t.Fatalf("outstanding ticket: %q", got)
	}
	r.sessionTicketStore.Revoke()
	r.config.Platforms[gateway.PlatformTelegram] = &gateway.PlatformConfig{Enabled: true}
	if got := busyReason(ctx, r, 0.2, true); !strings.Contains(got, "messaging") {
		t.Fatalf("configured adapter: %q", got)
	}
	clear(r.config.Platforms)
	prompt, jobName := "ping", "j"
	if _, err := cron.CreateJob(ctx, h, cron.CreateJobOptions{Prompt: &prompt, Schedule: "every 1h", Name: &jobName}); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2, true); !strings.Contains(got, "cron jobs scheduled") {
		t.Fatalf("scheduled cron job: %q", got)
	}
	if err := os.WriteFile(filepath.Join(h, "cron", "jobs.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2, true); !strings.Contains(got, "idle probe failed") {
		t.Fatalf("unreadable cron store: %q", got)
	}
}

func TestAProfileStillStartingLeavesTheServedLedgersReadable(t *testing.T) {
	r, _, db := idleExitRunner(t)
	starting := filepath.Join(t.TempDir(), "starting")
	if err := os.MkdirAll(starting, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "starting"
	if err := r.sessionAuthorities.Add(starting, nil, &name); err != nil {
		t.Fatal(err)
	}
	queueAdmission(t, db, "queued while another profile starts")
	if got := busyReason(context.Background(), r, 0.2, true); !strings.Contains(got, "admissions pending") {
		t.Fatalf("busy %q", got)
	}
}

func TestAProfileStillStartingKeepsAnOtherwiseIdleGatewayUp(t *testing.T) {
	r, _, _ := idleExitRunner(t)
	ctx := context.Background()
	registry := r.sessionAuthorities
	starting := filepath.Join(t.TempDir(), "starting")
	if err := os.MkdirAll(starting, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "starting"
	if got := busyReason(ctx, r, 0.2, true); got != "" {
		t.Fatalf("idle runner busy: %s", got)
	}
	if err := registry.Add(starting, nil, &name); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2, true); !strings.Contains(got, "profile starting") {
		t.Fatalf("starting profile: %q", got)
	}
	if _, err := registry.Remove(starting); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2, true); got != "" {
		t.Fatalf("failed start still holds: %s", got)
	}
	if err := registry.Add(starting, nil, &name); err != nil {
		t.Fatal(err)
	}
	startingDB, err := state.NewSessionDB(ctx, filepath.Join(starting, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer startingDB.Close()
	if err := registry.Replace(starting, &gateway.SessionAuthority{ProfileID: starting, DB: startingDB}); err != nil {
		t.Fatal(err)
	}
	if got := busyReason(ctx, r, 0.2, true); got != "" {
		t.Fatalf("filled start still holds: %s", got)
	}
}

func TestSecondaryLocalSessionAdapterDoesNotCountAsMessaging(t *testing.T) {
	r, _, _ := idleExitRunner(t)
	ctx := context.Background()
	secondary := filepath.Join(t.TempDir(), "secondary")
	if err := os.MkdirAll(secondary, 0o755); err != nil {
		t.Fatal(err)
	}
	secondaryDB, err := state.NewSessionDB(ctx, filepath.Join(secondary, "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer secondaryDB.Close()
	name := "secondary"
	if err := r.sessionAuthorities.Add(secondary, &gateway.SessionAuthority{ProfileID: secondary, DB: secondaryDB}, &name); err != nil {
		t.Fatal(err)
	}
	r.profileAdapters.Set("secondary", adapterDict(gateway.PlatformLocal, newRestartTestAdapter()))
	if got := busyReason(ctx, r, 0.2, true); got != "" {
		t.Fatalf("local adapter counted: %s", got)
	}
	secondaryAdapters, _ := r.profileAdapters.Get("secondary")
	secondaryAdapters.Set(gateway.PlatformTelegram, newRestartTestAdapter())
	if got := busyReason(ctx, r, 0.2, true); !strings.Contains(got, "telegram") {
		t.Fatalf("telegram adapter: %q", got)
	}
}
