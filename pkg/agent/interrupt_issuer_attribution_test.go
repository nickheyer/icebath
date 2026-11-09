package agent

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pythread"
)

// Agent built the way hermes _bare_agent seeds the interrupt slots without __init__
func bareIssuerAgent() *AIAgent {
	return &AIAgent{hardInterruptRequested: &pythread.Event{}, activeChildrenLock: &sync.Mutex{}, quietMode: true}
}

func TestSystemProducerIsRecordedAndLoggedAtPublication(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)
	a := bareIssuerAgent()
	a.HardInterrupt("Cron job timed out (inactivity)", "cron inactivity watchdog")
	if !a.interruptRequested.Load() {
		t.Fatal("interrupt not requested")
	}
	if got := interruptIssuer(a); got != "cron_inactivity_watchdog" {
		t.Fatalf("issuer %q", got)
	}
	published := 0
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "msg=\"Interrupt requested") {
			published++
			if !strings.Contains(line, "cron inactivity watchdog") {
				t.Fatalf("published line %q", line)
			}
		}
	}
	if published != 1 {
		t.Fatalf("published %d lines: %s", published, logs.String())
	}
}

func TestHumanStopsHaveNoSystemIssuer(t *testing.T) {
	a := bareIssuerAgent()
	a.Interrupt("")
	if got := interruptIssuer(a); got != "" {
		t.Fatalf("plain interrupt issuer %q", got)
	}
	a.clearInterrupt(false)
	a.HardInterrupt("", "")
	if !a.interruptRequested.Load() {
		t.Fatal("hard stop not requested")
	}
	if got := interruptIssuer(a); got != "" {
		t.Fatalf("reasonless hard stop issuer %q", got)
	}
}

func TestRequeuePendingSteerAppendsToExistingSteer(t *testing.T) {
	a := &AIAgent{pendingSteerLock: &sync.Mutex{}, pendingSteer: "old"}
	requeuePendingSteer(a, "new")
	if a.pendingSteer != "old\nnew" {
		t.Fatalf("steer %q", a.pendingSteer)
	}
}

func TestSteerAndDrainRoundtripWithLock(t *testing.T) {
	a := &AIAgent{pendingSteerLock: &sync.Mutex{}}
	if !a.Steer("a") || !a.Steer("b") {
		t.Fatal("steer refused")
	}
	if got := a.drainPendingSteer(); got != "a\nb" {
		t.Fatalf("drained %q", got)
	}
	if a.pendingSteer != "" {
		t.Fatalf("left %q", a.pendingSteer)
	}
}
