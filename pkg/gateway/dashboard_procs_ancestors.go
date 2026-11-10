package gateway

import (
	"context"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
)

var (
	ancestorsGOOS = runtime.GOOS
	// Runs ps for one parent lookup, a var so tests answer without spawning
	ancestorsRunPS = func(ctx context.Context, argv []string) (*pysubprocess.Completed, error) {
		return pysubprocess.RunWith(ctx, argv, pysubprocess.Options{Timeout: 5 * time.Second})
	}
)

// Parent chain of pid nearest first, ending at init, a lookup failure, a cycle or eight ancestors
func ProcessAncestors(ctx context.Context, pid int) []int {
	return processAncestors(ctx, pid, 8)
}

// Parent chain of pid nearest first, ending at init, a lookup failure, a cycle or maxDepth ancestors
func processAncestors(ctx context.Context, pid, maxDepth int) []int {
	chain := []int{}
	current := pid
	for len(chain) < maxDepth {
		parent, ok := processPpid(ctx, current)
		if !ok || parent <= 1 || parent == pid || slices.Contains(chain, parent) {
			break
		}
		chain = append(chain, parent)
		current = parent
	}
	return chain
}

// Parent pid that ps reports, not ok on Windows or when the lookup fails
func processPpid(ctx context.Context, pid int) (int, bool) {
	if ancestorsGOOS == "windows" {
		return 0, false
	}
	result, err := ancestorsRunPS(ctx, []string{"ps", "-o", "ppid=", "-p", strconv.Itoa(pid)})
	if err != nil || result.Returncode != 0 || len(result.Stdout) == 0 {
		return 0, false
	}
	fields := pystr.SplitWhitespace(pystr.Strip(pystr.DecodeUTF8Replace(result.Stdout)), -1)
	if len(fields) == 0 {
		return 0, false
	}
	parent, ok := pyint.Parse(fields[0])
	if !ok || !parent.IsInt64() {
		return 0, false
	}
	return int(parent.Int64()), true
}
