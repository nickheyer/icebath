package terminal

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// How often a lingering one-shot run looks at the jobs it waits for
const lingerPoll = time.Second

// Waits, until the deadline or the end of the context, for the session's jobs that will tell it of their end and have not, then hands their
// completions to the session at once rather than after the batch window, so a one-shot run answers them before it ends. Answers the jobs
// it waited for and those still running when it stopped
func (r *Jobs) Linger(ctx context.Context, session string, deadline time.Time) (waited, running []string) {
	var pending []*Job
	r.mu.Lock()
	for _, table := range []map[string]*Job{r.running, r.finished} {
		for _, j := range table {
			j.mu.Lock()
			notify := j.notify
			j.mu.Unlock()
			if notify && j.Session == session && !j.completed.isSet() {
				pending = append(pending, j)
			}
		}
	}
	r.mu.Unlock()
	limit := time.Until(deadline)
	if len(pending) == 0 || limit <= 0 {
		return nil, nil
	}
	for _, j := range pending {
		waited = append(waited, j.ID)
	}
	slog.Info(fmt.Sprintf("One-shot exit lingering (bounded %gs) for %d notify_on_complete background process(es): %s", limit.Seconds(),
		len(pending), strings.Join(waited, ", ")))
	stopped := false
	for _, j := range pending {
		for !j.completed.isSet() && !stopped {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				break
			}
			if ctx.Err() != nil {
				stopped = true
				break
			}
			r.reconcile(j)
			if r.refresh(j) == nil || j.completed.isSet() {
				break
			}
			j.completed.wait(min(remaining, lingerPoll))
		}
		if !j.completed.isSet() {
			running = append(running, j.ID)
		}
	}
	if len(running) > 0 {
		slog.Warn(fmt.Sprintf("One-shot exit linger timed out after %gs with %d background process(es) still running: %s — they may be killed when "+
			"this process exits.", limit.Seconds(), len(running), strings.Join(running, ", ")))
	}
	r.flush(session)
	return waited, running
}
