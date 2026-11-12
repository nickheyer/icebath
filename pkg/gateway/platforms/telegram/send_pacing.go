package telegram

import (
	"context"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// FIFO lock handing ownership to waiters in arrival order, the asyncio Lock of hermes
type fifoLock struct {
	locked  bool
	waiters []chan struct{}
}

// Per chat send ordering, flood windows, shared send and edit slots and typing cooldowns
type sendPacer struct {
	mu                    sync.Mutex
	locks                 map[string]*fifoLock
	typingCooldownUntil   map[string]float64
	typingCooldownSeconds float64
	sendCooldownUntil     map[string]float64
	outboundSlotUntil     map[string]float64
	outboundSlotSecs      float64
}

// Pacer with the configured typing cooldown and the shared one second outbound budget
func newSendPacer(typingCooldownSeconds float64) *sendPacer {
	return &sendPacer{locks: map[string]*fifoLock{}, typingCooldownUntil: map[string]float64{},
		typingCooldownSeconds: typingCooldownSeconds, sendCooldownUntil: map[string]float64{}, outboundSlotUntil: map[string]float64{},
		outboundSlotSecs: telegramChatOutboundBudgetSecs}
}

const telegramChatOutboundBudgetSecs = 1.0

// Bot API normalized chat id as the pacing key
func chatKey(chatID any) string { return pyrepr.Str(NormalizeTelegramChatID(chatID)) }

type chatLockKey struct{ key string }

// Runs fn holding the chat's FIFO send lock, re-entrant for calls nested under the same holder context
func (s *sendPacer) withChatSendLock(ctx context.Context, chatID any, fn func(ctx context.Context) error) error {
	key := chatKey(chatID)
	if held, _ := ctx.Value(chatLockKey{key}).(bool); held {
		return fn(ctx)
	}
	s.mu.Lock()
	lock, ok := s.locks[key]
	if !ok {
		lock = &fifoLock{}
		s.locks[key] = lock
	}
	if lock.locked {
		ticket := make(chan struct{})
		lock.waiters = append(lock.waiters, ticket)
		s.mu.Unlock()
		select {
		case <-ticket:
		case <-ctx.Done():
			s.mu.Lock()
			granted := true
			for i, w := range lock.waiters {
				if w == ticket {
					lock.waiters = append(lock.waiters[:i], lock.waiters[i+1:]...)
					granted = false
					break
				}
			}
			s.mu.Unlock()
			if granted {
				s.release(key, lock)
			}
			return ctx.Err()
		}
	} else {
		lock.locked = true
		s.mu.Unlock()
	}
	defer s.release(key, lock)
	return fn(context.WithValue(ctx, chatLockKey{key}, true))
}

// Hands the lock to the next waiter, dropping the entry when nobody is queued
func (s *sendPacer) release(key string, lock *fifoLock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(lock.waiters) > 0 {
		next := lock.waiters[0]
		lock.waiters = lock.waiters[1:]
		close(next)
		return
	}
	lock.locked = false
	if s.locks[key] == lock {
		delete(s.locks, key)
	}
}

// Whether a typing failure is transient enough to cool the chat's typing refreshes down
func isTransientTypingError(err error) bool {
	if te, ok := err.(*TelegramError); ok && te.Class == "RetryAfter" {
		return true
	}
	if coded, ok := err.(interface{ StatusCode() int }); ok {
		if code := coded.StatusCode(); code == 429 || code >= 500 {
			return true
		}
	}
	text := pystr.Lower(err.Error())
	for _, marker := range []string{"too many requests", "rate limit", "timed out", "timeout", "temporar"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return isOSError(err) || className(err) == "TimeoutError" || className(err) == "ConnectionError"
}

// Suppresses typing refreshes for the chat after a transient failure, the flood wait when Telegram sent one
func (s *sendPacer) recordTypingCooldown(chatID string, err error) {
	delay := s.typingCooldownSeconds
	if te, ok := err.(*TelegramError); ok && te.Class == "RetryAfter" {
		delay = te.RetryAfter
	}
	s.mu.Lock()
	s.typingCooldownUntil[chatID] = monotonicNow() + max(1.0, min(delay, 300.0))
	s.mu.Unlock()
}

// Whether the chat's typing refreshes are cooling down, expiring the window once it passed
func (s *sendPacer) typingInCooldown(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.typingCooldownUntil[chatID]
	if !ok {
		return false
	}
	if monotonicNow() < until {
		return true
	}
	delete(s.typingCooldownUntil, chatID)
	return false
}

// Clears the chat's typing cooldown after a typing action landed
func (s *sendPacer) clearTypingCooldown(chatID string) {
	s.mu.Lock()
	delete(s.typingCooldownUntil, chatID)
	s.mu.Unlock()
}

// Arms the chat's flood window after a refused send and returns the shared fail closed result
func (s *sendPacer) recordSendFloodCooldown(chatID any, wait float64) gateway.SendResult {
	s.mu.Lock()
	s.sendCooldownUntil[chatKey(chatID)] = monotonicNow() + max(1.0, min(wait, 300.0))
	s.mu.Unlock()
	return floodCapResult(wait)
}

// Seconds left in the chat's flood window, nil when sends may go out
func (s *sendPacer) sendFloodCooldownRemaining(chatID any) *float64 {
	key := chatKey(chatID)
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, ok := s.sendCooldownUntil[key]
	if !ok {
		return nil
	}
	if remaining := deadline - monotonicNow(); remaining > 0 {
		return &remaining
	}
	delete(s.sendCooldownUntil, key)
	return nil
}

// Seconds until the chat's shared send and edit slot opens, zero when a call may fire now
func (s *sendPacer) chatOutboundSlotRemaining(chatID any) float64 {
	key := chatKey(chatID)
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline, ok := s.outboundSlotUntil[key]
	if !ok {
		return 0
	}
	remaining := deadline - monotonicNow()
	if remaining <= 0 {
		delete(s.outboundSlotUntil, key)
		return 0
	}
	return remaining
}

// Re-arms the chat's shared slot after a send or edit call fired
func (s *sendPacer) holdChatOutboundSlot(chatID any) {
	s.mu.Lock()
	s.outboundSlotUntil[chatKey(chatID)] = monotonicNow() + max(0.0, s.outboundSlotSecs)
	s.mu.Unlock()
}
