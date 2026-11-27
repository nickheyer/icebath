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
		About:     "A locked-down Docker container on this computer",
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
		return errors.New("Docker is not installed: no docker program is on your PATH or in the usual places. Install Docker or Podman, or set " +
			"Container program in /settings under Terminal.")
	}
	runtime := runtimeName(docker)
	captured, err := terminal.Capture(context.Background(), []string{docker, "version"}, 5*time.Second, nil)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		return fmt.Errorf("%s is installed, but `%s version` got no answer within 5 seconds, so its daemon is not running. %s", runtime, docker,
			startHint(docker))
	case err != nil:
		return fmt.Errorf("%s is installed, but `%s version` could not run: %v. %s", runtime, docker, err, startHint(docker))
	case captured.Code != 0:
		return fmt.Errorf("%s is installed, but its daemon is not running: `%s version` ended with exit code %d. %s", runtime, docker,
			captured.Code, startHint(docker))
	}
	return nil
}

// Hint of the failures a command reads: switching away from Docker
const switchHint = "choose another place to run commands in /backends"

// The program when it and its daemon answer, else a connection error saying what to do
func available(ctx context.Context, cfg *config.Config) (string, error) {
	unavailable := func(log, reason, hint string) (string, error) {
		slog.Error(log)
		return "", terminal.NewConnectionError(reason, hint)
	}
	docker := sandbox.Docker(cfg)
	if docker == "" {
		return unavailable("Docker backend selected but no docker executable was found in PATH or known install locations. Install Docker Desktop and ensure "+
			"the CLI is available.", "Docker is not installed: no docker program is on your PATH or in the usual places.",
			"Install Docker or add it to your PATH, then try again, or "+switchHint+".")
	}
	captured, err := terminal.Capture(ctx, []string{docker, "version"}, 5*time.Second, nil)
	switch {
	case errors.Is(err, terminal.ErrTimedOut):
		return unavailable("Docker backend selected but '"+docker+" version' timed out. The Docker daemon may not be running.",
			"Docker does not answer: `"+docker+" version` got no answer within 5 seconds.",
			"Start Docker, such as with `systemctl start docker` or by opening Docker Desktop, then try again, or "+switchHint+".")
	case err != nil:
		return unavailable("Docker backend selected but the resolved docker executable '"+docker+"' could not be executed.",
			"The docker program at "+docker+" could not be run.", "Repair the Docker installation, then try again.")
	case captured.Code != 0:
		return unavailable(fmt.Sprintf("Docker backend selected but '%s version' failed (exit code %d, stderr=%s)", docker, captured.Code, strings.TrimSpace(captured.Stderr)),
			strings.TrimSpace(fmt.Sprintf("Docker is installed, but `%s version` failed with exit code %d. %s", docker, captured.Code, said(captured.Stderr))),
			"Start Docker, or add your user to the docker group, then try again, or "+switchHint+".")
	}
	return docker, nil
}

// What a program wrote to stderr as a sentence saying so, empty when it wrote nothing
func said(stderr string) string {
	stderr = strings.Join(strings.Fields(stderr), " ")
	if stderr == "" {
		return ""
	}
	if !strings.ContainsAny(stderr[len(stderr)-1:], ".!?") {
		stderr += "."
	}
	return "It said: " + stderr
}

// What the user does to make the backend usable
func setup(*config.Config) []string {
	return []string{
		"Install Docker from https://docs.docker.com/get-docker/ or Podman from https://podman.io/docs/installation, and make sure its program is " +
			"on your PATH. If it is somewhere else, set Container program in /settings under Terminal.",
		"Start it: open Docker Desktop, or run `systemctl start docker` or `podman machine start`.",
		"Choose the sandbox image with Container image in /settings under Terminal › Docker. icebath builds its own image the first time a " +
			"sandbox starts, or when you prepare Docker on this screen.",
	}
}

// The runtime, whether its daemon answers, the image, and a switch of image waiting for the user
func (r *registrant) doctor(cfg *config.Config) []terminal.Check {
	docker := sandbox.Docker(cfg)
	if docker == "" {
		return []terminal.Check{{Label: "Docker or Podman is not installed.", Detail: "Commands run in Docker only once one of them is."}}
	}
	runtime := runtimeName(docker)
	var checks []terminal.Check
	if captured, err := terminal.Capture(context.Background(), []string{docker, "version"}, 10*time.Second, nil); err != nil || captured.Code != 0 {
		label := runtime + " does not answer."
		if runtime == "Docker" {
			label = "The Docker daemon is not running."
		}
		checks = append(checks, terminal.Check{Label: label, Detail: startHint(docker)})
	} else {
		label := runtime + " answers."
		if runtime == "Docker" {
			label = "The Docker daemon is running."
		}
		checks = append(checks, terminal.Check{OK: true, Label: label})
	}
	image := cmp.Or(strings.TrimSpace(cfg.GetString(dockerconfig.Image)), sandbox.Image)
	checks = append(checks, terminal.Check{OK: true, Label: "Sandbox image:", Detail: image})
	if current, containers := r.pendingSwitch(context.Background(), cfg, docker, image); current != "" {
		checks = append(checks, terminal.Check{Label: "A new sandbox image is waiting.", Detail: fmt.Sprintf("%s still run %s, but the image is now %s. "+
			"Set Container image in /settings under Terminal › Docker to %s to move them, keeping the files in /root and /workspace, or to %s to keep "+
			"the old image.", strings.Join(containers, ", "), current, image, image, current)})
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
		say("Neither Docker nor Podman is on your PATH.")
		say("Install Docker from https://docs.docker.com/get-docker/ or Podman from https://podman.io/docs/installation.")
		return errors.New("Docker or Podman is not installed. Install one of them, then prepare Docker again.")
	}
	say("Found " + runtimeName(docker) + " at " + docker + ".")
	if _, err := available(ctx, cfg); err != nil {
		if broken, ok := errors.AsType[*terminal.ConnectionError](err); ok {
			return fmt.Errorf("%s %s", broken.Reason, broken.Hint)
		}
		return err
	}
	image := cmp.Or(strings.TrimSpace(cfg.GetString(dockerconfig.Image)), sandbox.Image)
	if sandbox.Default(image) {
		built := false
		err := sandbox.Ensure(ctx, docker, image, func(line string, headline bool) {
			buildLog(line, headline)
			if headline {
				built = true
				say("  " + line)
			}
		})
		if err != nil {
			return fmt.Errorf("%w\nThe first Docker sandbox to start tries the build again.", err)
		}
		if !built {
			say("The sandbox image " + image + " is ready.")
		}
		return nil
	}
	say("Getting the image " + image + ", unless it is here already.")
	if !imageReady(ctx, docker, image) {
		return fmt.Errorf("The image %s could not be downloaded. Check its name in /settings under Terminal › Docker › Container image.", image)
	}
	say("The sandbox image " + image + " is ready.")
	return nil
}
