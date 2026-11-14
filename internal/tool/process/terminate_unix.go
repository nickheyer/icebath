//go:build !windows

package process

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/internal/proc"
)

func signalQuietly(pid int) { _ = syscall.Kill(pid, syscall.SIGTERM) }

func waitForExit(targets []proc.Identity, grace time.Duration) {
	if grace <= 0 {
		return
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && slices.ContainsFunc(targets, proc.Identity.Alive) {
		time.Sleep(pollStep)
	}
}

func treeOf(root proc.Identity) []proc.Identity {
	table, err := proc.ReadTable()
	if err != nil {
		return nil
	}
	return table.Descendants(root)
}

func terminateTree(_ context.Context, pid int, grace time.Duration) {
	parent, err := proc.Identify(pid)
	if errors.Is(err, proc.ErrNotFound) {
		return
	}
	if err != nil {
		signalQuietly(pid)
		return
	}
	descendants := treeOf(parent)
	_ = parent.Signal(syscall.SIGTERM)
	waitForExit([]proc.Identity{parent}, grace)
	remaining := descendants
	if grace > 0 {
		remaining = slices.DeleteFunc(slices.Clone(descendants), func(d proc.Identity) bool { return !d.Alive() })
	}
	for _, d := range remaining {
		_ = d.Signal(syscall.SIGTERM)
	}
	if grace <= 0 {
		return
	}
	targets := append([]proc.Identity{parent}, remaining...)
	waitForExit(targets, grace)
	if parent.Alive() {
		for _, d := range treeOf(parent) {
			if !slices.ContainsFunc(targets, func(t proc.Identity) bool { return t.PID == d.PID }) {
				targets = append(targets, d)
			}
		}
	}
	for _, t := range targets {
		if t.Alive() {
			if err := t.Signal(syscall.SIGKILL); err == nil {
				slog.Info("Escalated to SIGKILL after the process ignored SIGTERM within the grace window",
					"pid", t.PID, "grace_seconds", grace.Seconds())
			}
		}
	}
}
