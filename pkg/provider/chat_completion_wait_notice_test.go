package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var waitWarningCopy = []string{"provider may be slow", "no response yet"}

func TestNonstreamNoticeOncePerPhaseThenNearDeadlineOnly(t *testing.T) {
	var state WaitNoticeState
	clock := func(elapsed float64, lastEvent *float64) (string, *WatchdogDeadline) {
		phase := "first_event"
		if lastEvent != nil {
			phase = "post_event"
		}
		return phase, CodexWatchdogDeadline(CodexWatchdogClock{StaleTimeout: 600, TTFBEnabled: true, TTFBTimeout: 300, LastEventTS: lastEvent,
			CallStart: 1000, IdleEnabled: true, IdleTimeout: 180, Elapsed: elapsed})
	}
	var shown []string
	for hb := 2; hb < 10; hb++ {
		phase, wd := clock(30*float64(hb), nil)
		if state.ShouldEmit(phase, wd) {
			shown = append(shown, WaitNoticeText("test-model", 30*float64(hb), phase, wd))
		}
	}
	if len(shown) != 1 || !strings.Contains(shown[0], "test-model") || !strings.Contains(shown[0], "TTFB watchdog in 240s") {
		t.Fatalf("quiet heartbeats rewrote the line %q", shown)
	}
	phase, wd := clock(290, nil)
	if !state.ShouldEmit(phase, wd) || !strings.HasPrefix(WaitNoticeText("test-model", 290, phase, wd), "⏳ still waiting on test-model") {
		t.Fatal("near deadline update missing")
	}
	state.Reset()
	event := 1295.0
	emitted := 0
	for _, elapsed := range []float64{360, 390} {
		phase, wd := clock(elapsed, &event)
		if wd == nil || wd.Label != "stream idle" {
			t.Fatalf("post event watchdog %+v", wd)
		}
		if state.ShouldEmit(phase, wd) {
			emitted++
			shown = append(shown, WaitNoticeText("test-model", elapsed, phase, wd))
		}
	}
	if emitted != 1 {
		t.Fatalf("post event notices %d", emitted)
	}
	for _, n := range shown {
		for _, c := range waitWarningCopy {
			if strings.Contains(n, c) {
				t.Fatalf("warning copy in %q", n)
			}
		}
	}
}

func TestCodexWatchdogDeadlineEdges(t *testing.T) {
	inf := func() float64 { var z float64; return 1 / z }()
	if d := CodexWatchdogDeadline(CodexWatchdogClock{StaleTimeout: inf, TTFBTimeout: inf}); d != nil {
		t.Fatalf("disabled watchdogs gave %+v", d)
	}
	retry, last := 1100.0, 1150.0
	d := CodexWatchdogDeadline(CodexWatchdogClock{StaleTimeout: inf, RetryStartedTS: &retry, LastEventTS: &last, CallStart: 1000, ProgressTimeout: 300, Elapsed: 200})
	if d == nil || d.Label != "first progress" || d.Remaining != 200 {
		t.Fatalf("first progress deadline %+v", d)
	}
	if d := CodexWatchdogDeadline(CodexWatchdogClock{StaleTimeout: 100, Elapsed: 100}); d != nil {
		t.Fatalf("passed deadline %+v", d)
	}
}

type waitCapture struct {
	notices []string
	at      []float64
	touches []string
	now     *float64
}

func (c *waitCapture) fn(ev StreamEvent) error {
	switch ev.Kind {
	case EventWaitNotice:
		c.notices = append(c.notices, ev.Text)
		c.at = append(c.at, *c.now)
	case EventActivity:
		c.touches = append(c.touches, ev.Text)
	}
	return nil
}

