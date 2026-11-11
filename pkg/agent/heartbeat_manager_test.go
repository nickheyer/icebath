package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseIntervalValid(t *testing.T) {
	for text, want := range map[string]int64{"10m": 600, "every 10m": 600, "2h": 7200, "every 2 hours": 7200, "1d": 86400, "90 minutes": 5400, "600s": 600} {
		if got, ok := ParseHeartbeatInterval(text); !ok || got != want {
			t.Fatalf("%q -> %d %v", text, got, ok)
		}
	}
}

func TestParseIntervalNotAnInterval(t *testing.T) {
	for _, text := range []string{"", "banana", "check CI", "every", "m10"} {
		if _, ok := ParseHeartbeatInterval(text); ok {
			t.Fatalf("%q parsed", text)
		}
	}
}

func TestParseIntervalTooSmallIsRejected(t *testing.T) {
	for _, text := range []string{"5s", "30s"} {
		if got, ok := ParseHeartbeatInterval(text); !ok || got != -1 {
			t.Fatalf("%q -> %d %v", text, got, ok)
		}
	}
	if got, ok := ParseHeartbeatInterval(fmt.Sprintf("%ds", MinHeartbeatIntervalSeconds)); !ok || got != MinHeartbeatIntervalSeconds {
		t.Fatalf("floor -> %d %v", got, ok)
	}
}

func TestFormatInterval(t *testing.T) {
	for seconds, want := range map[int64]string{600: "10m", 7200: "2h", 86400: "1d", 90: "90s"} {
		if got := FormatHeartbeatInterval(seconds); got != want {
			t.Fatalf("%d -> %q", seconds, got)
		}
	}
}

func TestIsDueAnchorsOnCreatedThenLastFired(t *testing.T) {
	now := nowSeconds()
	s := &HeartbeatState{Prompt: "p", IntervalSeconds: 600, Status: "active", CreatedAt: now}
	if s.IsDue(now+1) || !s.IsDue(now+601) {
		t.Fatal("created anchor")
	}
	s.LastFiredAt = now + 601
	if s.IsDue(now+700) || !s.IsDue(now+1300) {
		t.Fatal("last fired anchor")
	}
}

func TestPausedNeverDue(t *testing.T) {
	now := nowSeconds()
	s := &HeartbeatState{Prompt: "p", IntervalSeconds: 60, Status: "paused", CreatedAt: now - 3600}
	if s.IsDue(now) {
		t.Fatal("paused heartbeat due")
	}
}

func TestRenderPromptContainsInstructionAndInterval(t *testing.T) {
	rendered := NewHeartbeatState("check the deploy", 600).RenderPrompt()
	if !strings.Contains(rendered, "check the deploy") || !strings.Contains(rendered, "10m") {
		t.Fatalf("rendered %q", rendered)
	}
}

func TestManagerSetPauseResumeClear(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	mgr := NewHeartbeatManager(ctx, "hb-lifecycle-sid")
	st, err := mgr.Set("watch CI", 600)
	if err != nil || st.Status != "active" || !mgr.IsActive() {
		t.Fatalf("set %#v %v", st, err)
	}
	mgr.Pause()
	if mgr.IsActive() || !mgr.HasHeartbeat() {
		t.Fatal("pause")
	}
	mgr.Resume()
	if !mgr.IsActive() {
		t.Fatal("resume")
	}
	if !mgr.Clear() || mgr.HasHeartbeat() || LoadHeartbeat(ctx, "hb-lifecycle-sid") != nil {
		t.Fatal("clear")
	}
}

func TestManagerRejectsBadInput(t *testing.T) {
	goalsTestHome(t)
	mgr := NewHeartbeatManager(context.Background(), "hb-bad-sid")
	if _, err := mgr.Set("", 600); !IsHeartbeatValueError(err) {
		t.Fatalf("empty prompt %v", err)
	}
	if _, err := mgr.Set("ok", 5); !IsHeartbeatValueError(err) {
		t.Fatalf("short interval %v", err)
	}
}

func TestManagerPersistsAcrossInstances(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	if _, err := NewHeartbeatManager(ctx, "hb-persist-sid").Set("persisted prompt", 600); err != nil {
		t.Fatal(err)
	}
	again := NewHeartbeatManager(ctx, "hb-persist-sid")
	if !again.HasHeartbeat() || again.State().Prompt != "persisted prompt" {
		t.Fatalf("again %#v", again.State())
	}
}

