package tui

import (
	"context"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

const (
	gitBranchTTL     = 15 * time.Second
	gitBranchTimeout = 500 * time.Millisecond
)

// Cached branch of a folder and when it was read
type gitBranchHit struct {
	at     time.Time
	branch string
}

// Branch of the session folder, refreshed every TTL with one git call per folder in flight
type gitBranchTracker struct {
	cwd      string
	branch   string
	gen      int
	cache    map[string]gitBranchHit
	inflight map[string]bool
}

// Refresh tick of the folder tracked under one generation
type gitBranchTickMsg struct{ gen int }

// Branch git reported for a folder, empty on a detached head or any failure
type gitBranchResultMsg struct{ cwd, branch string }

func newGitBranchTracker() *gitBranchTracker {
	return &gitBranchTracker{cache: map[string]gitBranchHit{}, inflight: map[string]bool{}}
}

// Current branch of the tracked folder, held over from the previous folder until the new one resolves
func (g *gitBranchTracker) current() string { return g.branch }

// Branch of a folder from git, empty for a detached head, a missing repository or a timeout
func resolveGitBranch(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitBranchTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	b := strings.TrimFunc(string(out), isJSSpace)
	if b == "HEAD" {
		return ""
	}
	return b
}

// Starts tracking a folder, a fresh generation with its first tick and refresh interval
func (g *gitBranchTracker) track(cwd string) tea.Cmd {
	if cwd == g.cwd && g.gen > 0 {
		return nil
	}
	if g.gen == 0 {
		g.branch = g.cache[cwd].branch
	}
	g.cwd = cwd
	g.gen++
	return tea.Batch(g.tick(), g.interval())
}

func (g *gitBranchTracker) interval() tea.Cmd {
	gen := g.gen
	return tea.Tick(gitBranchTTL, func(time.Time) tea.Msg { return gitBranchTickMsg{gen: gen} })
}

// Fresh cache hit applied at once, otherwise one git call for the folder unless one is in flight
func (g *gitBranchTracker) tick() tea.Cmd {
	if hit, ok := g.cache[g.cwd]; ok && time.Since(hit.at) < gitBranchTTL {
		g.branch = hit.branch
		return nil
	}
	if g.inflight[g.cwd] {
		return nil
	}
	g.inflight[g.cwd] = true
	cwd := g.cwd
	return func() tea.Msg { return gitBranchResultMsg{cwd: cwd, branch: resolveGitBranch(cwd)} }
}

// Handles the tracker messages, false for any other message
func (g *gitBranchTracker) update(m tea.Msg) (bool, tea.Cmd) {
	switch m := m.(type) {
	case gitBranchTickMsg:
		if m.gen != g.gen {
			return true, nil
		}
		return true, tea.Batch(g.tick(), g.interval())
	case gitBranchResultMsg:
		delete(g.inflight, m.cwd)
		g.cache[m.cwd] = gitBranchHit{at: time.Now(), branch: m.branch}
		if m.cwd == g.cwd {
			g.branch = m.branch
		}
		return true, nil
	}
	return false, nil
}
