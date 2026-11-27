package server

import (
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/internal/tools"
)

// Hears of every call that returned, to note the files a session's writes landed in during the turn running, which verify on stop asks
// about before the turn ends
type edits struct {
	a *Agent
}

func (edits) Tools() []tools.Def { return nil }

func (e edits) Finished(call *tools.Call, result string) {
	e.a.mu.Lock()
	r := e.a.runs[call.SessionID]
	e.a.mu.Unlock()
	if r == nil {
		return
	}
	def, _ := e.a.tools.Def(call.Name)
	var paths []string
	for _, path := range tools.Landed(def, call, result) {
		if !filepath.IsAbs(path) && call.Env != nil && call.Env.Cwd != "" {
			path = filepath.Join(call.Env.Cwd, path)
		}
		paths = append(paths, filepath.Clean(path))
	}
	if len(paths) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, path := range paths {
		if !slices.Contains(r.edited, path) {
			r.edited = append(r.edited, path)
		}
	}
}
