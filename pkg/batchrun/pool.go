package batchrun

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	consoleutil "github.com/nickheyer/icebath/pkg/utils/console"
	ctxutil "github.com/nickheyer/icebath/pkg/utils/ctx"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// InterruptedError reports a run a signal stopped before it finished
type InterruptedError struct{ Signal os.Signal }

// Error names the interruption
func (e *InterruptedError) Error() string { return "batch run interrupted" }

// ExitCode is the status a shell reports for a process the signal ended
func (e *InterruptedError) ExitCode() int {
	if sig, ok := e.Signal.(syscall.Signal); ok {
		return 128 + int(sig)
	}
	return 130
}

// One batch handed to a worker
type batchTask struct {
	batchNum int
	entries  []indexedEntry
}

// What a worker reports for one batch, the error set when the batch code failed outright
type batchOutcome struct {
	result batchResult
	err    error
}

// Progress row of the run, a live bar on a terminal and one final row on any other writer
type batchProgress struct {
	progress *consoleutil.Progress
	task     int
}

// Starts the batch progress display with a spinner, a bar, the finished count and the time left
func (r *Runner) startProgress(total int) *batchProgress {
	console := consoleutil.NewConsole(r.out)
	digits := len(strconv.Itoa(total))
	finished := consoleutil.TaskColumn(func(t consoleutil.Task) string { return fmt.Sprintf("[green]%*d/%d", digits, t.Completed, t.Total) })
	progress := console.NewProgress(500*time.Millisecond, &consoleutil.SpinnerColumn{}, consoleutil.TextColumn("[bold blue]📦 Batches"),
		consoleutil.BarColumn{Width: 40}, finished, consoleutil.TextColumn("•"), consoleutil.TimeRemainingColumn{})
	progress.Start()
	return &batchProgress{progress: progress, task: progress.AddTask("Processing", total)}
}

// Worker context with its own context variables and thread locals, as a worker process starts with its own copies
func workerContext(ctx context.Context) context.Context {
	return tools.WithThreadLocals(ctxutil.Fork(ctx))
}

// Runs batches from tasks one after another until the queue closes, the run stops or a batch fails
func (r *Runner) work(ctx context.Context, seed int64, tasks <-chan batchTask, outcomes chan<- batchOutcome, completed map[int]bool) {
	w := &workerState{rng: rand.New(rand.NewSource(seed))}
	ctx = workerContext(ctx)
	for task := range tasks {
		result, err := r.processBatchSafely(ctx, w, task.batchNum, task.entries, completed)
		outcomes <- batchOutcome{result: result, err: err}
		if err != nil {
			return
		}
	}
}

// Stops every worker and waits for them to return, a second signal ending the wait
func (r *Runner) stopWorkers(cancel context.CancelCauseFunc, cause error, done <-chan struct{}, signals <-chan os.Signal) {
	r.agents.interruptAll("the batch run is stopping")
	cancel(cause)
	select {
	case <-done:
	case <-signals:
	}
}

// Processes every batch in a bounded pool of workers, checkpointing after each finished batch
func (r *Runner) runPool(ctx context.Context, checkpoint *jsonutil.Object, completed map[int]bool) ([]batchResult, error) {
	r.printf("\n🔧 Initializing %d workers...\n", r.o.NumWorkers)
	if r.o.NumWorkers < 1 {
		return nil, errors.New("Number of workers must be at least 1")
	}
	r.printf("✅ Created %d batch tasks\n", len(r.batches))
	r.printf("🚀 Starting parallel batch processing...\n\n")

	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	skip := make(map[int]bool, len(completed))
	for index := range completed {
		skip[index] = true
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	tasks := make(chan batchTask)
	outcomes := make(chan batchOutcome, len(r.batches))
	var workers sync.WaitGroup
	seed := time.Now().UnixNano()
	for n := 0; n < r.o.NumWorkers; n++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			r.work(runCtx, seed+int64(n)*7919, tasks, outcomes, skip)
		}()
	}
	go func() {
		defer close(tasks)
		for batchNum, entries := range r.batches {
			select {
			case tasks <- batchTask{batchNum: batchNum, entries: entries}:
			case <-runCtx.Done():
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()

	display := r.startProgress(len(r.batches))
	defer display.progress.Stop()
	var results []batchResult
	for len(results) < len(r.batches) {
		select {
		case outcome := <-outcomes:
			if isStopped(outcome.err) {
				continue
			}
			if outcome.err != nil {
				pkgLog().Error(fmt.Sprintf("Batch worker failed: %s", outcome.err))
				var panicked *promptPanic
				if r.o.Verbose && errors.As(outcome.err, &panicked) {
					fmt.Fprintf(os.Stderr, "%s\n", panicked.stack)
				}
				r.stopWorkers(cancel, outcome.err, done, signals)
				return nil, outcome.err
			}
			results = append(results, outcome.result)
			display.progress.Advance(display.task, 1)
			if err := r.checkpointBatch(ctx, checkpoint, completed, outcome.result); err != nil {
				r.printf("⚠️  Warning: Failed to save incremental checkpoint: %s\n", err.Error())
			}
		case sig := <-signals:
			r.printf("\n⚠️  Interrupted — terminating batch workers...\n")
			r.stopWorkers(cancel, tools.ErrKeyboardInterrupt, done, signals)
			return nil, &InterruptedError{Signal: sig}
		case <-ctx.Done():
			r.stopWorkers(cancel, context.Cause(ctx), done, signals)
			return nil, context.Cause(ctx)
		}
	}
	return results, nil
}
