package cron

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/periodic"
)

// ScheduledHandle is the cancel token for one scheduled periodic callback
type ScheduledHandle = periodic.ScheduledHandle

// PeriodicScheduler runs periodic callbacks on one scheduler goroutine
type PeriodicScheduler = periodic.PeriodicScheduler

// Scheduler with its own goroutine
func NewPeriodicScheduler() *PeriodicScheduler { return periodic.NewPeriodicScheduler() }

var periodicDefault = NewPeriodicScheduler()

// Runs fn every interval on the scheduler cron jobs share
func SchedulePeriodic(ctx context.Context, fn func(ctx context.Context) bool, interval time.Duration) *ScheduledHandle {
	return periodicDefault.Schedule(ctx, fn, interval)
}
