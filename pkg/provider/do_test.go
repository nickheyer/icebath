package provider

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/state"
)

func TestFastModeUnprovisionedDetectsOnlyZeroLimitFast429s(t *testing.T) {
	limit := func(v string) http.Header {
		h := http.Header{}
		h.Set("anthropic-fast-input-tokens-limit", v)
		h.Set("anthropic-fast-output-tokens-limit", v)
		return h
	}
	fastKwargs := map[string]any{"speed": "fast"}
	if !FastModeUnprovisioned(&APIError{StatusCode: 429}, fastKwargs, limit("0")) {
		t.Fatal("zero limit fast 429 should be unprovisioned")
	}
	if FastModeUnprovisioned(&APIError{StatusCode: 429}, fastKwargs, limit("2000000")) {
		t.Fatal("real limit is transient")
	}
	if FastModeUnprovisioned(&APIError{StatusCode: 429}, map[string]any{}, limit("0")) {
		t.Fatal("request must have asked for fast speed")
	}
	if FastModeUnprovisioned(&APIError{StatusCode: 500}, fastKwargs, limit("0")) {
		t.Fatal("non 429 never matches")
	}
}

func TestFastWindowPolicy(t *testing.T) {
	clock := time.Unix(1000, 0)
	fw := NewFastWindow("auto", map[string]any{"extra_body": map[string]any{"keep": 1}})
	fw.now = func() time.Time { return clock }
	fw.BeginTurn(nil)
	if !fw.Open() {
		t.Fatal("auto window should open on a user turn")
	}
	clock = clock.Add(61 * time.Second)
	if fw.Open() {
		t.Fatal("window should expire")
	}
	fw.BeginTurn([]*state.Message{{Role: "user", Content: "x"}})
	if !fw.Open() {
		t.Fatal("auto re-opens on the next turn")
	}
	cold := NewFastWindow("cold", nil)
	cold.now = func() time.Time { return clock }
	cold.BeginTurn([]*state.Message{{Role: "user", Content: "x"}})
	if cold.Open() {
		t.Fatal("cold with prior history must not open")
	}
	cold.BeginTurn(nil)
	if !cold.Open() {
		t.Fatal("cold first turn opens")
	}
	static := NewFastWindow("priority", map[string]any{"speed": "fast"})
	static.BeginTurn(nil)
	if static.Open() {
		t.Fatal("static tiers have no bounded window")
	}
	route := Route{Provider: "anthropic", Model: "claude-opus-5", Mode: ModeAnthropicMessages}
	if got := static.EffectiveRequestOverrides(route, ""); got["speed"] != "fast" {
		t.Fatalf("static overrides %v", got)
	}
	if !static.MarkFastModeUnavailable("claude-opus-5") || static.MarkFastModeUnavailable("claude-opus-5") {
		t.Fatal("mark must succeed once per model")
	}
	if got := static.EffectiveRequestOverrides(route, ""); len(got) != 0 {
		t.Fatalf("speed should be dropped for the unavailable model: %v", got)
	}
	route.Model = "claude-opus-5-5"
	if got := static.EffectiveRequestOverrides(route, ""); got["speed"] != "fast" {
		t.Fatalf("other model keeps speed: %v", got)
	}
	if static.RequestOverrides["speed"] != "fast" {
		t.Fatal("request overrides must never be mutated")
	}
}

func TestServiceTierParsing(t *testing.T) {
	cases := map[string]string{"auto": "auto", "COLD": "cold", "fast": "priority", "": "", "bogus": "", "ultra": "ultrafast", "on": "priority", "normal": ""}
	for raw, want := range cases {
		if got := ParseServiceTier(raw); got != want {
			t.Fatalf("ParseServiceTier(%q) = %q want %q", raw, got, want)
		}
	}
	if _, err := ParseExactServiceTier("bogus"); err == nil {
		t.Fatal("unknown word must error")
	}
	if tier, err := ParseExactServiceTier(""); err != nil || tier != "" {
		t.Fatalf("empty pins normal: %q %v", tier, err)
	}
	if ServiceTierWord("priority") != "fast" || ServiceTierWord("") != "normal" || ServiceTierWord("auto") != "auto" {
		t.Fatal("service tier words")
	}
}

