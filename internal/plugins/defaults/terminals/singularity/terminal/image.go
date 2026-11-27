package terminal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	singularityconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/singularity/config"
	"github.com/nickheyer/icebath/internal/terminal"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
	"github.com/nickheyer/icebath/pkg/fsx"
)

const (
	// Longest a build of another image may take before apptainer is left to pull it at each start
	pullBuild = 10 * time.Minute
	// Longest a build of icebath's own sandbox image may take
	sandboxBuild = 45 * time.Minute
)

// Image the setting names, else icebath's own sandbox image as a docker URL
func configured(cfg *config.Config) string {
	if image := strings.TrimSpace(cfg.GetString(singularityconfig.Image)); image != "" {
		return image
	}
	return "docker://" + sandbox.Image
}

// A bare registry reference as a docker URL; a URL of any scheme, a .sif file and a path that exists kept as given
func reference(image string) string {
	if strings.Contains(image, "://") || strings.HasSuffix(image, ".sif") {
		return image
	}
	if _, err := os.Stat(image); err == nil {
		return image
	}
	return "docker://" + image
}

// Environment of a build, its temp files and cache in the image cache, for apptainer and singularity alike
func buildEnv(temp, cache string) []string {
	env := terminal.ProcessEnv()
	for _, prefix := range []string{"APPTAINER_", "SINGULARITY_"} {
		env[prefix+"TMPDIR"], env[prefix+"CACHEDIR"] = temp, cache
	}
	return terminal.Environ(env)
}

// Image an instance starts from: a .sif file as it is, icebath's own image built once from its Dockerfile, another docker URL built
// once into the cache, or that URL itself when its build fails, which apptainer then pulls at each start; any other source as it is
func (r *registrant) sif(ctx context.Context, cfg *config.Config, image, exe string, say func(string)) (string, error) {
	if strings.HasSuffix(image, ".sif") && fsx.Exists(image) {
		return image, nil
	}
	if !strings.HasPrefix(image, "docker://") {
		return image, nil
	}
	if sandbox.Default(strings.TrimPrefix(image, "docker://")) {
		return r.sandboxSIF(ctx, cfg, exe, say)
	}
	cache, err := cacheDir(cfg)
	if err != nil {
		return "", err
	}
	target := filepath.Join(cache, strings.NewReplacer("/", "-", ":", "-").Replace(strings.TrimPrefix(image, "docker://"))+".sif")
	if fsx.Exists(target) {
		return target, nil
	}
	r.building.Lock()
	defer r.building.Unlock()
	if fsx.Exists(target) {
		return target, nil
	}
	for _, line := range []string{"Building SIF image (one-time setup)...", "  Source: " + image, "  Target: " + target} {
		slog.Info(line)
		say(line)
	}
	temp := filepath.Join(cache, "tmp")
	if err := os.MkdirAll(temp, 0o755); err != nil {
		return "", err
	}
	captured, err := terminal.Capture(ctx, []string{exe, "build", target, image}, pullBuild, buildEnv(temp, cache))
	fallback := func(text string, attrs ...any) (string, error) {
		slog.Warn(text, attrs...)
		say(text)
		return image, nil
	}
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		if fsx.Exists(target) {
			if err := os.Remove(target); err != nil {
				return "", err
			}
		}
		return fallback("SIF build timed out, falling back to docker:// URL")
	case err != nil:
		return fallback(fmt.Sprintf("SIF build error: %s, falling back to docker:// URL", err))
	case captured.Code != 0:
		stderr := []rune(captured.Stderr)
		return fallback("SIF build failed, falling back to docker:// URL", "error", string(stderr[:min(len(stderr), 500)]))
	}
	slog.Info("SIF image built successfully")
	say("SIF image built successfully")
	return target, nil
}

// .sif of icebath's own sandbox image, kept by the digest of its Dockerfile: converted from the image Docker builds, through an archive
// for Podman, whose images Apptainer can't read in place, else built with fakeroot from the Apptainer definition of the Dockerfile
func (r *registrant) sandboxSIF(ctx context.Context, cfg *config.Config, exe string, say func(string)) (string, error) {
	cache, err := cacheDir(cfg)
	if err != nil {
		return "", err
	}
	target := filepath.Join(cache, "icebath-sandbox-"+sandbox.Digest()[:16]+".sif")
	if fsx.Exists(target) {
		return target, nil
	}
	r.building.Lock()
	defer r.building.Unlock()
	if fsx.Exists(target) {
		return target, nil
	}
	temp := filepath.Join(cache, "tmp")
	if err := os.MkdirAll(temp, 0o755); err != nil {
		return "", err
	}
	progress := func(line string, headline bool) {
		if headline {
			slog.Info(line)
			say("  " + line)
			return
		}
		slog.Debug(line)
	}
	var argv []string
	switch docker := sandbox.Docker(cfg); {
	case docker != "" && !sandbox.Podman(docker):
		if err := sandbox.Ensure(ctx, docker, sandbox.Image, progress); err != nil {
			return "", err
		}
		argv = []string{exe, "build", target, "docker-daemon://" + sandbox.Image}
	case docker != "":
		if err := sandbox.Ensure(ctx, docker, sandbox.Image, progress); err != nil {
			return "", err
		}
		archive := filepath.Join(temp, "icebath-sandbox-"+sandbox.Digest()[:16]+".tar")
		defer os.Remove(archive)
		save := []string{docker, "save", "-o", archive, sandbox.Image}
		captured, err := terminal.Capture(ctx, save, sandboxBuild, nil)
		if err == nil {
			err = captured.Err(save)
		}
		if err != nil {
			return "", fmt.Errorf("saving the sandbox image for Apptainer: %w", err)
		}
		argv = []string{exe, "build", target, "docker-archive://" + archive}
	default:
		definition, err := sandbox.Apptainer()
		if err != nil {
			return "", err
		}
		path := filepath.Join(temp, "icebath-sandbox.def")
		if err := os.WriteFile(path, []byte(definition), 0o644); err != nil {
			return "", err
		}
		defer os.Remove(path)
		argv = []string{exe, "build", "--fakeroot", target, path}
	}
	slog.Info("Building the sandbox SIF image (one-time setup)", "source", argv[len(argv)-1], "target", target)
	say("Building the sandbox SIF image " + target + " (one-time setup)")
	captured, err := terminal.Capture(ctx, argv, sandboxBuild, buildEnv(temp, cache))
	if err != nil {
		return "", fmt.Errorf("building the sandbox SIF image: %w", err)
	}
	if captured.Code != 0 {
		os.Remove(target)
		stderr := []rune(captured.Stderr)
		return "", fmt.Errorf("building the sandbox SIF image failed: %s", string(stderr[:min(len(stderr), 500)]))
	}
	slog.Info("Sandbox SIF image built", "target", target)
	return target, nil
}
