package agent

import (
	"reflect"
	"testing"
)

type statusRecorder struct {
	statuses, warns, prints []string
	attempted               []string
}

func bareStatusAgent(r *statusRecorder) *AIAgent {
	return &AIAgent{
		notificationConfig: map[string]any{},
		printFn:            func(s string) { r.prints = append(r.prints, s) },
		statusCallback: func(kind string, message any) {
			text := statusText(message)
			r.attempted = append(r.attempted, text)
			if kind == "warn" {
				r.warns = append(r.warns, text)
			} else {
				r.statuses = append(r.statuses, text)
			}
		},
	}
}

func bufferedTexts(a *AIAgent) []string {
	out := []string{}
	for _, line := range a.retryStatusBuffer {
		out = append(out, line.kind+":"+statusText(line.message))
	}
	return out
}

func TestBufferStatusAccumulatesThenFlushes(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	a.bufferStatus("⏳ Retrying...")
	a.bufferStatus("⚠️ Fallback...")
	if len(r.statuses) != 0 || !reflect.DeepEqual(bufferedTexts(a), []string{"status:⏳ Retrying...", "status:⚠️ Fallback..."}) {
		t.Fatalf("buffered %v emitted %v", bufferedTexts(a), r.statuses)
	}
	a.flushStatusBuffer()
	if !reflect.DeepEqual(r.statuses, []string{"⏳ Retrying...", "⚠️ Fallback..."}) || len(a.retryStatusBuffer) != 0 {
		t.Fatalf("flushed %v left %v", r.statuses, bufferedTexts(a))
	}
}

func TestClearDropsBufferedMessagesSilently(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	a.bufferStatus("⏳ Retrying...")
	a.bufferStatus("⚠️ Fallback...")
	a.clearStatusBuffer()
	a.flushStatusBuffer()
	if len(r.statuses) != 0 || len(a.retryStatusBuffer) != 0 {
		t.Fatalf("cleared buffer emitted %v", r.statuses)
	}
}

func TestBufferVprintReplaysViaVprintWithLogPrefix(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	a.logPrefix = "[abc] "
	a.bufferVprint("⚠️  API call failed")
	a.flushStatusBuffer()
	if !reflect.DeepEqual(r.prints, []string{"[abc] ⚠️  API call failed"}) {
		t.Fatalf("vprint replay %q", r.prints)
	}
}

func TestFlushEmptyBufferIsNoop(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	a.flushStatusBuffer()
	a.clearStatusBuffer()
	a.flushStatusBuffer()
	if len(r.statuses)+len(r.prints) != 0 {
		t.Fatalf("empty flush emitted %v %v", r.statuses, r.prints)
	}
}

func TestMixedKindsReplayThroughCorrectChannels(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	a.bufferStatus("status-1")
	a.bufferVprint("vprint-1")
	a.bufferRetryMessage("warn", "warn-1")
	a.bufferStatus("status-2")
	a.flushStatusBuffer()
	if !reflect.DeepEqual(r.statuses, []string{"status-1", "status-2"}) || !reflect.DeepEqual(r.warns, []string{"warn-1"}) {
		t.Fatalf("statuses %v warns %v", r.statuses, r.warns)
	}
	if !reflect.DeepEqual(r.prints, []string{"status-1", "vprint-1", "warn-1", "status-2"}) {
		t.Fatalf("prints %v", r.prints)
	}
}

func TestPendingFallbackNoticeEmittedOnceOnSuccess(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	a.bufferStatus("\U0001f504 Primary model failed — switching to fallback: m2 via p2")
	notice := "\U0001f504 Switched to fallback model: m1 via p1 → m2 via p2"
	a.SetPendingFallbackNotice([]string{notice})
	a.emitPendingFallbackNotice()
	a.clearStatusBuffer()
	a.emitPendingFallbackNotice()
	if !reflect.DeepEqual(r.statuses, []string{notice}) || len(a.retryStatusBuffer) != 0 || a.pendingFallbackNotice != nil {
		t.Fatalf("emitted %v buffer %v pending %v", r.statuses, bufferedTexts(a), a.pendingFallbackNotice)
	}
}

func TestPendingFallbackNoticeEmitsAllSwitchesInOrder(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	a.SetPendingFallbackNotice([]string{"primary → fallback-1", "fallback-1 → fallback-2"})
	a.emitPendingFallbackNotice()
	if !reflect.DeepEqual(r.statuses, []string{"primary → fallback-1", "fallback-1 → fallback-2"}) || a.pendingFallbackNotice != nil {
		t.Fatalf("emitted %v", r.statuses)
	}
}

func TestPendingFallbackNoticeContinuesAfterCallbackError(t *testing.T) {
	var attempted []string
	a := &AIAgent{printFn: func(s string) {
		attempted = append(attempted, s)
		if s == "first" {
			panic("surface unavailable")
		}
	}}
	a.statusCallback = func(string, any) { panic("surface unavailable") }
	a.SetPendingFallbackNotice([]string{"first", "second"})
	a.emitPendingFallbackNotice()
	if !reflect.DeepEqual(attempted, []string{"first", "second"}) || a.pendingFallbackNotice != nil {
		t.Fatalf("attempted %v", attempted)
	}
}

func TestFlushDiscardsPendingFallbackNotice(t *testing.T) {
	r := &statusRecorder{}
	a := bareStatusAgent(r)
	line := "\U0001f504 Primary model failed — switching to fallback: m2 via p2"
	a.bufferStatus(line)
	a.SetPendingFallbackNotice([]string{"\U0001f504 Switched to fallback model: m1 via p1 → m2 via p2"})
	a.flushStatusBuffer()
	if !reflect.DeepEqual(r.statuses, []string{line}) || a.pendingFallbackNotice != nil {
		t.Fatalf("flushed %v pending %v", r.statuses, a.pendingFallbackNotice)
	}
	r.statuses = nil
	a.emitPendingFallbackNotice()
	if len(r.statuses) != 0 {
		t.Fatalf("stale notice re-emitted %v", r.statuses)
	}
}

func TestFlushSwallowsCallbackExceptions(t *testing.T) {
	var seen []string
	a := &AIAgent{statusCallback: func(_ string, message any) {
		seen = append(seen, statusText(message))
		panic("simulated callback failure")
	}, printFn: func(string) {}}
	a.bufferStatus("first")
	a.bufferStatus("second")
	a.flushStatusBuffer()
	if !reflect.DeepEqual(seen, []string{"first", "second"}) || len(a.retryStatusBuffer) != 0 {
		t.Fatalf("seen %v buffer %v", seen, bufferedTexts(a))
	}
}
