package agent

import (
	"context"
	"testing"
)

func TestLifetimePromptCounterIsNotASizeEstimate(t *testing.T) {
	engine := newStubEngine(200_000, 0.5)
	engine.LastPromptTokens = 0
	a := &AIAgent{contextCompressor: engine, sessionPromptTokens: 16_282_033}
	if estimate, ok := a.switchEstimateTokens(context.Background(), nil); ok {
		t.Fatalf("lifetime prompt counter read as a size estimate %d", estimate)
	}
}
