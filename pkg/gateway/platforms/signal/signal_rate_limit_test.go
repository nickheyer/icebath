package signal

import (
	"context"
	"errors"
	"math"
	"testing"
)

func patchSleepAndTime(t *testing.T) *[]float64 {
	var sleeps []float64
	offset := 0.0
	oldSleep, oldMono := signalSleep, monotonic
	signalSleep = func(ctx context.Context, seconds float64) error {
		sleeps = append(sleeps, seconds)
		offset += seconds
		return nil
	}
	monotonic = func() float64 { return offset }
	t.Cleanup(func() { signalSleep, monotonic = oldSleep, oldMono; resetScheduler() })
	resetScheduler()
	return &sleeps
}

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-6*math.Max(1, math.Abs(b)) }

func newDefaultScheduler() *SignalAttachmentScheduler {
	return NewSignalAttachmentScheduler(SignalRateLimitBucketCapacity, SignalRateLimitDefaultRetryAfter)
}

func TestEstimateWaitProportionalToDeficitWhenEmpty(t *testing.T) {
	patchSleepAndTime(t)
	s := newDefaultScheduler()
	s.Tokens = 0
	if !approx(s.EstimateWait(32), 32/s.RefillRate) || !approx(s.EstimateWait(1), 1/s.RefillRate) {
		t.Fatal(s.EstimateWait(32))
	}
}

func TestAcquireWithinCapacityNoSleep(t *testing.T) {
	sleeps := patchSleepAndTime(t)
	s := newDefaultScheduler()
	wait, err := s.Acquire(context.Background(), 10)
	s.ReportRPCDuration(0.001, 10)
	if err != nil || wait != 0 || len(*sleeps) != 0 || s.Tokens != s.Capacity-10 {
		t.Fatal(wait, err, *sleeps, s.Tokens)
	}
}

func TestAcquireWhenEmptySleepsForDeficit(t *testing.T) {
	sleeps := patchSleepAndTime(t)
	s := newDefaultScheduler()
	s.Tokens = 0
	wait, err := s.Acquire(context.Background(), 32)
	s.ReportRPCDuration(1e-12, 32)
	expected := 32 / s.RefillRate
	if err != nil || !approx(wait, expected) || len(*sleeps) != 1 || !approx((*sleeps)[0], expected) || !approx(s.Tokens, 0) {
		t.Fatal(wait, err, *sleeps, s.Tokens)
	}
	var se *SignalSchedulerError
	if _, err := s.Acquire(context.Background(), 51); !errors.As(err, &se) || err.Error() != "Signal scheduler was called requesting 51 tokens (max is 50.0)" {
		t.Fatal(err)
	}
}

func TestFeedbackCalibratesRefillRateFromRetryAfter(t *testing.T) {
	s := newDefaultScheduler()
	original := s.RefillRate
	ra := 42.0
	s.Feedback(&ra, 1)
	if !approx(s.RefillRate, 1.0/42.0) || s.RefillRate == original || s.Tokens != 0 {
		t.Fatal(s.RefillRate)
	}
}

func TestRefillDoesNotExceedCapacity(t *testing.T) {
	patchSleepAndTime(t)
	s := newDefaultScheduler()
	s.Tokens = 0
	last := s.LastRefill
	monotonic = func() float64 { return last + 365*24*3600 }
	s.refill()
	if s.Tokens != s.Capacity {
		t.Fatal(s.Tokens)
	}
}

func TestConcurrentAcquiresSerialize(t *testing.T) {
	patchSleepAndTime(t)
	s := GetScheduler()
	type result struct {
		label string
		wait  float64
	}
	var results []result
	worker := func(label string) {
		wait, _ := s.Acquire(context.Background(), int(s.Capacity))
		s.ReportRPCDuration(1e-12, int(s.Capacity))
		results = append(results, result{label, wait})
	}
	done := make(chan struct{})
	go func() { worker("A"); worker("B"); close(done) }()
	<-done
	if len(results) != 2 || results[0].label != "A" || results[1].label != "B" || results[0].wait != 0 || !approx(results[1].wait, s.Capacity/s.RefillRate) {
		t.Fatalf("%+v", results)
	}
}

func TestRateLimitErrorClassificationAndRetryAfter(t *testing.T) {
	typed := map[string]any{"code": float64(-5), "message": "x", "data": map[string]any{"response": map[string]any{"results": []any{
		map[string]any{"retryAfterSeconds": 3}, map[string]any{"retryAfterSeconds": "7"}, "skip", map[string]any{"retryAfterSeconds": 0}}}}}
	if !isSignalRateLimitError(typed) {
		t.Fatal("typed code not detected")
	}
	if v, ok, err := extractRetryAfterSeconds(typed); err != nil || !ok || v != 7 {
		t.Fatal(v, ok, err)
	}
	leaked := map[string]any{"message": "AttachmentInvalidException: RetryLaterException: Retry after 4.5 seconds"}
	if v, ok, _ := extractRetryAfterSeconds(leaked); !isSignalRateLimitError(leaked) || !ok || v != 4.5 {
		t.Fatal(v, ok)
	}
	if isSignalRateLimitError(errors.New("network down")) || !isSignalRateLimitError(errors.New("Failed [429] Too Many")) {
		t.Fatal("string classification wrong")
	}
	if _, ok, err := extractRetryAfterSeconds(map[string]any{"data": map[string]any{"response": map[string]any{"results": true}}}); ok || err == nil {
		t.Fatal("non iterable results did not raise")
	}
	if _, _, err := extractRetryAfterSeconds(map[string]any{"data": "x"}); err == nil {
		t.Fatal("string data did not raise")
	}
	if signalSendTimeout(0) != 30 || signalSendTimeout(3) != 60 || signalSendTimeout(20) != 100 {
		t.Fatal("send timeout")
	}
	if st := GetScheduler().State(); st.Keys[0] != "tokens" || st.Vals["capacity"] != 50 || st.Vals["refill_seconds_per_token"] != 4.0 {
		t.Fatalf("%+v", st)
	}
	t.Setenv("ICEBATH_HOME", t.TempDir())
	if a, b := formatWait(context.Background(), 30), formatWait(context.Background(), 600); a == "" || b == "" || a == b || a == "platform.signal.wait_seconds" {
		t.Fatal(a, b)
	}
}
