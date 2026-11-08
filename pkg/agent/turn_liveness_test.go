package agent

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"runtime/pprof"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/state"
)

type loggerRecord struct {
	level  slog.Level
	logger string
	msg    string
}

type loggerCapture struct {
	mu     *sync.Mutex
	recs   *[]loggerRecord
	logger string
}

func (h loggerCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h loggerCapture) Handle(_ context.Context, r slog.Record) error {
	logger := h.logger
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "logger" {
			logger = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, loggerRecord{r.Level, logger, r.Message})
	return nil
}
func (h loggerCapture) WithAttrs(as []slog.Attr) slog.Handler {
	for _, a := range as {
		if a.Key == "logger" {
			h.logger = a.Value.String()
		}
	}
	return h
}
func (h loggerCapture) WithGroup(string) slog.Handler { return h }

// Collects the records one named logger emits at or above a level for the rest of the test
func captureLogger(t *testing.T, logger string, min slog.Level) func() []loggerRecord {
	t.Helper()
	var mu sync.Mutex
	var recs []loggerRecord
	prev := slog.Default()
	slog.SetDefault(slog.New(loggerCapture{mu: &mu, recs: &recs}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []loggerRecord {
		mu.Lock()
		defer mu.Unlock()
		var out []loggerRecord
		for _, r := range recs {
			if r.logger == logger && r.level >= min {
				out = append(out, r)
			}
		}
		return out
	}
}

func livenessSection(section any) map[string]any {
	return map[string]any{"agent": map[string]any{"turn_liveness": section}}
}

func mustResolveLiveness(t *testing.T, cfg map[string]any) (*float64, float64) {
	t.Helper()
	timeout, poll, err := resolveTurnLivenessSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return timeout, poll
}

func assertLivenessDefaults(t *testing.T, cfg map[string]any) {
	t.Helper()
	timeout, poll := mustResolveLiveness(t, cfg)
	if timeout == nil || *timeout != defaultTurnLivenessTimeoutS || poll != defaultTurnLivenessPollS {
		t.Fatalf("settings %v %v for %v", timeout, poll, cfg)
	}
}

func TestDefaultsWhenConfigMissingOrUnset(t *testing.T) {
	assertLivenessDefaults(t, nil)
	assertLivenessDefaults(t, map[string]any{})
	assertLivenessDefaults(t, livenessSection(map[string]any{}))
}

func TestPrecedenceExplicitValuesWinOverDefaults(t *testing.T) {
	timeout, poll := mustResolveLiveness(t, livenessSection(map[string]any{"timeout_s": 30, "poll_s": 5}))
	if *timeout != 30.0 || poll != 5.0 {
		t.Fatalf("settings %v %v", *timeout, poll)
	}
}

func TestPrecedenceNumericStringsAccepted(t *testing.T) {
	timeout, poll := mustResolveLiveness(t, livenessSection(map[string]any{"timeout_s": "45", "poll_s": "2.5"}))
	if *timeout != 45.0 || poll != 2.5 {
		t.Fatalf("settings %v %v", *timeout, poll)
	}
}

func TestTimeoutZeroIsDocumentedOptOut(t *testing.T) {
	timeout, poll := mustResolveLiveness(t, livenessSection(map[string]any{"timeout_s": 0, "poll_s": 15}))
	if timeout != nil || poll != 15.0 {
		t.Fatalf("settings %v %v", timeout, poll)
	}
	if timeout, _ := mustResolveLiveness(t, livenessSection(map[string]any{"timeout_s": -1})); timeout != nil {
		t.Fatalf("negative timeout %v", *timeout)
	}
}

func TestTypoDoesNotCrashAndFallsBackToDefault(t *testing.T) {
	records := captureLogger(t, "agent.turn_liveness", slog.LevelWarn)
	assertLivenessDefaults(t, livenessSection(map[string]any{"timeout_s": "oops", "poll_s": "typo"}))
	recs := records()
	if len(recs) != 2 {
		t.Fatalf("records %v", recs)
	}
	for _, r := range recs {
		if !strings.Contains(r.msg, "agent.turn_liveness") {
			t.Fatalf("record %q", r.msg)
		}
	}
}

func TestNanTimeoutFallsBackToDefaultNotDisabled(t *testing.T) {
	records := captureLogger(t, "agent.turn_liveness", slog.LevelWarn)
	assertLivenessDefaults(t, livenessSection(map[string]any{"timeout_s": math.NaN()}))
	if n := len(records()); n != 1 {
		t.Fatalf("records %d", n)
	}
}

func TestInfPollFallsBackToDefaultNotFrozenWatcher(t *testing.T) {
	records := captureLogger(t, "agent.turn_liveness", slog.LevelWarn)
	assertLivenessDefaults(t, livenessSection(map[string]any{"poll_s": math.Inf(1)}))
	if n := len(records()); n != 1 {
		t.Fatalf("records %d", n)
	}
}

func TestInfTimeoutFallsBackToDefault(t *testing.T) {
	records := captureLogger(t, "agent.turn_liveness", slog.LevelWarn)
	timeout, _ := mustResolveLiveness(t, livenessSection(map[string]any{"timeout_s": math.Inf(1)}))
	if *timeout != defaultTurnLivenessTimeoutS || len(records()) != 1 {
		t.Fatalf("timeout %v records %v", *timeout, records())
	}
}

func TestNonPositivePollFallsBackToDefault(t *testing.T) {
	captureLogger(t, "agent.turn_liveness", slog.LevelWarn)
	for _, raw := range []any{0, -3} {
		if _, poll := mustResolveLiveness(t, livenessSection(map[string]any{"poll_s": raw})); poll != defaultTurnLivenessPollS {
			t.Fatalf("poll %v for %v", poll, raw)
		}
	}
}

func TestMalformedSectionsFallBackToDefaults(t *testing.T) {
	records := captureLogger(t, "agent.turn_liveness", slog.LevelWarn)
	assertLivenessDefaults(t, map[string]any{"agent": "nonsense"})
	assertLivenessDefaults(t, livenessSection("nonsense"))
	if len(records()) < 1 {
		t.Fatal("the malformed section was not surfaced as a warning")
	}
}

func TestOverflowingKnobRaisesLikePythonFloat(t *testing.T) {
	huge, _ := new(big.Int).SetString("1"+strings.Repeat("0", 400), 10)
	if _, _, err := resolveTurnLivenessSettings(livenessSection(map[string]any{"timeout_s": huge})); err == nil || !strings.Contains(err.Error(), "int too large to convert to float") {
		t.Fatalf("overflow error %v", err)
	}
}

// Parks the calling goroutine until released so the stall dump captures this frame
func parkStalledTurnThread(release <-chan struct{}) { <-release }

func TestWatchdogDumpTheTurnThreadStackWhenItFires(t *testing.T) {
	a := &AIAgent{suppressStatusOutput: true}
	parked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go pprof.Do(context.Background(), pprof.Labels("thread", "prompt-turn-stalled"), func(context.Context) {
		defer close(done)
		a.executionGoroutineID.Store(pythread.GoroutineID())
		close(parked)
		parkStalledTurnThread(release)
	})
	<-parked
	w := newTurnLivenessWatchdog(a, turnLivenessWatchdogConfig{
		sessionID: "stalled-session", timeoutS: 600, pollS: 15, stopEvent: &pythread.Event{},
		activityLock: a.livenessActivityLock(), isTurnActive: func() bool { return true },
		commitAbort: func(activitySnapshot, string) bool { return true }, deactivateTurn: func() {},
	})
	records := captureLogger(t, "agent.turn_liveness", slog.LevelError)
	func() {
		defer func() { close(release); <-done }()
		w.surfaceStall(activitySnapshot{generation: 1, idleSeconds: 610.0})
	}()
	var msgs []string
	for _, r := range records() {
		msgs = append(msgs, r.msg)
	}
	dump := strings.Join(msgs, "\n")
	for _, want := range []string{"All thread stacks at turn stall", "prompt-turn-stalled", "<- turn thread", "parkStalledTurnThread"} {
		if !strings.Contains(dump, want) {
			t.Fatalf("dump lacks %q\n%s", want, dump)
		}
	}
	before := len(records())
	w.surfaceStall(activitySnapshot{generation: 1, idleSeconds: 620.0})
	if len(records()) != before {
		t.Fatal("a second surface of the same generation dumped again")
	}
}

func TestHostSleepIsNotAStalledTurn(t *testing.T) {
	for _, duringSample := range []bool{false, true} {
		t.Run(fmt.Sprintf("during_sample=%v", duringSample), func(t *testing.T) {
			wall, mono := 1000.0, 1000.0
			prevWall, prevMeter := activityWallNow, newTurnLivenessIdleMeter
			activityWallNow = func() float64 { return wall }
			newTurnLivenessIdleMeter = func() *state.AwakeIdleMeter {
				return state.NewAwakeIdleMeterWithClocks(func() float64 { return wall }, func() float64 { return mono })
			}
			t.Cleanup(func() { activityWallNow, newTurnLivenessIdleMeter = prevWall, prevMeter })
			a := &AIAgent{suppressStatusOutput: true}
			a.TouchActivity("starting new turn")
			aborts := 0
			w := newTurnLivenessWatchdog(a, turnLivenessWatchdogConfig{
				sessionID: "test", timeoutS: 600, pollS: 15, stopEvent: &pythread.Event{},
				activityLock: a.livenessActivityLock(), isTurnActive: func() bool { return true },
				commitAbort: func(activitySnapshot, string) bool { aborts++; return true }, deactivateTurn: func() {},
			})
			pendingSleep := 0.0
			originalSample := w.sample
			w.sample = func() *activitySnapshot {
				snapshot := originalSample()
				wall += pendingSleep
				pendingSleep = 0.0
				return snapshot
			}
			poll := func(asleep float64) bool {
				mono += 15
				if duringSample {
					wall += 15
					pendingSleep = asleep
				} else {
					wall += 15 + asleep
					pendingSleep = 0
				}
				return w.tick(context.Background())
			}
			poll(0)
			poll(900.0)
			if aborts != 0 {
				t.Fatalf("host sleep aborted the turn %d times", aborts)
			}
			for aborts == 0 && mono < 3000 {
				poll(0)
			}
			if aborts != 1 || mono-1000.0 != 600.0 {
				t.Fatalf("aborts %d after %v awake seconds", aborts, mono-1000.0)
			}
		})
	}
}

func TestKeepTurnBlockInDumpKeepsTheTurnGoroutineUnderTheCap(t *testing.T) {
	var blocks []string
	for id := 1; id <= turnStallDumpMaxThreads+5; id++ {
		blocks = append(blocks, fmt.Sprintf("goroutine %d [select]:\nmain.worker%d()", id, id))
	}
	turnID := uint64(turnStallDumpMaxThreads + 4)
	got := keepTurnBlockInDump(blocks, turnID)
	if len(got) != len(blocks) || got[turnStallDumpMaxThreads-1] != blocks[turnID-1] {
		t.Fatalf("turn block not in the last dumped slot %q", got[turnStallDumpMaxThreads-1])
	}
	if got[0] != blocks[0] || got[turnStallDumpMaxThreads] != blocks[turnStallDumpMaxThreads-1] || got[len(got)-1] != blocks[len(blocks)-1] {
		t.Fatal("the other goroutines lost their order")
	}
	if early := keepTurnBlockInDump(blocks, 3); &early[0] != &blocks[0] {
		t.Fatal("a turn goroutine already under the cap must leave the dump order alone")
	}
}
