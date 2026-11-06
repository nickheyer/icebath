package wecom

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	bucketMaxTokens      = 30
	bucketNormalTokens   = 24
	bucketReservedTokens = 6
)

var processStart = time.Now()

// Monotonic seconds and context aware sleep, swappable in tests
var (
	monotonic = func() float64 { return time.Since(processStart).Seconds() }
	sendSleep = func(ctx context.Context, seconds float64) error {
		timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

type tokenUsage struct{ normal, reserved, lastReset float64 }

type sendResult struct {
	value any
	err   error
}

type sendJob struct {
	send   func(ctx context.Context) (any, error)
	result chan sendResult
}

// One FIFO lane of a chat drained by its own worker
type sendLane struct {
	mu     sync.Mutex
	jobs   []sendJob
	notify chan struct{}
}

// Two FIFO lanes per chat under a per minute token bucket, the control lane drawing on a reserved pool
type ChatSendQueue struct {
	Name          string
	mu            sync.Mutex
	chatQueues    map[string]*sendLane
	controlQueues map[string]*sendLane
	tokenUsage    map[string]*tokenUsage
	ctx           context.Context
	cancel        context.CancelFunc
}

// Send queue whose workers stop when the adapter shuts it down
func NewChatSendQueue(name string) *ChatSendQueue {
	ctx, cancel := context.WithCancel(context.Background())
	return &ChatSendQueue{Name: name, chatQueues: map[string]*sendLane{}, controlQueues: map[string]*sendLane{},
		tokenUsage: map[string]*tokenUsage{}, ctx: ctx, cancel: cancel}
}

// Token usage of a chat, created with the window starting now
func (q *ChatSendQueue) getTokenUsage(chatID string) *tokenUsage {
	key := pystr.Strip(chatID)
	u, ok := q.tokenUsage[key]
	if !ok {
		u = &tokenUsage{lastReset: monotonic()}
		q.tokenUsage[key] = u
	}
	return u
}

// Consumes one token returning zero, else seconds until the next minute window, control spilling into the reserve
func (q *ChatSendQueue) bucketTryConsume(chatID string, isControl bool) float64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	usage := q.getTokenUsage(chatID)
	now := monotonic()
	if now-usage.lastReset > 60.0 {
		usage.normal, usage.reserved, usage.lastReset = 0, 0, now
	}
	if usage.normal < bucketNormalTokens {
		usage.normal++
		return 0
	}
	if isControl && usage.reserved < bucketReservedTokens {
		usage.reserved++
		return 0
	}
	return 60.0 - (now - usage.lastReset)
}

// Queues and lane name of the normal or control lane
func (q *ChatSendQueue) lane(isControl bool) (map[string]*sendLane, string) {
	if isControl {
		return q.controlQueues, "control"
	}
	return q.chatQueues, "normal"
}

// Queues a send for the chat and waits for its result, FIFO per chat and lane
func (q *ChatSendQueue) EnqueueChatSend(ctx context.Context, chatID string, send func(ctx context.Context) (any, error), isControl bool) (any, error) {
	key := pystr.Strip(chatID)
	q.mu.Lock()
	queues, laneName := q.lane(isControl)
	l, ok := queues[key]
	if !ok {
		slog.Debug(fmt.Sprintf("[%s] Creating %s queue + worker for chat %s", q.Name, laneName, key))
		l = &sendLane{notify: make(chan struct{}, 1)}
		queues[key] = l
		go q.sendWorker(key, l, isControl)
	}
	q.mu.Unlock()
	job := sendJob{send: send, result: make(chan sendResult, 1)}
	l.mu.Lock()
	slog.Debug(fmt.Sprintf("[%s] Enqueuing send for chat %s (lane=%s, qsize=%d)", q.Name, key, laneName, len(l.jobs)))
	l.jobs = append(l.jobs, job)
	l.mu.Unlock()
	select {
	case l.notify <- struct{}{}:
	default:
	}
	select {
	case r := <-job.result:
		return r.value, r.err
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

// Drains one lane under the token bucket, a shutdown abandoning the send in flight and failing the queued ones
func (q *ChatSendQueue) sendWorker(chatKey string, l *sendLane, isControl bool) {
	if !isControl {
		slog.Debug(fmt.Sprintf("[%s] Normal send worker started for chat %s", q.Name, chatKey))
	}
	defer func() {
		l.mu.Lock()
		pending := l.jobs
		l.jobs = nil
		l.mu.Unlock()
		for _, job := range pending {
			job.result <- sendResult{err: errors.New("WeCom adapter shutting down")}
		}
	}()
	for {
		job, ok := q.nextJob(l)
		if !ok {
			return
		}
		if wait := q.bucketTryConsume(chatKey, isControl); wait > 0 {
			if !isControl {
				slog.Debug(fmt.Sprintf("[%s] Normal worker rate-limited for chat %s, waiting %.1fs", q.Name, chatKey, wait))
			}
			if sendSleep(q.ctx, wait) != nil {
				return
			}
			q.bucketTryConsume(chatKey, isControl)
		}
		value, err := job.send(q.ctx)
		if q.ctx.Err() != nil {
			return
		}
		job.result <- sendResult{value, err}
	}
}

// Stops every worker, failing the sends still queued
func (q *ChatSendQueue) Shutdown() { q.cancel() }
