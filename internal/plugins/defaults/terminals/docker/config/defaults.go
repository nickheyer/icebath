package config

import (
	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/terminal/sandbox"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: terminalconfig.Key,
		Title:  "Docker",
		About:  "Commands run in a hardened Docker or Podman container",
		Settings: []config.Setting{
			{Key: Image, Title: "Container image", About: "Image the Docker sandbox starts from; icebath builds its own default image the first time it is used", Default: sandbox.Image, Env: []string{"TERMINAL_DOCKER_IMAGE"}},
			{Key: ImagePinned, Title: "Enforce the image", About: "Replace sandboxes running another image with Container image even while it is icebath's default; otherwise such a sandbox is kept until you set the image yourself", Default: false, Env: []string{"TERMINAL_DOCKER_IMAGE_PINNED"}},
			{Key: Volumes, Title: "Docker volumes", About: "Folders mounted into Docker sandboxes, as host:container", Default: []string{}, Env: []string{"TERMINAL_DOCKER_VOLUMES"}},
			{Key: MountCwd, Title: "Mount working folder", About: "Mount the session's folder into the Docker sandbox at /workspace, which also makes risky commands ask for approval again", Default: false, Env: []string{"TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE"}},
			{Key: ForwardEnv, Title: "Docker forwarded variables", About: "Host variables passed into Docker sandboxes. Names only; the values come from your environment", Default: []string{}, Env: []string{"TERMINAL_DOCKER_FORWARD_ENV"}},
			{Key: Env, Title: "Docker variables", About: "Variables set inside Docker sandboxes, as name and value", Default: map[string]any{}, Env: []string{"TERMINAL_DOCKER_ENV"}},
			{Key: RunAsHostUser, Title: "Run as me", About: "Run Docker commands as your own user, not root", Default: false, Env: []string{"TERMINAL_DOCKER_RUN_AS_HOST_USER"}},
			{Key: ExtraArgs, Title: "Docker extra flags", About: "Extra flags added to the Docker run command", Default: []string{}, Env: []string{"TERMINAL_DOCKER_EXTRA_ARGS"}},
			{Key: ShmSize, Title: "Docker shared memory", About: "Shared memory size for Docker sandboxes, such as 1g; empty or 0 keeps Docker's own", Default: "1g", Env: []string{"TERMINAL_DOCKER_SHM_SIZE"}},
			{Key: Network, Title: "Docker network", About: "Let Docker sandboxes reach the network; off runs them air-gapped", Default: true, Env: []string{"TERMINAL_DOCKER_NETWORK"}},
			{Key: PersistAcrossProcesses, Title: "Reuse Docker sandbox", About: "Keep a Docker sandbox alive so later runs of icebath can reuse it", Default: true, Env: []string{"TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES"}},
			{Key: SharedContainerKey, Title: "Shared sandbox name", About: "Name that lets the sessions of every profile share one Docker sandbox; empty keeps one per profile, or one per session when sandbox files are not kept", Default: "", Env: []string{"TERMINAL_DOCKER_SHARED_CONTAINER_KEY"}},
			{Key: OrphanReaper, Title: "Clean stray containers", About: "Remove leftover Docker sandboxes of earlier runs", Default: true, Env: []string{"TERMINAL_DOCKER_ORPHAN_REAPER"}},
			{Key: SnapCompat, Title: "Snap Docker mode", About: "Work around Docker installed as a snap package: no --init and no no-new-privileges", Default: false, Env: []string{"TERMINAL_DOCKER_SNAP_COMPAT"}},
		},
	}})
}
