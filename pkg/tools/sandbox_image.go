//go:build !icebath_core

package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/daytona/clients/sdk-go/pkg/daytona"
	modal "github.com/modal-labs/modal-client/go"
	"github.com/nickheyer/icebath/docker/sandbox"
	"github.com/nickheyer/icebath/pkg/config"
)

// Longest a sandbox image build may run
const sandboxBuildTimeout = 45 * time.Minute

var sandboxBuildMu sync.Mutex

// Build step headers of BuildKit plain progress and of the Podman and classic builders
var sandboxBuildStep = regexp.MustCompile(`^(#\d+ \[[^\]]*\d+/\d+\] .*|STEP \d+/\d+: .*|Step \d+/\d+ : .*)$`)

// Progress sink logging build step headlines at info and every other output line at debug
func sandboxBuildLogger(env *Env) func(string, bool) {
	log := envLog(env)
	return func(line string, headline bool) {
		if headline {
			log.Info(line)
		} else {
			log.Debug(line)
		}
	}
}

// Whether the image is the sandbox icebath builds itself
func IsDefaultSandboxImage(image string) bool {
	return strings.TrimSpace(image) == config.DefaultSandboxImage
}

// Dockerfile digest the local image carries, empty when it is absent or unlabelled
func sandboxImageDigest(ctx context.Context, docker, image string) string {
	res, err := runCapture(ctx, []string{docker, "image", "inspect", image, "--format", `{{index .Config.Labels "` + sandbox.DigestLabel + `"}}`}, 30*time.Second, false, nil)
	if err != nil || res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// Builds the default sandbox image from the embedded Dockerfile unless the local one already matches it, other images untouched
func EnsureSandboxImage(ctx context.Context, docker, image string, progress func(line string, headline bool)) error {
	if !IsDefaultSandboxImage(image) {
		return nil
	}
	digest := sandbox.Digest()
	if sandboxImageDigest(ctx, docker, image) == digest {
		return nil
	}
	sandboxBuildMu.Lock()
	defer sandboxBuildMu.Unlock()
	if sandboxImageDigest(ctx, docker, image) == digest {
		return nil
	}
	if progress == nil {
		progress = func(string, bool) {}
	}
	argv := []string{docker, "build"}
	if !strings.Contains(strings.ToLower(filepath.Base(docker)), "podman") {
		argv = append(argv, "--progress=plain")
	}
	argv = append(argv, "--label", sandbox.DigestLabel+"="+digest, "-t", image, "-")
	progress(fmt.Sprintf("Building the sandbox image %s from the Dockerfile icebath ships", image), true)
	buildCtx, cancel := context.WithTimeout(ctx, sandboxBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(buildCtx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(sandbox.Dockerfile)
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("building the sandbox image %s: %w", image, err)
	}
	var tail []string
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		tail = append(tail, line)
		if len(tail) > 20 {
			tail = tail[1:]
		}
		progress(line, sandboxBuildStep.MatchString(line))
	}
	if err := cmd.Wait(); err != nil {
		if buildCtx.Err() != nil {
			return fmt.Errorf("building the sandbox image %s did not finish within %s", image, sandboxBuildTimeout)
		}
		return fmt.Errorf("building the sandbox image %s failed: %w\n%s", image, err, strings.Join(tail, "\n"))
	}
	if got := sandboxImageDigest(ctx, docker, image); got != digest {
		return fmt.Errorf("building the sandbox image %s left an image labelled %q, want %s", image, got, digest)
	}
	progress(fmt.Sprintf("Sandbox image %s is ready", image), true)
	return nil
}

// Base image and the remaining Dockerfile commands of the embedded sandbox Dockerfile
func sandboxBaseAndCommands() (string, []string, error) {
	steps, err := sandbox.Instructions()
	if err != nil {
		return "", nil, err
	}
	commands := make([]string, 0, len(steps)-1)
	for _, step := range steps[1:] {
		commands = append(commands, step.Keyword+" "+step.Args)
	}
	return steps[0].Args, commands, nil
}

// Modal image of the default sandbox, built by Modal from the embedded Dockerfile
func modalSandboxImage(mc *modal.Client) (*modal.Image, error) {
	base, commands, err := sandboxBaseAndCommands()
	if err != nil {
		return nil, err
	}
	return mc.Images.FromRegistry(base, nil).DockerfileCommands(commands, nil), nil
}

// Daytona image argument, the default sandbox becoming a Dockerfile build and any other name passing through
func daytonaSandboxImage(image string) any {
	if IsDefaultSandboxImage(image) {
		return daytona.FromDockerfile(sandbox.Dockerfile)
	}
	return image
}

// Apptainer definition equal to the embedded Dockerfile, its ENV exported in both build and run
func apptainerSandboxDefinition() (string, error) {
	steps, err := sandbox.Instructions()
	if err != nil {
		return "", err
	}
	var env, post []string
	for _, step := range steps[1:] {
		switch step.Keyword {
		case "ENV":
			for _, pair := range strings.Fields(step.Args) {
				env = append(env, "export "+pair)
			}
			post = append(post, "export "+step.Args)
		case "RUN":
			post = append(post, "( "+step.Args+" )")
		default:
			return "", fmt.Errorf("the sandbox Dockerfile instruction %s has no Apptainer definition form", step.Keyword)
		}
	}
	return "Bootstrap: docker\nFrom: " + apptainerBaseRef(steps[0].Args) + "\n\n%environment\n    " + strings.Join(env, "\n    ") +
		"\n\n%post\n    set -e\n    " + strings.Join(post, "\n    ") + "\n", nil
}

// Base image reference Apptainer accepts, the tag dropped where a digest pins the image
func apptainerBaseRef(ref string) string {
	name, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ref
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	return name + "@" + digest
}

// Cached SIF of the default sandbox, converted from the local docker build or built from the equal Apptainer definition
func apptainerSandboxSIF(ctx context.Context, env *Env, executable string) (string, error) {
	cacheDir, err := apptainerCacheDir(env)
	if err != nil {
		return "", err
	}
	sifPath := filepath.Join(cacheDir, "icebath-sandbox-"+sandbox.Digest()[:16]+".sif")
	if _, err := os.Stat(sifPath); err == nil {
		return sifPath, nil
	}
	singularitySIFBuildMu.Lock()
	defer singularitySIFBuildMu.Unlock()
	if _, err := os.Stat(sifPath); err == nil {
		return sifPath, nil
	}
	tmpDir := filepath.Join(cacheDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", err
	}
	buildEnv := map[string]string{}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			buildEnv[k] = v
		}
	}
	buildEnv["APPTAINER_TMPDIR"] = tmpDir
	buildEnv["APPTAINER_CACHEDIR"] = cacheDir
	argv := []string{executable, "build", sifPath, "docker-daemon://" + config.DefaultSandboxImage}
	if docker := FindDocker(env); docker != "" {
		if err := EnsureSandboxImage(ctx, docker, config.DefaultSandboxImage, sandboxBuildLogger(env)); err != nil {
			return "", err
		}
	} else {
		definition, err := apptainerSandboxDefinition()
		if err != nil {
			return "", err
		}
		defPath := filepath.Join(tmpDir, "icebath-sandbox.def")
		if err := os.WriteFile(defPath, []byte(definition), 0o644); err != nil {
			return "", err
		}
		defer os.Remove(defPath)
		argv = []string{executable, "build", "--fakeroot", sifPath, defPath}
	}
	envLog(env).Info("Building the sandbox SIF image (one-time setup)", "source", argv[len(argv)-1], "target", sifPath)
	res, err := runCapture(ctx, argv, sandboxBuildTimeout, false, buildEnv)
	if err != nil {
		return "", fmt.Errorf("building the sandbox SIF image: %w", err)
	}
	if res.Code != 0 {
		_ = os.Remove(sifPath)
		return "", fmt.Errorf("building the sandbox SIF image failed: %s", runePrefix(res.Stderr, 500))
	}
	envLog(env).Info("Sandbox SIF image built", "target", sifPath)
	return sifPath, nil
}
