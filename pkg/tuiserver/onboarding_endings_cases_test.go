package tuiserver

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Server DB of a fresh launch home with the desktop onboarding chat started on the mac facts
func onboardingEndingsDB(t *testing.T) (*state.DB, string) {
	t.Helper()
	h := onboardingLaunchRoot(t)
	db := getDB(context.Background())
	if err := db.CreateSession(state.NewSession{ID: "onb", Source: "desktop"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.StartOnboarding(context.Background(), db, "onb", tools.OnboardingFacts{OS: "macos", Arch: "arm64", MachineKind: "Mac"}); err != nil {
		t.Fatal(err)
	}
	return db, h
}

// Ended announcements told while the case runs
type endedLog struct {
	mu     sync.Mutex
	active bool
	told   []map[string]any
}

func (l *endedLog) snapshot() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.told...)
}

// Listener collecting the ended announcements of this case only
func onboardingEndedLog(t *testing.T) *endedLog {
	t.Helper()
	log := &endedLog{active: true}
	tools.OnOnboardingEnded(func(e map[string]any) {
		log.mu.Lock()
		defer log.mu.Unlock()
		if log.active {
			log.told = append(log.told, e)
		}
	})
	t.Cleanup(func() {
		log.mu.Lock()
		log.active = false
		log.mu.Unlock()
	})
	return log
}

// The install flag onboarding.run of the launch config
func onboardingRunFlag(t *testing.T, h string) any {
	t.Helper()
	raw, err := config.ReadUserConfigRaw(context.Background(), filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	section, _ := raw["onboarding"].(map[string]any)
	return section["run"]
}

// Delete closure removing a session through the store
func deleteSessionFn(db *state.DB, sid string) func() bool {
	return func() bool {
		removed, err := db.DeleteSession(context.Background(), sid, state.DeleteSessionOptions{})
		return err == nil && removed
	}
}

func TestARefusedDeleteLeavesTheRunGoing(t *testing.T) {
	db, h := onboardingEndingsDB(t)
	ended := onboardingEndedLog(t)
	deleted, err := tools.DeleteOnboardingChat(context.Background(), db, "onb", func() bool { return false })
	if err != nil || deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	if tools.ActiveOnboardingSessionID(db) != "onb" || onboardingRunFlag(t, h) != true || len(ended.snapshot()) != 0 {
		t.Fatalf("active %q flag %v ended %v", tools.ActiveOnboardingSessionID(db), onboardingRunFlag(t, h), ended.snapshot())
	}
}

func TestDeletingTheNewestCopyEndsTheRunOnEveryCopy(t *testing.T) {
	db, h := onboardingEndingsDB(t)
	ended := onboardingEndedLog(t)
	for _, pair := range [][2]string{{"onb", "mid"}, {"mid", "tip"}} {
		p := state.NewCompressionChildPublish(pair[0], pair[1], "desktop", []map[string]any{{"role": "user", "content": "summary"}})
		p.RequireCompressionLease = false
		if err := db.PublishCompressionChild(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := tools.DeleteOnboardingChat(context.Background(), db, "tip", deleteSessionFn(db, "tip"))
	if err != nil || !deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	if tools.ActiveOnboardingSessionID(db) != "" || onboardingRunFlag(t, h) != false {
		t.Fatalf("active %q flag %v", tools.ActiveOnboardingSessionID(db), onboardingRunFlag(t, h))
	}
	for _, sid := range []string{"onb", "mid"} {
		if got := tools.LoadOnboardingRecord(db, sid)["ended_by"]; got != "deleted" {
			t.Fatalf("%s ended_by %v", sid, got)
		}
	}
	if told := ended.snapshot(); len(told) != 1 || told[0]["stored_session_id"] != "tip" || told[0]["outcome"] != "deleted" {
		t.Fatalf("ended %v", told)
	}
}

func TestADeleteWhoseFlagWriteFailsKeepsTheChatForARetry(t *testing.T) {
	db, h := onboardingEndingsDB(t)
	ended := onboardingEndedLog(t)
	real := tools.SetOnboardingRun
	tools.SetOnboardingRun = func(context.Context, bool, bool) error { return errors.New("disk full") }
	_, err := tools.DeleteOnboardingChat(context.Background(), db, "onb", deleteSessionFn(db, "onb"))
	tools.SetOnboardingRun = real
	if err == nil {
		t.Fatal("the failed flag write was swallowed")
	}
	if s, _ := db.GetSession(context.Background(), "onb"); s == nil {
		t.Fatal("the chat was deleted after the flag write failed")
	}
	if tools.ActiveOnboardingSessionID(db) != "onb" || len(ended.snapshot()) != 0 {
		t.Fatalf("active %q ended %v", tools.ActiveOnboardingSessionID(db), ended.snapshot())
	}
	deleted, err := tools.DeleteOnboardingChat(context.Background(), db, "onb", deleteSessionFn(db, "onb"))
	if err != nil || !deleted {
		t.Fatalf("retry = %v %v", deleted, err)
	}
	if told := ended.snapshot(); onboardingRunFlag(t, h) != false || len(told) != 1 || told[0]["outcome"] != "deleted" {
		t.Fatalf("flag %v ended %v", onboardingRunFlag(t, h), told)
	}
}

func TestADeleteRefusedByTheWriteGuardLeavesTheFlagSet(t *testing.T) {
	db, h := onboardingEndingsDB(t)
	ended := onboardingEndedLog(t)
	var guardErr error
	deleted, err := tools.DeleteOnboardingChat(context.Background(), db, "onb", func() bool {
		guardErr = &state.SessionActiveWriteGuardError{Msg: "active turn lease"}
		return false
	})
	if err != nil || deleted {
		t.Fatalf("delete = %v %v", deleted, err)
	}
	var guard *state.SessionActiveWriteGuardError
	if !errors.As(guardErr, &guard) {
		t.Fatalf("guard error %v", guardErr)
	}
	if tools.ActiveOnboardingSessionID(db) != "onb" || onboardingRunFlag(t, h) != true || len(ended.snapshot()) != 0 {
		t.Fatalf("active %q flag %v ended %v", tools.ActiveOnboardingSessionID(db), onboardingRunFlag(t, h), ended.snapshot())
	}
}
