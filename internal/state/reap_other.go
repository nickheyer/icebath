//go:build !linux

package state

import (
	"context"
	"time"
)

// Desktop backend reaping runs on Linux alone, so no holder is ever signalled here
func (s *Store) reapInactiveOrphanDesktopHolders(context.Context, []Holder, time.Duration) []int {
	return nil
}
