//go:build !linux

package state

// Desktop holder reaping exists only on Linux, every other platform keeping its holders
func reapInactiveOrphanDesktopHolders(string, []Holder, float64) ([]int, error) { return nil, nil }
