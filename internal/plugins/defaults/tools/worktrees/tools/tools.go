package tools

import (
	"os"
	"sync"

	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/internal/worktrees"
)

// Brings back the removed worktree a session works in as its tools are readied, so its commands and files have their folder again
type registrant struct {
	worktrees *worktrees.Worktrees
	// One recreation at a time, so two calls readied together make the tree once
	mu sync.Mutex
}

func Register(registry *tools.Registry, host *worktrees.Worktrees) {
	registry.Register(&registrant{worktrees: host})
}

func (r *registrant) Tools() []tools.Def { return nil }

func (r *registrant) Prepare(env *tools.Env) {
	if env.Cwd == "" {
		return
	}
	if _, err := os.Stat(env.Cwd); err == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.worktrees.Recreate(env.Config, env.Cwd, "")
}
