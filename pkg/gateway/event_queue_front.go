package gateway

// Puts an item back at the head of the queue ahead of everything waiting, never refused by the bound
func (q *EventQueue) PutFirst(item any) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append([]any{item}, q.items...)
	wake(&q.ready)
}

// Removes and returns the queued items match accepts, the rest staying in order
func (q *EventQueue) TakeMatching(match func(item any) bool) []any {
	q.mu.Lock()
	defer q.mu.Unlock()
	var taken, kept []any
	for _, item := range q.items {
		if match(item) {
			taken = append(taken, item)
		} else {
			kept = append(kept, item)
		}
	}
	if len(taken) > 0 {
		q.items = kept
		wake(&q.moved)
	}
	return taken
}