func TestClampTimeout(t *testing.T) {
	if _, ok := ClampTimeout(0); ok {
		t.Fatal("zero is unbounded")
	}
	if _, ok := ClampTimeout(-5); ok {
		t.Fatal("negative is unbounded")
	}
	if d, ok := ClampTimeout(420); !ok || d != 420*time.Second {
		t.Fatalf("normal value %v %v", d, ok)
	}
	if d, ok := ClampTimeout(1e18); !ok || d != MaxSafeTimeout {
		t.Fatalf("oversized value %v %v", d, ok)
	}
	if _, ok := ClampTimeout(math.NaN()); ok {
		t.Fatal("NaN is unbounded")
	}
}

type fakeClock struct {
	now   time.Time
	slept []time.Duration
}

func useFakeClock(t *testing.T) *fakeClock {
	t.Helper()
	clock := &fakeClock{now: time.Unix(0, 0)}
	oldNow, oldSleep := deadlineNow, deadlineSleep
	deadlineNow = func() time.Time { return clock.now }
	deadlineSleep = func(ctx context.Context, d time.Duration) error {
		clock.slept = append(clock.slept, d)
		clock.now = clock.now.Add(d)
		return nil
	}
	t.Cleanup(func() { deadlineNow, deadlineSleep = oldNow, oldSleep })
	return clock
}

func TestPollUntil(t *testing.T) {
	clock := useFakeClock(t)
	if got := PollUntil(func() (int, bool) { return 4242, true }, 10*time.Second, 500*time.Millisecond); got != 4242 || len(clock.slept) != 0 {
		t.Fatalf("truthy result %v slept %v", got, clock.slept)
	}
	clock.slept = nil
	if got := PollUntil(func() (bool, bool) { return false, false }, 2*time.Second, 500*time.Millisecond); got {
		t.Fatal("falsy at deadline")
	}
	total := time.Duration(0)
	for _, s := range clock.slept {
		total += s
	}
	if total != 2*time.Second {
		t.Fatalf("slept %v", clock.slept)
	}
	clock.now, clock.slept = time.Unix(0, 0), nil
	got := PollUntil(func() (bool, bool) { ok := clock.now.Sub(time.Unix(0, 0)) >= 5*time.Second; return ok, ok }, 5*time.Second, 3*time.Second)
	if !got || len(clock.slept) != 2 || clock.slept[0] != 3*time.Second || clock.slept[1] != 2*time.Second {
		t.Fatalf("deadline condition %v slept %v", got, clock.slept)
	}
	clock.slept = nil
	calls := 0
	PollUntil(func() (any, bool) { calls++; return nil, false }, 0, time.Second)
	if calls != 1 || len(clock.slept) != 0 {
		t.Fatalf("zero timeout calls %d slept %v", calls, clock.slept)
	}
}

func TestRunBounded(t *testing.T) {
	res, err := RunBounded(context.Background(), func(ctx context.Context) (any, error) { return 7, nil }, 0, false, "op", nil, nil)
	if err != nil || res.Value != 7 || res.TimedOut || res.Bounded {
		t.Fatalf("unbounded %+v %v", res, err)
	}
	boom := errors.New("boom")
	if _, err := RunBounded(context.Background(), func(ctx context.Context) (any, error) { return nil, boom }, time.Second, true, "op", nil, nil); !errors.Is(err, boom) {
		t.Fatalf("operation error must propagate: %v", err)
	}
	marked := &suspectBackend{}
	onTimeout := 0
	release := make(chan struct{})
	defer close(release)
	res, err = RunBounded(context.Background(), func(ctx context.Context) (any, error) { <-release; return nil, nil }, 20*time.Millisecond, true, "slow", func() { onTimeout++ }, marked)
	if err != nil || !res.TimedOut || onTimeout != 1 || marked.reasons != 1 {
		t.Fatalf("timeout %+v err %v onTimeout %d marked %d", res, err, onTimeout, marked.reasons)
	}
	if !strings.Contains(marked.last, "slow timed out after 0.0s") {
		t.Fatalf("mark reason %q", marked.last)
	}
	res, err = RunBounded(context.Background(), func(ctx context.Context) (any, error) { return "fast", nil }, time.Second, true, "quick", nil, marked)
	if err != nil || res.TimedOut || res.Value != "fast" || marked.reasons != 1 {
		t.Fatalf("completed op must never mark: %+v %v %d", res, err, marked.reasons)
	}
}

