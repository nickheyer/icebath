package tools

import (
	"github.com/nickheyer/icebath/pkg/config"
)

type builtinEntry struct {
	def *Def
}

var safeWorkerEnabled = config.SafeWorkerEnabled

var safeWorkerTools = map[string]bool{
	"read_file": true, "write_file": true, "patch": true, "search_files": true, "terminal": true, "process_manage": true,
}

func (r *Registry) registerBuiltin(d Def) {
	r.Register(d)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builtin == nil {
		r.builtin = map[string]builtinEntry{}
	}
	r.builtin[d.Name] = builtinEntry{def: r.tools[d.Name]}
}

// Whether a global entry is a builtin outside the reviewed core while a safe worker policy is bound
func (r *Registry) hiddenLocked(name string, d *Def) bool {
	if d == nil || safeWorkerTools[name] {
		return false
	}
	b, ok := r.builtin[name]
	return ok && b.def == d && safeWorkerEnabled()
}

// Global entry for a name unless the safe worker policy hides it
func (r *Registry) globalLocked(name string) *Def {
	d := r.tools[name]
	if r.hiddenLocked(name, d) {
		return nil
	}
	return d
}
