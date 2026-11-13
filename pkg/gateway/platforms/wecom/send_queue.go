package wecom

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Per chat token bucket of WeCom's 30 messages a minute, split into a normal and a reserved control pool
const (
	bucketNormalTokens   = 24
	bucketReservedTokens = 6
	bucketWindow         = time.Minute
)

// Error every send still queued or in flight receives once the adapter shuts its queue down
var errQueueShutdown = errors.New("WeCom adapter shutting down")

type tokenUsage struct {
	normal, reserved int
	windowStart      time.Time
}

type sendOutcome struct {
	value any
	err   error
}

type sendJob struct {
	send   func(ctx context.Context) (any, error)
	result chan sendOutcome
}

// One FIFO lane of a chat drained by its own worker
type sendLane struct {
	mu     sync.Mutex
	jobs   []sendJob
	notify chan struct{}
}

// Two FIFO lanes per chat under a per minute token bucket, the control lane drawing on a reserved pool
type ChatSendQueue struct {
	name         string
	mu           sync.Mutex
	chatLanes    map[string]*sendLane
	controlLanes map[string]*sendLane
	usage        map[string]*tokenUsage
	ctx          context.Context
	cancel       context.CancelFunc
}

// Send queue whose workers stop when the adapter shuts it down
func NewChatSendQueue(name string) *ChatSendQueue {
	ctx, cancel := context.WithCancel(context.Background())
	return &ChatSendQueue{name: name, chatLanes: map[string]*sendLane{}, controlLanes: map[string]*sendLane{},
		usage: map[string]*tokenUsage{}, ctx: ctx, cancel: cancel}
}

// Consumes one token returning zero, else the wait until the minute window resets, control spilling into the reserve
func (q *ChatSendQueue) bucketTryConsume(chatID string, isControl bool) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	u, ok := q.usage[chatID]
	if !ok {
		u = &tokenUsage{windowStart: now}
		q.usage[chatID] = u
	}
	if now.Sub(u.windowStart) > bucketWindow {
		u.normal, u.reserved, u.windowStart = 0, 0, now
	}
	if u.normal < bucketNormalTokens {
		u.normal++
		return 0
	}
	if isControl && u.reserved < bucketReservedTokens {
		u.reserved++
		return 0
	}
	return bucketWindow - now.Sub(u.windowStart)
}

// Queues a send for the chat and waits for its result, FIFO per chat and lane
func (q *ChatSendQueue) EnqueueChatSend(ctx context.Context, chatID string, send func(ctx context.Context) (any, error), isControl bool) (any, error) {
	if q.ctx.Err() != nil {
		return nil, errQueueShutdown
	}
	key := strings.TrimSpace(chatID)
	laneName := "normal"
	q.mu.Lock()
	lanes := q.chatLanes
	if isControl {
		lanes, laneName = q.controlLanes, "control"
	}
	l, ok := lanes[key]
	if !ok {
		slog.Debug(fmt.Sprintf("[%s] Creating %s queue + worker for chat %s", q.name, laneName, key))
		l = &sendLane{notify: make(chan struct{}, 1)}
		lanes[key] = l
		go q.sendWorker(key, l, isControl)
	}
	q.mu.Unlock()
	job := sendJob{send: send, result: make(chan sendOutcome, 1)}
	l.mu.Lock()
	slog.Debug(fmt.Sprintf("[%s] Enqueuing send for chat %s (lane=%s, qsize=%d)", q.name, key, laneName, len(l.jobs)))
	l.jobs = append(l.jobs, job)
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
	select {
	case r := <-job.result:
		return r.value, r.err
	case <-q.ctx.Done():
		return nil, errQueueShutdown
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Next queued job, false once the queue shuts down
func (q *ChatSendQueue) nextJob(l *sendLane) (sendJob, bool) {
	for {
		if q.ctx.Err() != nil {
			return sendJob{}, false
		}
		l.mu.Lock()
		if len(l.jobs) > 0 {
			job := l.jobs[0]
			l.jobs = l.jobs[1:]
			l.mu.Unlock()
			return job, true
		}
		l.mu.Unlock()
		select {
		case <-l.notify:
		case <-q.ctx.Done():
			return sendJob{}, false
		}
	}
}

// Drains one lane under the token bucket, a shutdown failing the send in flight and every queued one
func (q *ChatSendQueue) sendWorker(chatKey string, l *sendLane, isControl bool) {
	if !isControl {
		slog.Debug(fmt.Sprintf("[%s] Normal send worker started for chat %s", q.name, chatKey))
	}
	defer func() {
		l.mu.Lock()
		pending := l.jobs
		l.jobs = nil
		l.mu.Unlock()
		for _, job := range pending {
			job.result <- sendOutcome{err: errQueueShutdown}
		}
	}()
	for {
		job, ok := q.nextJob(l)
		if !ok {
			return
		}
		if wait := q.bucketTryConsume(chatKey, isControl); wait > 0 {
			if !isControl {
				slog.Debug(fmt.Sprintf("[%s] Normal worker rate-limited for chat %s, waiting %.1fs", q.name, chatKey, wait.Seconds()))
			}
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-q.ctx.Done():
				timer.Stop()
				job.result <- sendOutcome{err: errQueueShutdown}
				return
			}
			q.bucketTryConsume(chatKey, isControl)
		}
		value, err := job.send(q.ctx)
		if q.ctx.Err() != nil {
			job.result <- sendOutcome{err: errQueueShutdown}
			return
		}
		job.result <- sendOutcome{value, err}
	}
}

// Stops every worker, failing the sends still queued or in flight
func (q *ChatSendQueue) Shutdown() { q.cancel() }
