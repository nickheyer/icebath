package tools

import (
	"sort"

	"github.com/nickheyer/icebath/pkg/config"
)

type builtinEntry struct {
	def    *Def
	module string
}

var safeWorkerEnabled = config.SafeWorkerEnabled

var safeWorkerModules = []string{"tools.file_tools", "tools.terminal_tool", "tools.process_registry"}

var safeWorkerTools = map[string]bool{
	"read_file": true, "write_file": true, "patch": true, "search_files": true, "terminal": true, "process_manage": true,
}

func (r *Registry) registerBuiltin(d Def, module string) {
	r.Register(d)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.builtin == nil {
		r.builtin = map[string]builtinEntry{}
	}
	r.builtin[d.Name] = builtinEntry{def: r.tools[d.Name], module: module}
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

// DiscoverBuiltinTools lists the builtin tool modules the registry exposes, only the reviewed core under a safe worker
func DiscoverBuiltinTools() []string {
	if safeWorkerEnabled() {
		return append([]string{}, safeWorkerModules...)
	}
	Default.mu.RLock()
	defer Default.mu.RUnlock()
	seen := map[string]bool{}
	var out []string
	for name, b := range Default.builtin {
		if Default.tools[name] == b.def && b.module != "" && !seen[b.module] {
			seen[b.module] = true
			out = append(out, b.module)
		}
	}
	sort.Strings(out)
	return out
}
