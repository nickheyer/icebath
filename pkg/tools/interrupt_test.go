package tools

import (
	"context"
	"testing"
)

func TestInterruptSetAndCheck(t *testing.T) {
	id := NewWorkerID()
	ctx := WithWorker(context.Background(), id)
	SetInterrupt(id, false, "")
	if IsInterrupted(ctx) {
		t.Fatal("fresh worker interrupted")
	}
	SetInterrupt(id, true, "user interrupt")
	if !IsInterrupted(ctx) || InterruptReason(ctx) != "user interrupt" {
		t.Fatal("interrupt not visible")
	}
	SetInterrupt(id, false, "")
	if IsInterrupted(ctx) || InterruptReason(ctx) != "" {
		t.Fatal("clear failed")
	}
}

func TestIsWorkerInterruptedChecksTargetNotCaller(t *testing.T) {
	me, other := NewWorkerID(), NewWorkerID()
	ctx := WithWorker(context.Background(), me)
	SetInterrupt(other, true, "")
	if IsInterrupted(ctx) || !IsWorkerInterrupted(other) || IsWorkerInterrupted(0) {
		t.Fatal("target check")
	}
	SetInterrupt(other, false, "")
	if IsWorkerInterrupted(other) {
		t.Fatal("clear")
	}
	acting := WithActingFor(ctx, other)
	SetInterrupt(other, true, "")
	if !IsInterrupted(acting) {
		t.Fatal("acting for ignored")
	}
	SetInterrupt(other, false, "")
}

func TestClearCurrentInterruptLeavesOthers(t *testing.T) {
	me, other := NewWorkerID(), NewWorkerID()
	ctx := WithWorker(context.Background(), me)
	SetInterrupt(other, true, "")
	SetInterrupt(me, true, "")
	ClearCurrentInterrupt(ctx)
	if IsInterrupted(ctx) || !IsWorkerInterrupted(other) {
		t.Fatal("clear touched the wrong worker")
	}
	SetInterrupt(other, false, "")
}

func TestRunIfNotInterrupted(t *testing.T) {
	id := NewWorkerID()
	ctx := WithWorker(context.Background(), id)
	SetInterrupt(id, true, "")
	ran := false
	if RunIfNotInterrupted(ctx, func() { ran = true }) || ran {
		t.Fatal("callback ran while interrupted")
	}
	SetInterrupt(id, false, "")
	if !RunIfNotInterrupted(ctx, func() { ran = true }) || !ran {
		t.Fatal("callback skipped")
	}
}

func TestYieldRequests(t *testing.T) {
	id := NewWorkerID()
	RequestYield(id)
	if !IsWorkerYieldRequested(id) || !ConsumeYield(id) || ConsumeYield(id) || IsWorkerYieldRequested(0) {
		t.Fatal("yield state")
	}
	RequestYield(id)
	SetInterrupt(id, false, "")
	if IsWorkerYieldRequested(id) {
		t.Fatal("clearing the interrupt kept the yield")
	}
}
