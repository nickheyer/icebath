package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config, backends func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Terminal",
		About: "Where the agent's commands run and how its background processes are kept",
		Settings: []config.Setting{
			{Key: Backend, Title: "Where commands run", About: "The machine or sandbox where the agent's shell commands run", Default: "local", Env: []string{"TERMINAL_ENV"}, Options: backends},
			{Key: Cwd, Title: "Working folder", About: "Folder the agent's shell commands start in; . starts them in the session's folder", Default: ".", Env: []string{"TERMINAL_CWD"}},
			{Key: Timeout, Title: "Command timeout", About: "Seconds a foreground command may run before it is stopped", Default: 3 * time.Minute, Env: []string{"TERMINAL_TIMEOUT"}},
			{Key: Lifetime, Title: "Sandbox idle limit", About: "Seconds an unused shell or sandbox is kept before it is shut down", Default: 5 * time.Minute,
				Min: config.Bound(0), Env: []string{"TERMINAL_LIFETIME_SECONDS"}},
			{Key: Degraded, Title: "Backend failure mode", About: "What happens when the place commands run cannot be reached", Default: DegradedWarn, Env: []string{"TERMINAL_DEGRADED_MODE"}, Options: config.Options(
				config.Option{Value: DegradedWarn, Title: "Warn", About: "Report the problem and keep the session going"},
				config.Option{Value: DegradedFail, Title: "Fail", About: "Stop the call with an error"},
			)},
			{Key: EnvPassthrough, Title: "Passthrough env names", About: "Extra variable names commands may see from your environment; provider keys are refused here", Default: []string{}, Env: []string{"TERMINAL_ENV_PASSTHROUGH"}},
			{Key: ClientEnvPassthrough, Title: "Client variables", About: "Extra variable names copied from the connecting terminal into its sessions' commands, besides the toolchain (PATH, virtualenvs, Go, Rust, Java, Conda, Node, pyenv); names only, secrets are filtered out", Default: []string{}},
			{Key: OneshotWait, Title: "One-shot wait limit", About: "Longest wait in seconds for a one-shot run's background commands that notify when they finish, before it exits; 0 exits at once", Default: 10 * time.Minute},
			{Key: LocalMemoryMax, Title: "Worker memory cap", About: "Most memory in MB a background command of a server run under systemd may use in its own scope; 0 uses half of this machine's memory, at most 4 GB, and it never exceeds this machine's own limit", Default: 0, Env: []string{"TERMINAL_LOCAL_MEMORY_MAX_MB"}},
			{Key: CredentialFiles, Title: "Credential files", About: "Files under the profile folder copied into remote sandboxes, as paths relative to it", Default: []string{}},
			{Key: HomeMode, Title: "Home folder mode", About: "Which home folder commands see", Default: HomeAuto, Env: []string{"TERMINAL_HOME_MODE"}, Options: config.Options(
				config.Option{Value: HomeAuto, Title: "Automatic", About: "Pick the safest home for where commands run"},
				config.Option{Value: HomeProfile, Title: "Profile home", About: "Use the home folder inside the profile folder"},
				config.Option{Value: HomeReal, Title: "Real home", About: "Use your own home folder"},
			)},
			{Key: TempDir, Title: "Temp folder", About: "Folder commands use for temporary files; empty uses the system's", Default: "", Env: []string{"TERMINAL_TEMP_DIR"}},
			{Key: SandboxDir, Title: "Sandbox folder", About: "Host folder where sandbox files are stored; empty uses the sandboxes folder of the profile", Default: "", Env: []string{"TERMINAL_SANDBOX_DIR"}},
			{Key: TermGrace, Title: "Stop grace period", About: "Seconds to wait before force-killing a background process that was asked to stop; 0 kills it at once", Default: 2 * time.Second, Env: []string{"TERMINAL_DAEMON_TERM_GRACE_SECONDS"}},
			{Key: ContainerCPU, Title: "Sandbox CPUs", About: "CPU cores given to a container or cloud sandbox", Default: 1.0, Env: []string{"TERMINAL_CONTAINER_CPU"}},
			{Key: ContainerMemory, Title: "Sandbox memory", About: "Megabytes of memory given to a container or cloud sandbox", Default: 5120, Env: []string{"TERMINAL_CONTAINER_MEMORY"}},
			{Key: ContainerDisk, Title: "Sandbox disk", About: "Megabytes of disk given to a container or cloud sandbox", Default: 51200, Env: []string{"TERMINAL_CONTAINER_DISK"}},
			{Key: ContainerPersistent, Title: "Keep sandbox files", About: "Keep a sandbox's files between sessions", Default: true, Env: []string{"TERMINAL_CONTAINER_PERSISTENT"}},
			{Key: ContainerProgram, Title: "Container program", About: "Path of the docker or podman program container sandboxes run and build their images with; empty finds docker, then podman, on PATH and in the usual install places", Default: "", Env: []string{"ICEBATH_DOCKER_BINARY"}},
			{Key: SyncBackMax, Title: "Sync-back size cap", About: "Largest archive in bytes copied back from a remote sandbox", Default: int64(2 << 30)},
			{Key: SudoPassword, Title: "Sudo password", About: "Password sudo is given in commands; empty asks once per session", Default: "", Env: []string{"SUDO_PASSWORD"}, Secret: true},
			{Key: Notifications, Title: "Background job notices", About: "What you are told when a background command produces output or ends", Default: NotifyConcise, Options: config.Options(
				config.Option{Value: NotifyConcise, Title: "Concise", About: "Short summary when it finishes"},
				config.Option{Value: NotifyAll, Title: "All", About: "Every update with its output"},
				config.Option{Value: NotifyResult, Title: "Result", About: "Only the final result"},
				config.Option{Value: NotifyError, Title: "Errors", About: "Only when it fails"},
				config.Option{Value: NotifyOff, Title: "Off", About: "No notices, and none reach the agent"},
			)},
		},
	}})
}
