package terminal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	dockerconfig "github.com/nickheyer/icebath/internal/plugins/defaults/terminals/docker/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

// Value of terminal.backend that picks the backend
const name = "docker"

// The Docker backend, and what it learned of the machine's runtime
type registrant struct {
	// Skills whose folders and needs a sandbox is handed
	skills *skills.Registry

	mu sync.Mutex
	// What the daemon supports
	cgroups, storage probe
	// Profiles whose stray containers this run swept
	reaped map[string]bool
	// Whether each program's daemon remaps users
	remaps map[string]bool
	// Sandbox folders a failed privilege strip was warned of
	warned map[string]bool
}

func Register(registry *terminal.Registry, skills *skills.Registry) {
	registry.Register(&registrant{skills: skills, reaped: map[string]bool{}, remaps: map[string]bool{}, warned: map[string]bool{}})
}

func (r *registrant) Backends() []terminal.Backend {
	return []terminal.Backend{{
		Name:      name,
		Title:     "Docker",
		About:     "Run commands in a hardened Docker container with persistent /root and /workspace sandboxes",
		Where:     "a Docker container (Linux)",
		Remote:    true,
		Container: true,
		Isolated:  true,
		Exposed:   exposed,
		Home:      terminal.SandboxProfile,
		Shared:    shared,
		Mount:     mount,
		Check:     check,
		Setup:     setup,
		Doctor:    r.doctor,
		Prepare:   r.prepare,
		Streams:   true,
		Switch:    r.imageSwitch,
		Decide:    decide,
		Create:    r.create,
	}}
}

// Image change the profile's sandboxes wait on, nil when the image was chosen or every sandbox runs it
func (r *registrant) imageSwitch(cfg *config.Config) *terminal.Switch {
	docker := sandbox.Docker(cfg)
	if docker == "" {
		return nil
	}
	image := cmp.Or(strings.TrimSpace(cfg.GetString(dockerconfig.Image)), sandbox.Image)
	current, containers := r.pendingSwitch(context.Background(), cfg, docker, image)
	if current == "" {
		return nil
	}
	return &terminal.Switch{Current: current, Target: image, Sandboxes: containers}
}

// Pins the image the sandboxes run, or with approve the new one they move to as they start again
func decide(cfg *config.Config, s *terminal.Switch, approve bool) (string, error) {
	image := s.Current
	if approve {
		image = s.Target
	}
	return image, cfg.Set(dockerconfig.Image, image)
}

// Key of the container the sessions of the profile working in dir share: the shared name's for every profile, else one per workspace
// when the working folder is mounted, else the profile's; one per session when sandbox files are not kept
func shared(cfg *config.Config, dir string) string {
	if !cfg.GetBool(terminalconfig.ContainerPersistent) {
		return ""
	}
	if key := strings.TrimSpace(cfg.GetString(dockerconfig.SharedContainerKey)); key != "" {
		return "shared:" + key
	}
	if cfg.GetBool(dockerconfig.MountCwd) && dir != "" {
		return workspaceKey(dir)
	}
	return "default"
}

// Why Docker can't run here: no program, or a daemon that doesn't answer
func check(cfg *config.Config) error {
	docker := sandbox.Docker(cfg)
	if docker == "" {
		return errors.New("Docker is not installed — no docker executable in PATH or the usual install locations")
	}
	captured, err := terminal.Capture(context.Background(), []string{docker, "version"}, 5*time.Second, nil)
	if err != nil {
		return fmt.Errorf("Docker is installed but the Docker daemon is not running (`%s version` failed: %v)", docker, err)
	}
	if captured.Code != 0 {
		return fmt.Errorf("Docker is installed but the Docker daemon is not running (`%s version` exited with code %d)", docker, captured.Code)
	}
	return nil
}

// Hint of the failures a command reads: switching away from Docker
const switchHint = "pick another backend with /backends, or Where commands run in /settings under Terminal"

// The program when it and its daemon answer, else a connection error saying what to do
func available(ctx context.Context, cfg *config.Config) (string, error) {
	unavailable := func(log, reason, hint string) (string, error) {
		slog.Error(log)
		return "", terminal.NewConnectionError(reason, hint)
	}
	docker := sandbox.Docker(cfg)
	if docker == "" {
		return unavailable("Docker backend selected but no docker executable was found in PATH or known install locations. Install Docker Desktop and ensure "+
			"the CLI is available.", "Docker executable not found in PATH or known install locations. Install Docker and ensure the 'docker' command is available.",
			"Install Docker (or fix PATH) and retry, or "+switchHint+".")
	}
	captured, err := terminal.Capture(ctx, []string{docker, "version"}, 5*time.Second, nil)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		return unavailable("Docker backend selected but '"+docker+" version' timed out. The Docker daemon may not be running.",
			"Docker daemon is not responding. Ensure Docker is running and try again.",
			"Start Docker (e.g. `systemctl start docker` or launch Docker Desktop), then retry — or "+switchHint+".")
	case err != nil:
		return unavailable("Docker backend selected but the resolved docker executable '"+docker+"' could not be executed.",
			"Docker executable could not be executed. Check your Docker installation.", "Repair the Docker installation and retry.")
	case captured.Code != 0:
		return unavailable(fmt.Sprintf("Docker backend selected but '%s version' failed (exit code %d, stderr=%s)", docker, captured.Code, strings.TrimSpace(captured.Stderr)),
			"Docker command is available but 'docker version' failed. Check your Docker installation.",
			"Start Docker, or add your user to the docker group, then retry — or "+switchHint+".")
	}
	return docker, nil
}

