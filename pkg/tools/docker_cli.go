package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

var dockerSearchPaths = []string{"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker"}

// Located docker or podman CLI, empty until the first lookup finds one
var dockerExecutable struct {
	sync.Mutex
	path string
}

// Forgets the located docker CLI so the next lookup searches the PATH again
func ResetDockerExecutable() {
	dockerExecutable.Lock()
	dockerExecutable.path = ""
	dockerExecutable.Unlock()
}

// Whether the path is a regular file the user may execute
func isExecutableFile(p string) bool {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}

// FindDocker locates the docker or podman CLI once per process
func FindDocker(env *Env) string {
	dockerExecutable.Lock()
	defer dockerExecutable.Unlock()
	if dockerExecutable.path != "" {
		return dockerExecutable.path
	}
	found := ""
	if override := os.Getenv("ICEBATH_DOCKER_BINARY"); override != "" && isExecutableFile(override) {
		envLog(env).Info("Using ICEBATH_DOCKER_BINARY override", "path", override)
		found = override
	} else if p, err := exec.LookPath("docker"); err == nil {
		found = p
	} else if p, err := exec.LookPath("podman"); err == nil {
		envLog(env).Info("Using podman as container runtime", "path", p)
		found = p
	} else {
		for _, p := range dockerSearchPaths {
			if isExecutableFile(p) {
				envLog(env).Info("Found docker at non-PATH location", "path", p)
				found = p
				break
			}
		}
	}
	dockerExecutable.path = found
	return found
}

// DockerRuntimeName is the user facing runtime name for the CLI at a path
func DockerRuntimeName(executable string) string {
	if strings.Contains(strings.ToLower(filepath.Base(executable)), "podman") {
		return "Podman"
	}
	return "Docker"
}

// DockerRuntimeStartHint says how to bring the runtime back up
func DockerRuntimeStartHint(executable string) string {
	if DockerRuntimeName(executable) != "Podman" {
		return "start Docker and retry"
	}
	return "run `podman machine start` and retry"
}

// Lossy coercion into a docker label safe value
func sanitizeLabelValue(value string) string {
	if value == "" {
		return "unknown"
	}
	out := labelValueBadRe.ReplaceAllString(value, "_")
	if len(out) > 63 {
		out = out[:63]
	}
	if out == "" {
		return "unknown"
	}
	return out
}

// Profile label used for container reuse and orphan reaping
func dockerContainerIdentity(env *Env, sharedKey string) string {
	if sharedKey == "" {
		return sanitizeLabelValue(homeProfileName(env))
	}
	digest := sha256.Sum256([]byte(sharedKey))
	label := sanitizeLabelValue(sharedKey)
	if len(label) > 50 {
		label = label[:50]
	}
	return label + "-" + hex.EncodeToString(digest[:])[:12]
}

// Output of a docker CLI query, nil with a debug line when it fails or exits non zero
func dockerQuery(ctx context.Context, env *Env, argv []string, timeout time.Duration, fail string, nonzero string) *CaptureResult {
	res, err := runCapture(ctx, argv, timeout, false, nil)
	if err != nil {
		envLog(env).Debug(fail, "error", err)
		return nil
	}
	if nonzero != "" && res.Code != 0 {
		envLog(env).Debug(nonzero, "code", res.Code, "stderr", strings.TrimSpace(res.Stderr))
		return nil
	}
	return res
}

// Characters a docker label value cannot carry
var labelValueBadRe = regexp.MustCompile(`[^A-Za-z0-9_.-]`)
