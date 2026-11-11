package cron

import "context"

// Keyword arguments a caller outside the package passes to the cronjob tool, nil standing for an omitted value
type CronjobCall struct {
	Action, JobID, Name, Schedule, Prompt, Repeat, Deliver, Continuity any
	IncludeDisabled                                                    bool
}

// Runs the cronjob tool against the store of the bound home and answers its JSON text
func Cronjob(ctx context.Context, c CronjobCall) string {
	a := &cronjobArgs{Action: c.Action, JobID: c.JobID, Name: c.Name, Schedule: c.Schedule, Prompt: c.Prompt,
		Repeat: c.Repeat, Deliver: c.Deliver, Continuity: c.Continuity, IncludeDisabled: c.IncludeDisabled, Paused: false}
	return cronToolStore(ctx, nil).cronjob(ctx, a)
}