// What the user does to make the backend usable
func setup(*config.Config) []string {
	return []string{
		"Install Docker Desktop or Podman and make sure the CLI is on PATH (Docker: https://docs.docker.com/get-docker/, Podman: https://podman.io/docs/installation), " +
			"or point Container program in /settings under Terminal at it.",
		"Start the daemon (Docker Desktop, `systemctl start docker`, or `podman machine start`).",
		"Choose the sandbox image with Container image in /settings under Terminal → Docker; icebath builds its own default image the first time a " +
			"sandbox starts, or when the backend is readied.",
	}
}

// The runtime, whether its daemon answers, the image, and a switch of image waiting for the user
func (r *registrant) doctor(cfg *config.Config) []terminal.Check {
	docker := sandbox.Docker(cfg)
	if docker == "" {
		return []terminal.Check{{Label: "Docker or Podman not installed", Detail: "(needed for the 'docker' terminal backend)"}}
	}
	runtime := runtimeName(docker)
	var checks []terminal.Check
	if captured, err := terminal.Capture(context.Background(), []string{docker, "version"}, 10*time.Second, nil); err != nil || captured.Code != 0 {
		label := runtime + " not reachable"
		if runtime == "Docker" {
			label = runtime + " daemon not running"
		}
		checks = append(checks, terminal.Check{Label: label, Detail: "(needed for the 'docker' terminal backend; " + startHint(docker) + ")"})
	} else {
		detail := "(reachable)"
		if runtime == "Docker" {
			detail = "(daemon running)"
		}
		checks = append(checks, terminal.Check{OK: true, Label: runtime, Detail: detail})
	}
	image := cmp.Or(strings.TrimSpace(cfg.GetString(dockerconfig.Image)), sandbox.Image)
	checks = append(checks, terminal.Check{OK: true, Label: "Docker Image", Detail: "(" + image + ")"})
	if current, containers := r.pendingSwitch(context.Background(), cfg, docker, image); current != "" {
		checks = append(checks, terminal.Check{Label: "Sandbox image switch pending", Detail: fmt.Sprintf("(%s run %s while the image is now %s: set "+
			"Container image in /settings under Terminal → Docker to %s to switch, files in /root and /workspace carrying over, or to %s to keep it)",
			strings.Join(containers, ", "), current, image, image, current)})
	}
	return checks
}

// Image the profile's sandboxes run that the default image has since changed from, the one most of them run, and the containers on
// another image; none when the image was chosen, which replaces them as they start
func (r *registrant) pendingSwitch(ctx context.Context, cfg *config.Config, docker, image string) (string, []string) {
	if pinned(cfg) {
		return "", nil
	}
	shared := strings.TrimSpace(cfg.GetString(dockerconfig.SharedContainerKey))
	listing := query(ctx, []string{docker, "ps", "-a", "--filter", "label=" + agentLabel + "=1", "--filter", "label=" + profileLabel + "=" + identity(cfg, shared),
		"--format", "{{.Names}}\t{{.Image}}"}, 10*time.Second, "sandbox image switch probe failed", "docker ps exited")
	if listing == nil {
		return "", nil
	}
	stale := map[string][]string{}
	var order []string
	for _, line := range strings.Split(strings.TrimSpace(listing.Stdout), "\n") {
		container, running, _ := strings.Cut(line, "\t")
		if running == "" || running == image {
			continue
		}
		if _, seen := stale[running]; !seen {
			order = append(order, running)
		}
		stale[running] = append(stale[running], container)
	}
	if len(order) == 0 {
		return "", nil
	}
	current := order[0]
	var containers []string
	for _, running := range order {
		if len(stale[running]) > len(stale[current]) {
			current = running
		}
		containers = append(containers, stale[running]...)
	}
	slices.Sort(containers)
	return current, containers
}

// Finds the runtime and readies the image: icebath's own built from its Dockerfile, another pulled when it isn't here
func (r *registrant) prepare(ctx context.Context, cfg *config.Config, say func(line string)) error {
	docker := sandbox.Docker(cfg)
	if docker == "" {
		say("Docker or Podman not found in PATH!")
		say("Install Docker: https://docs.docker.com/get-docker/ or Podman: https://podman.io/docs/installation")
		return errors.New("Docker or Podman is not installed.")
	}
	say(runtimeName(docker) + " found: " + docker)
	if _, err := available(ctx, cfg); err != nil {
		if broken, ok := errors.AsType[*terminal.ConnectionError](err); ok {
			return fmt.Errorf("%s %s", broken.Reason, broken.Hint)
		}
		return err
	}
	image := cmp.Or(strings.TrimSpace(cfg.GetString(dockerconfig.Image)), sandbox.Image)
	if sandbox.Default(image) {
		err := sandbox.Ensure(ctx, docker, image, func(line string, headline bool) {
			buildLog(line, headline)
			if headline {
				say("  " + line)
			}
		})
		if err != nil {
			return fmt.Errorf("%w; the first Docker sandbox start builds it again", err)
		}
		say("Sandbox image " + image + " ready")
		return nil
	}
	say("Pulling " + image + " unless it is here already")
	if !imageReady(ctx, docker, image) {
		return fmt.Errorf("The image %s could not be pulled; check its name in Container image under Terminal → Docker.", image)
	}
	say("Sandbox image " + image + " ready")
	return nil
}
