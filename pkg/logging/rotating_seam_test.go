package logging

// Stream the handler holds, nil once closed or between writes under the Windows lock
func (h *managedRotatingFileHandler) currentStream() logStream {
	h.lock.Lock()
	defer h.lock.Unlock()
	return h.stream
}
