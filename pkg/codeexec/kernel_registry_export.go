package codeexec

// Seeds an unspawned project kernel of owner the way hermes cases set a registry item, with its probes and removal
func RegisterKernelForTests(owner, cwd string) (registered, stopped func() bool, remove func()) {
	k := newSessionKernel(kernelKey{owner: owner, mode: "project", python: "python", cwd: cwd})
	registry.mu.Lock()
	registry.kernels[k.key] = k
	registry.mu.Unlock()
	registered = func() bool {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		return registry.kernels[k.key] == k
	}
	stopped = func() bool {
		select {
		case <-k.stop:
			return true
		default:
			return false
		}
	}
	remove = func() {
		registry.mu.Lock()
		if registry.kernels[k.key] == k {
			delete(registry.kernels, k.key)
		}
		registry.mu.Unlock()
	}
	return registered, stopped, remove
}
