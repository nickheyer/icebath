package updaterecord

import "context"

// Restores the receipt that was open before the current one began, the reset of its begin token
func ResetCurrentReceipt(ctx context.Context) {
	st := stateOf(ctx)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.current != nil {
		st.current = st.current.prev
	}
}
