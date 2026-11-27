package terminal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

// Name of the runtime a program is, as the user knows it
func runtimeName(program string) string {
	if sandbox.Podman(program) {
		return "Podman"
	}
	return "Docker"
}

// How to bring the runtime back up
func startHint(program string) string {
	if runtimeName(program) == "Podman" {
		return "run `podman machine start` and retry"
	}
	return "start Docker and retry"
}

// Output of a docker query, nil with a debug line when it could not run, or exited non-zero when nonzero names that failure
func query(ctx context.Context, argv []string, timeout time.Duration, fail, nonzero string) *terminal.Captured {
	captured, err := terminal.Capture(ctx, argv, timeout, nil)
	if err != nil {
		slog.Debug(fail, "error", err)
		return nil
	}
	if nonzero != "" && captured.Code != 0 {
		slog.Debug(nonzero, "code", captured.Code, "stderr", strings.TrimSpace(captured.Stderr))
		return nil
	}
	return captured
}

// Characters a docker label value can't hold
var labelBad = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// A value made safe for a docker label, at most 63 characters
func label(value string) string {
	out := labelBad.ReplaceAllString(value, "_")
	if len(out) > 63 {
		out = out[:63]
	}
	if out == "" {
		return "unknown"
	}
	return out
}

// Profile label of the profile's containers, which reuse and the orphan sweep look for: the profile's name, or for a shared name that
// name with a digest of it, the same for every profile
func identity(cfg *config.Config, shared string) string {
	if shared == "" {
		return label(cfg.Profile())
	}
	digest := sha256.Sum256([]byte(shared))
	name := label(shared)
	if len(name) > 50 {
		name = name[:50]
	}
	return name + "-" + hex.EncodeToString(digest[:])[:12]
}

// First twelve characters of a container id, as logs name it
func short(id string) string {
	return id[:min(12, len(id))]
}
