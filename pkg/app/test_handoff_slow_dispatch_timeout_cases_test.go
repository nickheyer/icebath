package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/state"
)

// Real state database in a fresh home whose gateway config enables discord with a home channel
func handoffCaseDB(t *testing.T) *state.SessionDB {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	yaml := "platforms:\n  discord:\n    enabled: true\n    token: test-token\n    home_channel:\n      platform: discord\n      chat_id: \"123\"\n      name: home\n"
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := state.NewSessionDB(context.Background(), filepath.Join(t.TempDir(), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// Registry store whose handoff state read runs a hook, the wrapped database answering the rest
type handoffProxy struct {
	registryHandoffStore
	getState func(sessionID string) *pyjson.Object
}

func (p handoffProxy) GetHandoffState(_ context.Context, sessionID string) *pyjson.Object {
	return p.getState(sessionID)
}

// Runs /handoff discord against the store on a clock each half second sleep advances by two seconds
func runHandoff(t *testing.T, store handoffStore, sessionID string) (bool, string, *CLI) {
	t.Helper()
	out := &bytes.Buffer{}
	t.Cleanup(SetStreams(&cliui.Console{In: strings.NewReader(""), Out: out}, out))
	clock := float64(time.Now().UnixNano()) / 1e9
	savedTime, savedSleep := handoffTime, handoffSleep
	handoffTime = func() float64 { return clock }
	handoffSleep = func(d time.Duration) { clock += max(d.Seconds()*4, 2.0) }
	t.Cleanup(func() { handoffTime, handoffSleep = savedTime, savedSleep })
	c := &CLI{ctx: context.Background(), sessionID: sessionID, handoffDB: store}
	keep := c.handleHandoffCommand("/handoff discord")
	return keep, out.String(), c
}

// Handoff state of a session read straight off the database
func handoffState(db *state.SessionDB, sessionID string) any {
	row := db.GetHandoffState(context.Background(), sessionID)
	if row == nil {
		return nil
	}
	value, _ := row.Get("state")
	return value
}

func TestCLIWaitLoopPendingTimeoutStillReportsGatewayDown(t *testing.T) {
	db := handoffCaseDB(t)
	if err := db.EnsureSession(state.NewSession{ID: "cli-sess-a", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	keep, out, c := runHandoff(t, registryHandoffStore{db}, "cli-sess-a")
	if !keep {
		t.Fatal("keep")
	}
	if !strings.Contains(out, c.commandsT("handoff.timed_out", nil)) {
		t.Fatalf("timed out line missing in\n%s", out)
	}
	if got := handoffState(db, "cli-sess-a"); got != "failed" {
		t.Fatalf("state %v", got)
	}
}

func TestCLIWaitLoopRunningRowIsNeverFailedByCli(t *testing.T) {
	db := handoffCaseDB(t)
	ctx := context.Background()
	if err := db.EnsureSession(state.NewSession{ID: "cli-sess-b", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	proxy := handoffProxy{registryHandoffStore: registryHandoffStore{db}, getState: func(sid string) *pyjson.Object {
		row := db.GetHandoffState(ctx, sid)
		if row != nil {
			if value, _ := row.Get("state"); value == "pending" {
				if _, err := db.ClaimHandoff(ctx, sid); err != nil {
					t.Fatal(err)
				}
				row = db.GetHandoffState(ctx, sid)
			}
		}
		return row
	}}
	keep, out, c := runHandoff(t, proxy, "cli-sess-b")
	if !keep {
		t.Fatal("keep")
	}
	if strings.Contains(out, c.commandsT("handoff.timed_out", nil)) {
		t.Fatalf("timed out line printed in\n%s", out)
	}
	if !strings.Contains(out, c.commandsT("handoff.taking_long", nil)) {
		t.Fatalf("taking long line missing in\n%s", out)
	}
	if got := handoffState(db, "cli-sess-b"); got != "running" {
		t.Fatalf("state %v", got)
	}
	if err := db.CompleteHandoff(ctx, "cli-sess-b"); err != nil {
		t.Fatal(err)
	}
	if got := handoffState(db, "cli-sess-b"); got != "completed" {
		t.Fatalf("state after completion %v", got)
	}
}

func TestCLIWaitLoopSlowDispatchBeyond60sCompletes(t *testing.T) {
	db := handoffCaseDB(t)
	ctx := context.Background()
	if err := db.EnsureSession(state.NewSession{ID: "cli-sess-c", Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	polls := 0
	proxy := handoffProxy{registryHandoffStore: registryHandoffStore{db}, getState: func(sid string) *pyjson.Object {
		polls++
		var current any
		if row := db.GetHandoffState(ctx, sid); row != nil {
			current, _ = row.Get("state")
		}
		if current == "pending" && polls >= 2 {
			if _, err := db.ClaimHandoff(ctx, sid); err != nil {
				t.Fatal(err)
			}
		}
		if current == "running" && polls >= 45 {
			if err := db.CompleteHandoff(ctx, sid); err != nil {
				t.Fatal(err)
			}
		}
		return db.GetHandoffState(ctx, sid)
	}}
	keep, out, c := runHandoff(t, proxy, "cli-sess-c")
	if keep {
		t.Fatal("completed handoff keeps the session")
	}
	if !strings.Contains(out, c.commandsT("handoff.complete", map[string]any{"platform": "discord"})) {
		t.Fatalf("complete line missing in\n%s", out)
	}
	if strings.Contains(out, c.commandsT("handoff.timed_out", nil)) {
		t.Fatalf("timed out line printed in\n%s", out)
	}
	if !c.shouldExit {
		t.Fatal("should exit")
	}
	if got := handoffState(db, "cli-sess-c"); got != "completed" {
		t.Fatalf("state %v", got)
	}
}