func TestDuePromptFiresOnceAndReanchors(t *testing.T) {
	goalsTestHome(t)
	mgr := NewHeartbeatManager(context.Background(), "hb-due-sid")
	if _, err := mgr.Set("tick", 600); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.DuePrompt(nil); ok {
		t.Fatal("fired before due")
	}
	mgr.State().CreatedAt = nowSeconds() - 700
	prompt, ok := mgr.DuePrompt(nil)
	if !ok || !strings.Contains(prompt, "tick") || mgr.State().FireCount != 1 {
		t.Fatalf("prompt %q %v", prompt, ok)
	}
	if _, ok := mgr.DuePrompt(nil); ok {
		t.Fatal("fired twice")
	}
}

func TestAStaleManagerDoesNotFireAHeartbeatStoppedSince(t *testing.T) {
	for _, tc := range []struct{ stop, stored string }{{"clear", ""}, {"pause", "paused"}} {
		t.Run(tc.stop, func(t *testing.T) {
			goalsTestHome(t)
			ctx := context.Background()
			sid := "hb-stale-" + tc.stop
			mgr := NewHeartbeatManager(ctx, sid)
			st, err := mgr.Set("tick", 600)
			if err != nil {
				t.Fatal(err)
			}
			st.CreatedAt = nowSeconds() - 700
			other := NewHeartbeatManager(ctx, sid)
			if tc.stop == "clear" {
				other.Clear()
			} else {
				other.Pause()
			}
			if _, ok := mgr.DuePrompt(nil); ok {
				t.Fatal("stale manager fired")
			}
			got := ""
			if stored := LoadHeartbeat(ctx, sid); stored != nil {
				got = stored.Status
			}
			if got != tc.stored {
				t.Fatalf("stored %q", got)
			}
		})
	}
}

func TestAHeartbeatStoppedDuringAClaimStaysStopped(t *testing.T) {
	for _, tc := range []struct{ stop, stored string }{{"clear", ""}, {"pause", "paused"}} {
		t.Run(tc.stop, func(t *testing.T) {
			goalsTestHome(t)
			ctx := context.Background()
			sid := "hb-race-" + tc.stop
			mgr := NewHeartbeatManager(ctx, sid)
			st, err := mgr.Set("tick", 600)
			if err != nil {
				t.Fatal(err)
			}
			st.CreatedAt = nowSeconds() - 700
			other := NewHeartbeatManager(ctx, sid)
			claimed := make(chan bool)
			go func() {
				_, ok := mgr.DuePrompt(nil)
				claimed <- ok
			}()
			stopped := make(chan struct{})
			go func() {
				if tc.stop == "clear" {
					other.Clear()
				} else {
					other.Pause()
				}
				close(stopped)
			}()
			<-claimed
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("stop never ran")
			}
			got := ""
			if stored := LoadHeartbeat(ctx, sid); stored != nil {
				got = stored.Status
			}
			if got != tc.stored {
				t.Fatalf("stored %q", got)
			}
		})
	}
}

func TestMissedTicksCoalesce(t *testing.T) {
	goalsTestHome(t)
	mgr := NewHeartbeatManager(context.Background(), "hb-coalesce-sid")
	if _, err := mgr.Set("tick", 600); err != nil {
		t.Fatal(err)
	}
	mgr.State().CreatedAt = nowSeconds() - 600*5 - 10
	if _, ok := mgr.DuePrompt(nil); !ok {
		t.Fatal("first fire")
	}
	if _, ok := mgr.DuePrompt(nil); ok || mgr.State().FireCount != 1 {
		t.Fatal("coalesce")
	}
}

func TestResumeReanchorsInsteadOfInstantFire(t *testing.T) {
	goalsTestHome(t)
	mgr := NewHeartbeatManager(context.Background(), "hb-resume-sid")
	if _, err := mgr.Set("tick", 600); err != nil {
		t.Fatal(err)
	}
	mgr.State().CreatedAt = nowSeconds() - 3600
	mgr.Pause()
	mgr.Resume()
	if _, ok := mgr.DuePrompt(nil); ok {
		t.Fatal("instant fire after resume")
	}
}
