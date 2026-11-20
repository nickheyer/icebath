package cron

import "context"

// Runs the job through the home owner and returns success, output document, final answer and error text
func RunCanonicalJob(ctx context.Context, job *Job, extraPrompt *string, cancel CancelSource, executionID *string) (bool, string, string, *string, error) {
	out, err := cronStoreFrom(ctx).runCanonicalJob(ctx, job, extraPrompt, cancel, executionID)
	return out.Success, out.Output, out.FinalResponse, out.Error, err
}
