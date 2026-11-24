package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

func parsedInterval(t *testing.T, text string) int64 {
	t.Helper()
	got, err := ParseHeartbeatInterval(text)
	if err != nil {
		t.Fatalf("%q raised %v", text, err)
	}
	return got
}

func TestParseIntervalValid(t *testing.T) {
	for text, want := range map[string]int64{"10m": 600, "every 10m": 600, "2h": 7200, "every 2 hours": 7200, "1d": 86400, "90 minutes": 5400, "600s": 600} {
		if got := parsedInterval(t, text); got != want {
			t.Fatalf("%q -> %v", text, got)
		}
	}
}

func TestParseIntervalNotAnInterval(t *testing.T) {
	for _, text := range []string{"", "banana", "check CI", "every", "m10"} {
		if got := parsedInterval(t, text); got != 0 {
			t.Fatalf("%q parsed", text)
		}
	}
}

func TestParseIntervalTooSmallIsRejected(t *testing.T) {
	for _, text := range []string{"5s", "30s"} {
		if got := parsedInterval(t, text); got != -1 {
			t.Fatalf("%q -> %v", text, got)
		}
	}
	if got := parsedInterval(t, fmt.Sprintf("%ds", MinHeartbeatIntervalSeconds)); got != MinHeartbeatIntervalSeconds {
		t.Fatalf("floor -> %v", got)
	}
}

func TestParseIntervalRaisesKeyErrorForUnitsOnlyCaseFoldingMatches(t *testing.T) {
	for text, key := range map[string]string{"10ſ": "unknown interval unit ſ", "every 5 minuteſ": "unknown interval unit minuteſ", "3 mın": "unknown interval unit mın"} {
		got, err := ParseHeartbeatInterval(text)
		var raised *errorutil.Error
		if got != 0 || !errors.As(err, &raised) || raised.Kind != "KeyError" || raised.Msg != key {
			t.Fatalf("%q -> %v %v", text, got, err)
		}
	}
}

func TestParseIntervalRefusesSecondsBeyondSixtyFourBits(t *testing.T) {
	for _, text := range []string{"99999999999999999999999d", "1180591620717411303424d"} {
		if got, err := ParseHeartbeatInterval(text); got != 0 || !errors.Is(err, anyutil.ErrRange) {
			t.Fatalf("%q -> %d %v", text, got, err)
		}
	}
	_, err := ParseHeartbeatInterval(strings.Repeat("9", 400) + "s")
	if !errors.Is(err, anyutil.ErrRange) || err.Error() != "cannot convert +Inf to an integer: value out of range" {
		t.Fatalf("infinite seconds %v", err)
	}
}

func TestLargeIntervalHeartbeatPersistsAndNeverFires(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	interval := int64(math.MaxInt64)
	mgr := NewHeartbeatManager(ctx, "hb-large-sid")
	if _, err := mgr.Set("far away", interval); err != nil {
		t.Fatal(err)
	}
	loaded := NewHeartbeatManager(ctx, "hb-large-sid").State()
	if loaded == nil || loaded.IntervalSeconds != interval || loaded.IsDue(nowSeconds()+1e18) {
		t.Fatalf("loaded %+v", loaded)
	}
	line, err := mgr.StatusLine()
	if err != nil || line != ("♥ Heartbeat (every 9223372036854775807s, next in ~9223372036854775807s): far away") {
		t.Fatalf("status %q %v", line, err)
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

func TestHeartbeatStatusLineFollowsTheStoredState(t *testing.T) {
	goalsTestHome(t)
	mgr := NewHeartbeatManager(context.Background(), "hb-status-sid")
	status := func() string {
		t.Helper()
		line, err := mgr.StatusLine()
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	if got := status(); got != "No heartbeat. Set one with /heartbeat every <interval> <prompt>." {
		t.Fatalf("empty %q", got)
	}
	if _, err := mgr.Set("\t watch CI \u00a0", 7200); err != nil {
		t.Fatal(err)
	}
	if got := status(); !strings.HasPrefix(got, "♥ Heartbeat (every 2h, next in ~") || !strings.HasSuffix(got, "s): watch CI") {
		t.Fatalf("active %q", got)
	}
	mgr.State().FireCount = 3
	mgr.Pause()
	if got := status(); got != "⏸ Heartbeat (paused, every 2h, fired 3×): watch CI" {
		t.Fatalf("paused %q", got)
	}
	mgr.State().Status = "cleared"
	if got := status(); got != "Heartbeat (cleared, every 2h, fired 3×): watch CI" {
		t.Fatalf("cleared %q", got)
	}
	if _, err := mgr.Set("\t\u2003", 600); !IsHeartbeatValueError(err) || err.Error() != "heartbeat prompt is empty" {
		t.Fatalf("blank prompt %v", err)
	}
	if _, err := mgr.Set("ok", 59); !IsHeartbeatValueError(err) || err.Error() != "interval must be at least 60s" {
		t.Fatalf("short interval %v", err)
	}
}

func TestParseIntervalAcceptsEveryUnicodeSpace(t *testing.T) {
	for text, want := range map[string]int64{"\u00a0every\u200310\x1fm\u3000": 600, "\v2\u2028Hours\u0085": 7200, "1.5h": 5400, "٣m": 180} {
		if got := parsedInterval(t, text); got != want {
			t.Fatalf("%q -> %v", text, got)
		}
	}
	if got := parsedInterval(t, "10\u200bm"); got != 0 {
		t.Fatal("zero width space parsed")
	}
}

func TestViewerHeartbeatSetConvertsItsIntervalLikeTheManager(t *testing.T) {
	goalsTestHome(t)
	ctx := context.Background()
	call := func(args ...any) *jsonutil.Object {
		t.Helper()
		out, err := InvokeViewerAutomation(ctx, nil, "heartbeat", "viewer-hb-sid", "set", args, nil)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	large := int64(1) << 62
	set := call("watch", large)
	stored := LoadHeartbeat(ctx, "viewer-hb-sid")
	if _, isState := set.Value("value").(*jsonutil.Object); !isState || stored == nil || stored.IntervalSeconds != large {
		t.Fatalf("large interval set %v stored %+v", set, stored)
	}
	if text := call("watch", "600"); LoadHeartbeat(ctx, "viewer-hb-sid").IntervalSeconds != 600 || text.Value("error") != nil {
		t.Fatalf("text interval %v", text)
	}
	refused, _ := call("watch", "soon").Value("error").(*jsonutil.Object)
	if refused == nil || refused.Value("type") != "ValueError" || refused.Value("message") != "heartbeat interval must be a whole number of seconds, got soon" {
		t.Fatalf("refused %v", refused)
	}
}

// Reports a refused heartbeat set
func IsHeartbeatValueError(err error) bool {
	var hv *HeartbeatValueError
	return errors.As(err, &hv)
}
