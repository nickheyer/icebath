package tui

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// Ordered handoff of messages to the program that never blocks the caller, for senders inside the update loop
type mailbox struct {
	mu    sync.Mutex
	queue []tea.Msg
	wake  chan struct{}
}

// Mailbox delivering through send on its own goroutine until the context ends
func newMailbox(ctx context.Context, send func(tea.Msg)) *mailbox {
	m := &mailbox{wake: make(chan struct{}, 1)}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			}
			m.mu.Lock()
			batch := m.queue
			m.queue = nil
			m.mu.Unlock()
			for _, msg := range batch {
				send(msg)
			}
		}
	}()
	return m
}

// Queues a message behind the ones before it and returns at once
func (m *mailbox) post(msg tea.Msg) {
	m.mu.Lock()
	m.queue = append(m.queue, msg)
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