type suspectBackend struct {
	reasons int
	last    string
}

func (s *suspectBackend) MarkSuspect(reason string) { s.reasons++; s.last = reason }

func TestIsLocalValidationError(t *testing.T) {
	if !IsLocalValidationError(&LocalValidationError{Message: "bad request shape"}) {
		t.Fatal("local validation error")
	}
	if IsLocalValidationError(&APIError{StatusCode: 400, Kind: ErrInvalidRequest}) {
		t.Fatal("provider 400 is not local")
	}
	if IsLocalValidationError(&TimeoutPhaseError{Phase: TimeoutPhaseRead}) {
		t.Fatal("timeouts are not local")
	}
	if IsLocalValidationError(errors.New("expected value at line 1 column 2")) {
		t.Fatal("stream parse errors are not local")
	}
	if !IsLocalValidationError(errors.New("list content fed into a str regex helper")) {
		t.Fatal("plain local error")
	}
}

func TestFormatDurationCompactAndResetHint(t *testing.T) {
	if formatDurationCompact(45) != "45s" || formatDurationCompact(780) != "13m" || formatDurationCompact(3600*2+60*5) != "2h 5m" || formatDurationCompact(3600*3) != "3h" || formatDurationCompact(86400*1.5) != "1.5d" {
		t.Fatal("duration compact")
	}
	now := time.Now()
	if hint := ResetHint(&APIError{Message: "resets in 13 minutes"}, now); hint != "~13m" {
		t.Fatalf("reset hint %q", hint)
	}
	if hint := ResetHint(&APIError{Message: "plain"}, now); hint != "" {
		t.Fatalf("no reset hint expected, got %q", hint)
	}
}

func TestEagerFallbackStatusNamesTheFallbackCause(t *testing.T) {
	upstream := &Classified{Reason: ReasonUpstreamRateLimit, Context: map[string]any{"upstream_provider": "deepinfra"}}
	if got := EagerFallbackStatus(upstream, true, false); got != "⚠️ Upstream deepinfra rate-limited — switching to fallback model..." {
		t.Fatalf("upstream = %q", got)
	}
	if got := EagerFallbackStatus(&Classified{Reason: ReasonBilling}, false, false); got != "⚠️ Billing or credits exhausted — switching to fallback provider..." {
		t.Fatalf("billing = %q", got)
	}
	if got := EagerFallbackStatus(&Classified{Reason: ReasonTimeout}, false, true); got != "⚠️ Provider unreachable — switching to fallback provider..." {
		t.Fatalf("transport = %q", got)
	}
	if got := EagerFallbackStatus(&Classified{Reason: ReasonRateLimit}, false, false); got != "⚠️ Rate limited — switching to fallback provider..." {
		t.Fatalf("rate limit = %q", got)
	}
}

func TestNonretryableLabelNamesOnlyLabelledReasons(t *testing.T) {
	if label, ok := NonretryableLabel(ReasonContentPolicyBlocked); !ok || label != "The provider's safety filter refused this request" {
		t.Fatalf("content policy label %q %v", label, ok)
	}
	if label, ok := NonretryableLabel(ReasonUnknown); ok || label != "" {
		t.Fatalf("unknown reason labelled %q", label)
	}
}

func TestZAIPolicyNoteNamesOnlyZAIPolicies(t *testing.T) {
	if got := ZAIPolicyNote("zai_coding_overload_long"); got != " (Z.AI Coding overload adaptive long backoff)" {
		t.Fatalf("long note %q", got)
	}
	if got := ZAIPolicyNote("default"); got != "" {
		t.Fatalf("default note %q", got)
	}
}

func TestLadderReasonAndStopHintExports(t *testing.T) {
	for reason, want := range map[FailoverReason]bool{ReasonOverloaded: true, ReasonServerError: true, ReasonTimeout: true, ReasonAuth: false, ReasonContextOverflow: false} {
		if LadderReason(reason) != want {
			t.Fatalf("%s: want %v", reason, want)
		}
	}
	for platform, want := range map[string]string{"CLI": "press Esc to stop", "api_server": "cancel the request to stop", "cron": "", "telegram": "send /stop to cancel", "": "send /stop to cancel"} {
		if got := LadderStopHint(platform); got != want {
			t.Fatalf("%q: got %q want %q", platform, got, want)
		}
	}
}
