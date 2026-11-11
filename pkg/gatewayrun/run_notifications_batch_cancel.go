package gatewayrun

import "context"

// Settles every waiting future of the batch entries with result
func settleBatchWaiters(entries []completionBatchItem, result bool) {
	for _, entry := range entries {
		if entry.future != nil && !entry.future.isDone() {
			entry.future.settle(result, nil)
		}
	}
}

// Settles pending completion batches before adapter teardown, cancelling their flush tasks
func (r *GatewayRunner) cancelProcessCompletionBatchTasks(ctx context.Context) {
	r.completionNotificationBatchesStopping = true
	var tasks []*gatewayTask
	for task := range r.completionNotificationBatchFlushTasks {
		if !task.isDone() {
			tasks = append(tasks, task)
		}
	}
	for _, task := range tasks {
		task.Cancel()
	}
	for _, task := range tasks {
		if _, err := task.wait(ctx); err != nil && ctx.Err() != nil {
			return
		}
	}
	for _, entries := range r.completionNotificationBatches {
		settleBatchWaiters(entries, false)
	}
	clear(r.completionNotificationBatches)
	clear(r.completionNotificationBatchTasks)
	clear(r.completionNotificationBatchFlushTasks)
}
