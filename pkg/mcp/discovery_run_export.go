package mcp

import "context"

// Puts a discovery run that ends when done closes in the slot of the caller's home, nil emptying it, and returns the restore
func SetHomeDiscoveryRunForTests(ctx context.Context, done chan struct{}) func() {
	key := discoveryHomeKey(ctx)
	discoveryMu.Lock()
	saved, had := discoveryRuns[key]
	if done == nil {
		delete(discoveryRuns, key)
	} else {
		discoveryRuns[key] = &discoveryRun{done: done}
	}
	discoveryMu.Unlock()
	return func() {
		discoveryMu.Lock()
		defer discoveryMu.Unlock()
		if had {
			discoveryRuns[key] = saved
		} else {
			delete(discoveryRuns, key)
		}
	}
}
