package wecom

// Req ids that hold a reply queue, in no fixed order
func (s *Streamer) replyQueueKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.replyQueues))
	for k := range s.replyQueues {
		keys = append(keys, k)
	}
	return keys
}

// Whether a req id holds a reply queue and whether that queue awaits an ack
func (s *Streamer) replyQueueState(reqID string) (exists, pendingAck bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	queue, ok := s.replyQueues[reqID]
	return ok, ok && queue.PendingAck != nil
}

// Forgets that the stream channel of a chat expired so a fresh req id can stream again
func (s *Streamer) clearExpiredChat(chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.expiredChats, chatID)
}

// Whether an active or expired stream owns the req id of the chat
func (s *Streamer) streamOwnsChat(chatID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.init()
	return s.findActiveTurnForChat(chatID) != nil || s.expiredChats[chatID]
}