func TestStreamMonitorNoticeDoesNotRepeatEveryHeartbeat(t *testing.T) {
	now := 1000.0
	capture := &waitCapture{now: &now}
	s := &ccStreamState{req: &Request{Model: "test-model"}, fn: capture.fn}
	m := newStreamWaitMonitor(s, "", 180, time.Unix(1000, 0))
	for _, secs := range []int{30, 60, 90, 120, 150} {
		m.lastHeartbeat = time.Unix(1000+int64(secs), 0)
		if err := m.heartbeat(secs); err != nil {
			t.Fatal(err)
		}
	}
	if len(capture.notices) != 1 || !strings.Contains(capture.notices[0], "test-model") || len(capture.touches) < 3 {
		t.Fatalf("notices %q touches %d", capture.notices, len(capture.touches))
	}
	_ = m.heartbeat(170)
	if len(capture.notices) != 2 {
		t.Fatalf("near stale update missing %q", capture.notices)
	}
	m.notice.Reset()
	s.firstChunkAt = time.Unix(1005, 0)
	_ = m.heartbeat(60)
	if len(capture.notices) != 3 || capture.notices[2] == capture.notices[0] {
		t.Fatalf("post chunk phase %q", capture.notices)
	}
}

func TestResumedChunksClearWaitWithoutErasingLocalLoad(t *testing.T) {
	for _, tc := range []struct {
		name               string
		localLoading, race bool
	}{{"remote", false, false}, {"local loading", true, false}, {"heartbeat race", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			now := 1000.0
			at := func(f float64) time.Time { return time.Unix(0, int64(f*1e9+0.5)) }
			loading := "Loading local model weights"
			prev := localLoadNotice
			localLoadNotice = func(string, string, int) (string, bool) { return loading, now >= 1060.6 }
			t.Cleanup(func() { localLoadNotice = prev })
			base := "https://example.com"
			if tc.localLoading {
				base = "http://localhost:1234"
			}
			capture := &waitCapture{now: &now}
			s := &ccStreamState{req: &Request{Model: "test-model"}, fn: capture.fn}
			m := newStreamWaitMonitor(s, base, 180, at(now))
			lastChunk := at(now)
			for steps := 0; now < 1062.0; steps++ {
				now = float64(int64((now+0.3)*10+0.5)) / 10
				if !tc.race && now >= 1061.5 {
					lastChunk = at(now)
				}
				beat := at(now).Sub(m.lastHeartbeat) >= streamHeartbeatInterval
				if _, err := m.tick(at(now), &lastChunk); err != nil {
					t.Fatal(err)
				}
				if tc.race && beat && at(now).Sub(lastChunk) >= time.Minute {
					now += 0.1
					lastChunk = at(now)
				}
			}
			if len(capture.notices) == 0 || capture.notices[0] == "" || len(capture.touches) == 0 {
				t.Fatalf("notices %q touches %q", capture.notices, capture.touches)
			}
			for _, a := range capture.at {
				if a < 1060.0 {
					t.Fatalf("notice before a minute of silence at %v", a)
				}
			}
			last := capture.notices[len(capture.notices)-1]
			switch {
			case tc.localLoading:
				for _, n := range capture.notices {
					if n == "" {
						t.Fatalf("local load line erased %q", capture.notices)
					}
				}
				if last != loading {
					t.Fatalf("last notice %q", last)
				}
			default:
				want := 1061.5
				if tc.race {
					want = 1060.4
				}
				if len(capture.notices) != 2 || last != "" || capture.at[1] < want-0.05 || capture.at[1] > want+0.05 {
					t.Fatalf("clear %q at %v", capture.notices, capture.at)
				}
			}
		})
	}
}

