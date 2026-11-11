package tuiserver

import "sync"

// One handler a module declared, with whether it runs in the profile scope of its params
type pendingHandler struct {
	name          string
	fn            Handler
	profileScoped bool
}

// Deferred method registrar of one handler module, installed once every module declared its handlers
type handlerRegistry struct {
	mu      sync.Mutex
	pending []pendingHandler
}

// Declares a handler, registered when the registry installs
func (r *handlerRegistry) method(name string, fn Handler) Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, pendingHandler{name: name, fn: fn})
	return fn
}

// Declares a handler that runs in the profile scope of its params when installed
func (r *handlerRegistry) scopedMethod(name string, fn Handler) Handler {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pending = append(r.pending, pendingHandler{name: name, fn: fn, profileScoped: true})
	return fn
}

// Names of every handler the module declared
func (r *handlerRegistry) names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.pending))
	for _, entry := range r.pending {
		out = append(out, entry.name)
	}
	return out
}

// Registers every declared handler, the profile scoped ones wrapped in their scope
func (r *handlerRegistry) install() {
	r.mu.Lock()
	pending := append([]pendingHandler(nil), r.pending...)
	r.mu.Unlock()
	for _, entry := range pending {
		fn := entry.fn
		if entry.profileScoped {
			fn = profileScoped(fn)
		}
		registerMethod(entry.name, fn)
	}
}
