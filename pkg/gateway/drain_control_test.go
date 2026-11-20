package gateway

import (
	"errors"
	"strings"
	"testing"
)

func TestDrainEpochEmptyWhenProcUnreadable(t *testing.T) {
	prev := procReadFile
	procReadFile = func(name string) ([]byte, error) {
		if strings.HasPrefix(name, "/proc/") {
			return nil, errors.New("no /proc")
		}
		return prev(name)
	}
	instantiationEpochCacheClear()
	defer func() { procReadFile = prev; instantiationEpochCacheClear() }()
	if got := CurrentInstantiationEpoch(); got != "" {
		t.Fatalf("epoch %q", got)
	}
}

type fakeDrainRunner struct {
	descriptor        map[string]any
	draining, running bool
}

func (r *fakeDrainRunner) SessionRuntimeDescriptor() map[string]any { return r.descriptor }
func (r *fakeDrainRunner) Draining() bool                           { return r.draining }
func (r *fakeDrainRunner) Running() bool                            { return r.running }

func TestRestartWaitGatesVerbs(t *testing.T) {
	r := &fakeDrainRunner{descriptor: map[string]any{"drain_reason": "operator"}, draining: true, running: true}
	OpenRestartWait(r)
	if RestartWaitOpen(r) {
		t.Fatal("restart wait overrode a named drain")
	}
	r.descriptor["drain_reason"] = ""
	OpenRestartWait(r)
	if !RestartWaitOpen(r) {
		t.Fatal("restart wait not open")
	}
	if WSMethodRefusedWhileDraining(r, "prompt.submit") || !WSMethodRefusedWhileDraining(r, "config.set") {
		t.Fatal("ws verb gate")
	}
	r.running = false
	if RestartWaitOpen(r) {
		t.Fatal("stopping runner still in restart wait")
	}
}

// Drops the cached instantiation epoch
func instantiationEpochCacheClear() {
	epochMu.Lock()
	epochCached = nil
	epochMu.Unlock()
}
