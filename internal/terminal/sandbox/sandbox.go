// Image icebath builds for its container sandboxes, which every container backend starts from unless told otherwise
package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

//go:embed Dockerfile
var Dockerfile string

// Name of the image built from the Dockerfile
const Image = "localhost/icebath-sandbox:desktop"

// Image label holding the digest of the Dockerfile a build came from
const DigestLabel = "app.icebath.sandbox.dockerfile"

// Longest a build may take
const buildTimeout = 45 * time.Minute

// Whether the image is the one icebath builds
func Default(image string) bool {
	return strings.TrimSpace(image) == Image
}

// Places docker is installed outside PATH
var dockerPaths = []string{"/usr/local/bin/docker", "/opt/homebrew/bin/docker", "/Applications/Docker.app/Contents/Resources/bin/docker"}

// Whether the path is a regular file the user may run
func executable(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || st.Mode().Perm()&0o111 != 0
}

// Docker or podman program of the profile: the one the container program setting names when it can run, else docker, then podman, on
// PATH, else docker in the usual install places; empty when there is none
func Docker(cfg *config.Config) string {
	if program := strings.TrimSpace(cfg.GetString(terminalconfig.ContainerProgram)); program != "" {
		if home, err := os.UserHomeDir(); err == nil && (program == "~" || strings.HasPrefix(program, "~/")) {
			program = home + program[1:]
		}
		if executable(program) {
			slog.Debug("Using the container program setting", "path", program)
			return program
		}
	}
	if path, err := exec.LookPath("docker"); err == nil {
		return path
	}
	if path, err := exec.LookPath("podman"); err == nil {
		slog.Debug("Using podman as container runtime", "path", path)
		return path
	}
	for _, path := range dockerPaths {
		if executable(path) {
			slog.Debug("Found docker at non-PATH location", "path", path)
			return path
		}
	}
	return ""
}

// Whether the program is podman rather than docker
func Podman(program string) bool {
	return strings.Contains(strings.ToLower(filepath.Base(program)), "podman")
}

// Hex sha256 of the Dockerfile
func Digest() string {
	sum := sha256.Sum256([]byte(Dockerfile))
	return hex.EncodeToString(sum[:])
}

// Keyword and arguments of one Dockerfile instruction, continuation lines joined
type Instruction struct {
	Keyword string
	Args    string
}

// Instructions of the Dockerfile in order, its comments and blank lines dropped; the first is FROM
func Instructions() ([]Instruction, error) {
	var out []Instruction
	var pending strings.Builder
	for _, raw := range strings.Split(Dockerfile, "\n") {
		line := strings.TrimSpace(raw)
		if pending.Len() == 0 && (line == "" || strings.HasPrefix(line, "#")) {
			continue
		}
		if continued, ok := strings.CutSuffix(line, "\\"); ok {
			pending.WriteString(continued + " ")
			continue
		}
		pending.WriteString(line)
		keyword, args, _ := strings.Cut(pending.String(), " ")
		out = append(out, Instruction{Keyword: strings.ToUpper(keyword), Args: strings.TrimSpace(args)})
		pending.Reset()
	}
	if pending.Len() > 0 {
		return nil, errors.New("sandbox Dockerfile ends inside a continued line")
	}
	if len(out) == 0 || out[0].Keyword != "FROM" {
		return nil, errors.New("sandbox Dockerfile does not start with FROM")
	}
	return out, nil
}

// Base image and the rest of the Dockerfile as commands, for backends that build from a registry image
func Recipe() (string, []string, error) {
	steps, err := Instructions()
	if err != nil {
		return "", nil, err
	}
	commands := make([]string, 0, len(steps)-1)
	for _, step := range steps[1:] {
		commands = append(commands, step.Keyword+" "+step.Args)
	}
	return steps[0].Args, commands, nil
}

// Apptainer definition doing what the Dockerfile does, its ENV exported at build and at run
func Apptainer() (string, error) {
	steps, err := Instructions()
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
	return "Bootstrap: docker\nFrom: " + apptainerRef(steps[0].Args) + "\n\n%environment\n    " + strings.Join(env, "\n    ") +
		"\n\n%post\n    set -e\n    " + strings.Join(post, "\n    ") + "\n", nil
}

// Base image reference Apptainer takes, the tag dropped where a digest pins the image
func apptainerRef(ref string) string {
	name, digest, ok := strings.Cut(ref, "@")
	if !ok {
		return ref
	}
	if colon := strings.LastIndex(name, ":"); colon > strings.LastIndex(name, "/") {
		name = name[:colon]
	}
	return name + "@" + digest
}

// Build step headers of BuildKit's plain progress, Podman's and the classic builder's
var buildStep = regexp.MustCompile(`^(#\d+ \[[^\]]*\d+/\d+\] .*|STEP \d+/\d+: .*|Step \d+/\d+ : .*)$`)

var building sync.Mutex

// Digest of the Dockerfile the local image was built from, empty when it is absent or unlabelled
func builtDigest(ctx context.Context, docker, image string) string {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, docker, "image", "inspect", image, "--format", `{{index .Config.Labels "`+DigestLabel+`"}}`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Builds the default image with the docker or podman CLI unless the local one matches the Dockerfile already, other images left alone;
// progress gets each line of output, headline set for the build's steps
func Ensure(ctx context.Context, docker, image string, progress func(line string, headline bool)) error {
	if !Default(image) {
		return nil
	}
	digest := Digest()
	if builtDigest(ctx, docker, image) == digest {
		return nil
	}
	building.Lock()
	defer building.Unlock()
	if builtDigest(ctx, docker, image) == digest {
		return nil
	}
	if progress == nil {
		progress = func(string, bool) {}
	}
	argv := []string{"build"}
	if !Podman(docker) {
		argv = append(argv, "--progress=plain")
	}
	argv = append(argv, "--label", DigestLabel+"="+digest, "-t", image, "-")
	progress(fmt.Sprintf("Building the sandbox image %s from the Dockerfile icebath ships", image), true)
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, argv...)
	cmd.Stdin = strings.NewReader(Dockerfile)
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
		if tail = append(tail, line); len(tail) > 20 {
			tail = tail[1:]
		}
		progress(line, buildStep.MatchString(line))
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("building the sandbox image %s did not finish within %s", image, buildTimeout)
		}
		return fmt.Errorf("building the sandbox image %s failed: %w\n%s", image, err, strings.Join(tail, "\n"))
	}
	if got := builtDigest(context.Background(), docker, image); got != digest {
		return fmt.Errorf("building the sandbox image %s left an image labelled %q, want %s", image, got, digest)
	}
	progress(fmt.Sprintf("Sandbox image %s is ready", image), true)
	return nil
}
