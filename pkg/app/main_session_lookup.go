package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Git repo root of the working directory, else the directory itself, empty when neither resolves
func resolveWorkspaceKey() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if text := strings.TrimSpace(strings.ToValidUTF8(string(out), "�")); err == nil && text != "" {
		if abs, aerr := filepath.Abs(text); aerr == nil {
			return abs
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

// Most recently used session of a source, scoped to the current workspace first, empty on a miss
func resolveLastSession(ctx context.Context, source string) string {
	var sources []string
	if source == "cli" {
		for s := range agent.CLIFamilySources {
			sources = append(sources, s)
		}
		sort.Strings(sources)
	} else {
		sources = []string{source}
	}
	db, err := state.NewSessionDB(ctx, home.StatePath(home.Dir(ctx)), true)
	if err != nil {
		return ""
	}
	defer db.Close()
	reader := gatewaycli.NewSurfaceLabelReader(db)
	if key := resolveWorkspaceKey(); key != "" {
		rows, err := reader.SearchSessions(ctx, gatewaycli.SearchSessionsOptions{Sources: sources, Limit: 1, WorkspaceKey: key, SkipLaunchStubs: true})
		if err != nil {
			return ""
		}
		if len(rows) > 0 {
			return pyrepr.Str(rows[0]["id"])
		}
	}
	rows, err := reader.SearchSessions(ctx, gatewaycli.SearchSessionsOptions{Sources: sources, Limit: 1, SkipLaunchStubs: true})
	if err != nil || len(rows) == 0 {
		return ""
	}
	return pyrepr.Str(rows[0]["id"])
}
