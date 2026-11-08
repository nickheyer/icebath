package gateway

import "context"

// Owner work the authority tracks so retirement can join it, a mutation or a ledger write
type trackedTask struct {
	done   chan struct{}
	result any
	err    error
}

// Result of the task once it finished
func (t *trackedTask) wait() (any, error) {
	<-t.done
	return t.result, t.err
}

// Republishes the runner's in flight count once owner work it counts ends
func persistWorkCount(a *SessionAuthority) {
	if persist, ok := a.Runner.(activeAgentsPersister); ok {
		persist.PersistActiveAgents()
	}
}

// Unfinished mutation tasks of the authority
func mutationTasks(a *SessionAuthority) []*trackedTask {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*trackedTask
	for task := range a.mutationTasks {
		select {
		case <-task.done:
		default:
			out = append(out, task)
		}
	}
	return out
}

// Runs an operation as a tracked task the caller cannot cancel apart from its commit
func trackMutation(a *SessionAuthority, operation func(ctx context.Context) (any, error)) *trackedTask {
	task := &trackedTask{done: make(chan struct{})}
	a.mu.Lock()
	a.mutationTasks[task] = true
	a.mu.Unlock()
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.mutationTasks, task)
			a.mu.Unlock()
			close(task.done)
			persistWorkCount(a)
		}()
		task.result, task.err = operation(context.Background())
	}()
	return task
}

// Runs a ledger write and follow up off the owner loop as one tracked task, ordered writes keep call order
func trackedWrite(ctx context.Context, a *SessionAuthority, write func(ctx context.Context) (any, error),
	then func(ctx context.Context, result any) (any, error), ordered bool, after func(result any)) (any, error) {
	task := trackMutation(a, func(taskCtx context.Context) (any, error) {
		var result any
		var err error
		if ordered {
			a.orderedWrites.Lock()
			result, err = write(taskCtx)
			a.orderedWrites.Unlock()
		} else {
			result, err = write(taskCtx)
		}
		if err != nil || then == nil {
			return result, err
		}
		return then(taskCtx, result)
	})
	if after == nil {
		select {
		case <-task.done:
			return task.result, task.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	select {
	case <-task.done:
	case <-ctx.Done():
		go func() {
			if result, err := task.wait(); err == nil {
				after(result)
			}
		}()
		return nil, ctx.Err()
	}
	if task.err != nil {
		return nil, task.err
	}
	after(task.result)
	return task.result, nil
}
