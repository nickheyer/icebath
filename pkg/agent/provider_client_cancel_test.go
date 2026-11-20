package agent

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// Child mode standing in for the real provider process the cancel test terminates
func dispatchProviderClientSleepChild() {
	if os.Getenv("ICEBATH_TEST_PROVIDER_CLIENT_SLEEP") != "1" {
		return
	}
	time.Sleep(60 * time.Second)
	os.Exit(0)
}

// Counts the socket sweeps the abort path runs instead of closing sockets
func countSocketSweeps(t *testing.T) *int {
	t.Helper()
	sweeps := 0
	prev := forceCloseTCPSockets
	forceCloseTCPSockets = func(client any) int {
		sweeps++
		return 0
	}
	t.Cleanup(func() { forceCloseTCPSockets = prev })
	return &sweeps
}

// Request owned non HTTP transport whose cancel terminates its provider process
type cancelingProcessClient struct{ cmd *exec.Cmd }

func (c cancelingProcessClient) Cancel() { _ = terminateProcess(c.cmd.Process) }

// Client answering a cancel shaped lookup that is not the cancel contract
type dynamicAttributeClient struct{ calls *[]string }

func (c dynamicAttributeClient) Cancel(reason string) { *c.calls = append(*c.calls, "cancel:"+reason) }

func TestCrossThreadCancelStopsRealProviderProcess(t *testing.T) {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "ICEBATH_TEST_PROVIDER_CLIENT_SLEEP=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
		}
	})
	sweeps := countSocketSweeps(t)
	a := &AIAgent{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.abortRequestOpenAIClient(cancelingProcessClient{cmd}, "interrupt_abort")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("abort from another goroutine did not return within 2s")
	}
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("provider process still alive 2s after cancel")
	}
	if *sweeps != 0 {
		t.Fatalf("socket sweeps %d, want none for a cancelable transport", *sweeps)
	}
}

func TestDynamicAttributesDoNotOptHttpClientsIntoCancel(t *testing.T) {
	sweeps := countSocketSweeps(t)
	var calls []string
	a := &AIAgent{}
	a.abortRequestOpenAIClient(dynamicAttributeClient{&calls}, "interrupt_abort")
	if *sweeps != 1 {
		t.Fatalf("socket sweeps %d, want one", *sweeps)
	}
	if len(calls) != 0 {
		t.Fatalf("cancel shaped method called: %v", calls)
	}
}
