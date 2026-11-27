package terminal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	dockerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker/config"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

const (
	// Label of every container icebath makes, and the labels it finds them again by
	agentLabel       = "icebath-agent"
	taskLabel        = "icebath-task-id"
	profileLabel     = "icebath-profile"
	egressLabel      = "icebath-egress"
	environmentLabel = "icebath-environment"
	// Digest of the host folder a persistent container serves one workspace of, which stops rather than goes when idle
	workspaceLabel = "icebath-workspace"
)

// Task of a container serving one workspace, ending in the digest of its folder
var workspaceTask = regexp.MustCompile(`:ws:([0-9a-f]{16})$`)

var workspaceDigest = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Removes the exited containers of the profile left by earlier runs, once per profile and run, unless the setting keeps them; those that
// sat stopped for less than twice the idle limit, and stopped workspace containers waiting for their sessions, stay
func (r *registrant) reap(ctx context.Context, cfg *config.Config, docker, shared string) {
	if !cfg.GetBool(dockerconfig.OrphanReaper) {
		return
	}
	profile := identity(cfg, shared)
	r.mu.Lock()
	if r.reaped[profile] {
		r.mu.Unlock()
		return
	}
	r.reaped[profile] = true
	r.mu.Unlock()
	maxAge := 2 * time.Duration(max(cfg.GetInt(terminalconfig.Lifetime), 60)) * time.Second
	if removed := r.reapOrphans(ctx, cfg, docker, maxAge, profile); removed > 0 {
		slog.Info("Docker orphan reaper removed stale containers", "count", removed, "profile", profile)
	}
}

func (r *registrant) reapOrphans(ctx context.Context, cfg *config.Config, docker string, maxAge time.Duration, profile string) int {
	argv := []string{docker, "ps", "-a", "--filter", "label=" + agentLabel + "=1", "--filter", "status=exited", "--filter", "label=" + profileLabel + "=" + label(profile),
		"--format", "{{.ID}}"}
	listing := query(ctx, argv, 15*time.Second, "orphan reaper docker ps failed", "orphan reaper docker ps returned nonzero")
	if listing == nil {
		return 0
	}
	removed := 0
	for _, line := range strings.Split(listing.Stdout, "\n") {
		id := strings.TrimSpace(line)
		if id == "" {
			continue
		}
		finished, ok := finishedAt(ctx, docker, id)
		if !ok {
			continue
		}
		age := time.Since(finished)
		if age < maxAge {
			continue
		}
		r.stripPrivileges(ctx, cfg, docker, id, nil)
		if idleWorkspace(ctx, docker, id) {
			continue
		}
		captured := query(ctx, []string{docker, "rm", id}, 30*time.Second, "orphan reaper docker rm failed", "")
		switch {
		case captured == nil:
		case captured.Code == 0:
			removed++
			slog.Info("Reaped orphan container", "id", short(id), "age_seconds", int(age.Seconds()))
		default:
			slog.Debug("docker rm failed", "id", short(id), "stderr", strings.TrimSpace(captured.Stderr))
		}
	}
	return removed
}

// When a stopped container finished, false when docker can't say
func finishedAt(ctx context.Context, docker, id string) (time.Time, bool) {
	captured := query(ctx, []string{docker, "inspect", "--format", "{{.State.FinishedAt}}", id}, 10*time.Second, "orphan reaper docker inspect failed", "")
	if captured == nil || captured.Code != 0 {
		return time.Time{}, false
	}
	raw := strings.TrimSpace(captured.Stdout)
	if raw == "" || strings.HasPrefix(raw, "0001-01-01") {
		return time.Time{}, false
	}
	finished, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		slog.Debug("could not parse FinishedAt", "raw", raw, "error", err)
		return time.Time{}, false
	}
	return finished, true
}

// Whether a stopped container is a workspace's, kept for its sessions to start again
func idleWorkspace(ctx context.Context, docker, id string) bool {
	captured := query(ctx, []string{docker, "inspect", "--format", `{{index .Config.Labels "` + workspaceLabel + `"}}`, id}, 10*time.Second,
		"orphan reaper docker inspect failed", "")
	return captured != nil && captured.Code == 0 && workspaceDigest.MatchString(strings.TrimSpace(captured.Stdout))
}

// Key the persistent container of the sessions working in a folder is kept under, by a digest of the folder it resolves to
func workspaceKey(dir string) string {
	path := terminal.ExpandUser(dir)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	sum := sha256.Sum256([]byte(path))
	return "default:ws:" + hex.EncodeToString(sum[:])[:16]
}
