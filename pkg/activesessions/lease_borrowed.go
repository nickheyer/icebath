package activesessions

// Inert token of a slot another process owns, its release a no-op and its transfer local only
func NewBorrowedLease(leaseID, sessionID, surface string) *Lease {
	return &Lease{LeaseID: leaseID, Surface: surface, Enabled: false, sessionID: sessionID}
}
