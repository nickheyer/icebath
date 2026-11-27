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
		About:  "A locked-down Docker or Podman container on this computer",
		Order:  20,
		Settings: []config.Setting{
			{Key: Image, Title: "Container image", About: "The image that the sandbox starts from.",
				Help: "Left at the default, icebath builds its own image the first time the sandbox is used.", Default: sandbox.Image,
				Env: []string{"TERMINAL_DOCKER_IMAGE"}},
			{Key: Network, Title: "Network access", About: "Let the sandbox reach the network.", Help: "When off, the sandbox has no network at all.",
				Default: true, Env: []string{"TERMINAL_DOCKER_NETWORK"}},
			{Key: MountCwd, Title: "Mount working folder", About: "Mount the folder of the session into the sandbox at /workspace.",
				Help: "Risky commands then ask for approval again, as the sandbox can reach your files.", Default: false,
				Env: []string{"TERMINAL_DOCKER_MOUNT_CWD_TO_WORKSPACE"}},
			{Key: Volumes, Title: "Volumes", About: "Folders of this computer mounted into the sandbox.",
				Help: "Each written as host:container, such as ~/data:/data.", Default: []string{}, Env: []string{"TERMINAL_DOCKER_VOLUMES"}},
			{Key: ForwardEnv, Title: "Passed-in variables", About: "Variables from your environment that are passed into the sandbox.",
				Help: "Names only. The values come from your environment.", Default: []string{}, Env: []string{"TERMINAL_DOCKER_FORWARD_ENV"}},
			{Key: Env, Title: "Variables", About: "Variables set inside the sandbox.", Default: map[string]any{}, Env: []string{"TERMINAL_DOCKER_ENV"},
				Entry: &config.Entry{Noun: "variable", Name: "Variable name", Capitals: true, Fields: []config.Setting{
					{Title: "Value", About: "The text that the variable holds.", Default: ""},
				}}},
			{Key: RunAsHostUser, Title: "Run as your user", About: "Run commands in the sandbox as your own user instead of root.", Default: false,
				Env: []string{"TERMINAL_DOCKER_RUN_AS_HOST_USER"}},
			{Key: PersistAcrossProcesses, Title: "Reuse the sandbox", About: "Keep the sandbox running so later runs of icebath can use it again.",
				Default: true, Env: []string{"TERMINAL_DOCKER_PERSIST_ACROSS_PROCESSES"}},
			{Key: ImagePinned, Title: "Always use this image", About: "Replace sandboxes that run another image, even while the image is the default.",
				Help: "When off, such a sandbox is kept until you set the image yourself.", Default: false, Env: []string{"TERMINAL_DOCKER_IMAGE_PINNED"},
				Advanced: true},
			{Key: SharedContainerKey, Title: "Shared sandbox name", About: "A name that lets the sessions of every profile share one sandbox.",
				Help: "When sandbox files are not kept, each session gets its own sandbox instead.", Default: "", Zero: "One per profile",
				Env: []string{"TERMINAL_DOCKER_SHARED_CONTAINER_KEY"}, Advanced: true},
			{Key: ShmSize, Title: "Shared memory", About: "The size of the shared memory in the sandbox.", Help: "A size such as 1g or 512m.", Default: "1g",
				Zero: "Docker's default", Env: []string{"TERMINAL_DOCKER_SHM_SIZE"}, Advanced: true},
			{Key: ExtraArgs, Title: "Extra flags", About: "Flags added to the docker run command.", Default: []string{}, Env: []string{"TERMINAL_DOCKER_EXTRA_ARGS"},
				Advanced: true},
			{Key: OrphanReaper, Title: "Clean stray containers", About: "Remove sandboxes left over from earlier runs.", Default: true,
				Env: []string{"TERMINAL_DOCKER_ORPHAN_REAPER"}, Advanced: true},
			{Key: SnapCompat, Title: "Snap package mode", About: "Work around Docker installed as a snap package.",
				Help: "Runs the sandbox without --init and without no-new-privileges.", Default: false, Env: []string{"TERMINAL_DOCKER_SNAP_COMPAT"},
				Advanced: true},
		},
	}})
}
