package mcp

import (
	"context"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestConfigWriteScopeWaitsOnTheLockTheTUIConfigWritesHold(t *testing.T) {
	_, release := provider.LockConfigMutation(context.Background())
	entered := make(chan struct{})
	go func() {
		_, done := ConfigWriteScope(context.Background(), "")
		close(entered)
		done()
	}()
	select {
	case <-entered:
		release()
		t.Fatal("MCP enable scope entered while a TUI config write held the process config mutation lock")
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP enable scope never entered after the TUI config write released the lock")
	}
}

func TestConfigWriteScopeReentersALockTheCallerAlreadyHolds(t *testing.T) {
	held, release := provider.LockConfigMutation(context.Background())
	defer release()
	entered := make(chan struct{})
	go func() {
		_, done := ConfigWriteScope(held, "")
		close(entered)
		done()
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("a scope nested in a held config mutation lock deadlocked")
	}
}

func TestConfigWriteScopeHoldsOffTheTUIConfigWrites(t *testing.T) {
	_, done := ConfigWriteScope(context.Background(), "")
	entered := make(chan struct{})
	go func() {
		_, release := provider.LockConfigMutation(context.Background())
		close(entered)
		release()
	}()
	select {
	case <-entered:
		done()
		t.Fatal("a TUI config write entered while the MCP enable scope was open")
	case <-time.After(200 * time.Millisecond):
	}
	done()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the TUI config write never entered after the MCP enable scope closed")
	}
}
