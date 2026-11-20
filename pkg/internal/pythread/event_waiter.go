package pythread

// Channel closed once the flag is set, a select arm for the asyncio Event wait of the Python
func (e *Event) Waiter() <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.waker()
}
