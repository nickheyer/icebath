package worktrees

import "github.com/nickheyer/icebath/internal/worktrees"

func Register(host *worktrees.Worktrees) *worktrees.Registry {
	registry := new(worktrees.Registry)
	host.Register(registry)
	return registry
}
