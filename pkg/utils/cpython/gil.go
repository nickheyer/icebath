package cpythonutil

// Proof that the goroutine holds an attached thread state of the interpreter, valid inside the call that handed it out and never shared between goroutines
type GIL struct {
	h   *Host
	out outParams
}

// Out parameters of C calls kept in the token, so passing their address does not allocate on every call
type outParams struct {
	size     int
	overflow int32
	pos      int
	key, val uintptr
}

// The host the token belongs to
func (g *GIL) Host() *Host { return g.h }

// Borrowed event loop running on the host loop thread, 0 once Finalize stopped it
func (g *GIL) Loop() uintptr { return g.h.loop }

// Drops every reference Go released since the last GIL entry
func (g *GIL) Drain() {
	assertAttached("Drain")
	g.h.drainReleases()
}

// Releases the GIL around blocking Go work, even when fn panics; a thread finishing after finalization began parks instead of re-entering Python
func (g *GIL) Unblocked(fn func()) {
	assertAttached("Unblocked")
	ts := pyEvalSaveThread()
	defer g.h.reattach(ts)
	fn()
}

// Restores a thread state Unblocked saved, or parks the goroutine for good once finalization has begun
func (h *Host) reattach(ts uintptr) {
	// The read lock is held across the wait for the GIL; Finalize takes the write lock with the GIL let go, so the two never wait on each other
	h.reacquire.RLock()
	if h.finalizing.Load() {
		h.reacquire.RUnlock()
		select {}
	}
	pyEvalRestoreThread(ts)
	h.reacquire.RUnlock()
}

// Panics when the calling thread has no attached thread state, the misuse CPython would end the process for; a no-op when libpython cannot tell
func assertAttached(op string) {
	if pyThreadStateGetUnchecked != nil && pyThreadStateGetUnchecked() == 0 {
		panic("cpython: " + op + " called on a thread without an attached Python thread state")
	}
}
