package cron

import (
	"context"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/monitoring"
)

// Cron readers of the monitoring health snapshot over the active profile store
func init() {
	active := func() *Store { return openCronStore(home.Dir(context.Background()), time.Now) }
	age := func(read func(*Store) (float64, bool)) func() (*float64, error) {
		return func() (*float64, error) {
			if v, ok := read(active()); ok {
				return &v, nil
			}
			return nil, nil
		}
	}
	monitoring.SetCronHealthSources(monitoring.CronHealthSources{
		Now: func() time.Time { return active().clk.now() },
		LoadJobs: func(ctx context.Context) ([]monitoring.CronJobHealth, error) {
			s := active()
			jobs, err := s.LoadJobs(home.WithOverride(ctx, s.home))
			if err != nil {
				return nil, err
			}
			out := make([]monitoring.CronJobHealth, 0, len(jobs))
			for _, job := range jobs {
				var next any
				if job.NextRunAt != nil {
					next = *job.NextRunAt
				}
				out = append(out, monitoring.CronJobHealth{Enabled: job.Enabled, NextRunAt: next,
					HasSchedule: job.Schedule != nil, GraceSeconds: func() int { return s.jobGraceSeconds(job) }})
			}
			return out, nil
		},
		TickerHeartbeatAge:     age((*Store).TickerHeartbeatAge),
		TickerSuccessAge:       age((*Store).TickerSuccessAge),
		CatchUpOccurrenceCount: func() (int, error) { return active().CatchUpOccurrenceCount(), nil },
		RunningJobCount:        func() (int, error) { return len(getRunningJobIDs()), nil },
		DegradedSkippedRuns: func() ([]int, error) {
			records := DegradedRecords()
			runs := make([]int, len(records))
			for i, r := range records {
				runs[i] = r.SkippedRuns()
			}
			return runs, nil
		},
	})
}
