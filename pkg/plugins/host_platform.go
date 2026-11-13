package plugins

import "context"

// Context the guest was started for, its profile home bound
func (f *HostedCallable) HostContext() context.Context { return f.host.baseCtx }

// Wire reference of an object living in this child, marked stale when it belongs to another host or an earlier child
func (h *PluginHost) childObjectRef(o *HostedObject) map[string]any {
	s := o.slot
	s.mu.Lock()
	ref, generation, owner := s.ref, s.generation, s.host
	s.mu.Unlock()
	h.mu.Lock()
	current := h.generation
	h.mu.Unlock()
	out := map[string]any{"__child_object__": ref}
	if owner != h || generation != current {
		out["stale"] = true
	}
	return out
}
