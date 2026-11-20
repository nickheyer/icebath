package activesessions

// Whether the lease holds a real registry slot
func (l *Lease) IsEnabled() bool { return l.Enabled }

// Whether the lease records its owner process for cross process liveness checks
func (l *Lease) TracksLiveness() bool { return l.TrackLiveness }