func writeManagedServerState(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if err := os.MkdirAll(RuntimesRoot(), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"base_url": "http://127.0.0.1:18434/v1", "api_key": "k"})
	if err := os.WriteFile(filepath.Join(RuntimesRoot(), "server.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadNoticeForManagedModel(t *testing.T) {
	writeManagedServerState(t)
	lr3LoadReset(t)
	lr3Apply(t, "Qwen-Test", "status_change", lr3LoadingEvent(0.5, "text_model"))
	notice, ok := managedLocalLoadNotice("http://127.0.0.1:18434/v1", "Qwen-Test", 0)
	if !ok || !strings.HasPrefix(notice, "⏳ loading Qwen-Test into memory — 42%") {
		t.Fatalf("notice %q", notice)
	}
	if _, ok := managedLocalLoadNotice("http://127.0.0.1:9999/v1", "Qwen-Test", 0); ok {
		t.Fatal("claimed a foreign server load")
	}
	prev := prefillProgress
	prefillProgress = func(string) (int64, bool, error) { return 0, false, nil }
	t.Cleanup(func() { prefillProgress = prev })
	if _, ok := managedLocalLoadNotice("http://127.0.0.1:18434/v1", "Elsewhere", 0); ok {
		t.Fatal("notice for a model that is not loading")
	}
}

func TestPrefillNoticeForManagedModel(t *testing.T) {
	writeManagedServerState(t)
	lr3LoadReset(t)
	prev := prefillProgress
	prefillProgress = func(string) (int64, bool, error) { return 12288, true, nil }
	t.Cleanup(func() { prefillProgress = prev })
	if n, ok := managedLocalLoadNotice("http://127.0.0.1:18434/v1", "Qwen-Test", 39551); !ok || n != "⚙ processing prompt — 31%" {
		t.Fatalf("notice %q", n)
	}
	if n, ok := managedLocalLoadNotice("http://127.0.0.1:18434/v1", "Qwen-Test", 100); !ok || n != "⚙ processing prompt" {
		t.Fatalf("undercounted notice %q", n)
	}
	lr3Apply(t, "Qwen-Test", "status_change", lr3LoadingEvent(0.5, "text_model"))
	if n, _ := managedLocalLoadNotice("http://127.0.0.1:18434/v1", "Qwen-Test", 39551); !strings.HasPrefix(n, "⏳ loading Qwen-Test") {
		t.Fatalf("load notice did not outrank prefill %q", n)
	}
	prefillProgress = func(string) (int64, bool, error) { return 0, false, errors.New("bad slots") }
	lr3LoadReset(t)
	if _, ok := managedLocalLoadNotice("http://127.0.0.1:18434/v1", "Qwen-Test", 39551); ok {
		t.Fatal("a failing probe produced a notice")
	}
}

func TestStaleBreakerShortCircuitsCountsKillsAndResetsOnSuccess(t *testing.T) {
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "2")
	session := &RetrySession{}
	ctx := withStaleStreak(context.Background(), session)
	bumpStaleStreak(ctx)
	bumpStaleStreak(ctx)
	err := checkStaleGiveup(session)
	if err == nil || !strings.Contains(err.Error(), "2 consecutive stale attempts") || !strings.Contains(err.Error(), "aborting this call") {
		t.Fatalf("breaker error %v", err)
	}
	client := &fakeClient{route: Route{Provider: "openrouter", Model: "m"}}
	if _, err := Do(context.Background(), client, &Request{Model: "m"}, DoOptions{Session: session, MaxRetries: 0}); err == nil || client.calls != 0 {
		t.Fatalf("open breaker reached the network calls=%d err=%v", client.calls, err)
	}
	session.ResetStaleStreak()
	if _, err := Do(context.Background(), client, &Request{Model: "m"}, DoOptions{Session: session}); err != nil || session.StaleStreak() != 0 {
		t.Fatalf("success path err=%v streak=%d", err, session.StaleStreak())
	}
	t.Setenv("ICEBATH_STREAM_STALE_GIVEUP", "0")
	bumpStaleStreak(ctx)
	bumpStaleStreak(ctx)
	if checkStaleGiveup(session) != nil {
		t.Fatal("breaker fired with give up disabled")
	}
}

func TestRetryRecoveredLocalServerLetsOneAttemptThrough(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	base := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/v1"
	if !localServerAnswers(base) {
		t.Fatal("localhost health check failed")
	}
	session := &RetrySession{}
	session.staleStreak.Store(5)
	if session.RetryRecoveredLocalServer(ModeChatCompletions, "openrouter", base) {
		t.Fatal("unmanaged provider retried")
	}
	healthy.Store(false)
	if session.RetryRecoveredLocalServer(ModeChatCompletions, "llamacpp", base) {
		t.Fatal("unhealthy server retried")
	}
	healthy.Store(true)
	if !session.RetryRecoveredLocalServer(ModeChatCompletions, "llamacpp", base) || session.StaleStreak() != 4 {
		t.Fatalf("recovered server streak %d", session.StaleStreak())
	}
	if session.RetryRecoveredLocalServer(ModeChatCompletions, "llamacpp", base) {
		t.Fatal("breaker below threshold retried again")
	}
	if localServerAnswers("not a url") {
		t.Fatal("schemeless base answered")
	}
}
