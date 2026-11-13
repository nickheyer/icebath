package state

// Windows has no descriptor limit to measure headroom against
func fdSoftLimit() (int64, bool) { return 0, false }

// Windows has no descriptor table to count
func openFDCount() (int, bool) { return 0, false }
