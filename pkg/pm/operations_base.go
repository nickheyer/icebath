package pm

import (
	"time"
)

// LockProjectOptions are lock_project's keywords, nil Env for None
type LockProjectOptions struct {
	Source   string
	Upgrade  bool
	Python   string
	Env      map[string]string
	Cache    string
	Offline  bool
	Explicit bool
}

// EnsurePythonToolOptions are ensure_python_tool's arguments, empty Root for None
type EnsurePythonToolOptions struct {
	Name         string
	Requirements []string
	Executable   string
	Root         string
	Explicit     bool
	Timeout      time.Duration
}
