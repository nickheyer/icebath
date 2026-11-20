package discord

// Whether the view stopped or timed out
func (v *uiView) isFinished() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.finished
}

// Whether a click already answered the view
func (h *icebathView) isResolved() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.resolved
}
