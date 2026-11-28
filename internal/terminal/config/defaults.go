package config

import (
	"time"

	"github.com/nickheyer/icebath/internal/config"
)

func Register(cfg *config.Config, backends func() []config.Option) {
	cfg.Register(config.Sections{{
		Key:   Key,
		Title: "Sandbox",
		About: "Where the agent's commands run",
		Group: config.GroupAbilities,
		Order: 20,
		Settings: []config.Setting{
			{Key: Backend, Title: "Where commands run", About: "Which computer or sandbox the agent's commands run in.", Default: "local",
				Env: []string{"TERMINAL_ENV"}, Options: backends, Slash: "sandbox", Custom: true},
			{Key: Cwd, Title: "Working folder", About: "The folder that the agent's commands start in.",
				Help: "Leave it empty, or write a dot, to start them in the folder of the session. It is a folder of the place where they run, so " +
					"over SSH ~ is the home folder there.", Default: "", Zero: "Session's folder", Env: []string{"TERMINAL_CWD"}},
			{Key: HomeMode, Title: "Home folder", About: "Which home folder the agent's commands see.", Default: HomeAuto, Env: []string{"TERMINAL_HOME_MODE"},
				Options: config.Options(
					config.Option{Value: HomeAuto, Title: "Automatic", About: "The profile's home when icebath runs in a container, else yours"},
					config.Option{Value: HomeProfile, Title: "Profile home", About: "A home folder kept in the profile's folder"},
					config.Option{Value: HomeReal, Title: "Your home", About: "Your own home folder"},
				)},
			{Key: ContainerPersistent, Title: "Keep sandbox files", About: "Keep the files of a sandbox from one session to the next.", Default: true,
				Env: []string{"TERMINAL_CONTAINER_PERSISTENT"}},
			{Key: Notifications, Title: "Background notices", About: "What you are told when a background command prints output or ends.",
				Default: NotifyConcise, Options: config.Options(
					config.Option{Value: NotifyConcise, Title: "Short", About: "A short summary when the command ends"},
					config.Option{Value: NotifyAll, Title: "All", About: "Every update, with its output"},
					config.Option{Value: NotifyResult, Title: "Result", About: "Only the final result"},
					config.Option{Value: NotifyError, Title: "Errors", About: "Only when the command fails"},
					config.Option{Value: NotifyOff, Title: "Off", About: "No notices, for you or the agent"},
				)},
			{Key: SudoPassword, Title: "Sudo password", About: "Your password for sudo in the agent's commands.",
				Help: "When none is kept, you are asked once per session.", Default: "", Env: []string{"SUDO_PASSWORD"}, Secret: true},
			{Key: EnvPassthrough, Title: "Shared variables", About: "Variables from your environment that commands may see although they look secret.",
				Help: "API keys of your providers are refused here.", Default: []string{}, Env: []string{"TERMINAL_ENV_PASSTHROUGH"}},
			{Key: ClientEnvPassthrough, Title: "Client variables", About: "Variables copied from the terminal you connect from into the commands of its sessions.",
				Help: "PATH and the variables of virtualenvs, Go, Rust, Java, Conda, Node and pyenv come along anyway. " +
					"Secrets are filtered out.", Default: []string{}, Apply: config.NewSessions, Advanced: true},
			{Key: CredentialFiles, Title: "Files for sandboxes", About: "Files in the profile's folder that are copied into remote sandboxes.",
				Help: "Paths relative to the profile's folder.", Default: []string{}, Advanced: true},
			{Key: Timeout, Title: "Command time limit", About: "How long a command may run when the agent sets no limit of its own.",
				Default: 3 * time.Minute, Env: []string{"TERMINAL_TIMEOUT"}, Advanced: true},
			{Key: Lifetime, Title: "Idle sandbox limit", About: "How long an unused shell or sandbox is kept before it is shut down.",
				Default: 5 * time.Minute, Min: config.Bound(0), Zero: "Right away", Env: []string{"TERMINAL_LIFETIME_SECONDS"}, Advanced: true},
			{Key: Degraded, Title: "When unreachable", About: "What happens when the place where commands run cannot be reached.",
				Default: DegradedWarn, Env: []string{"TERMINAL_DEGRADED_MODE"}, Options: config.Options(
					config.Option{Value: DegradedWarn, Title: "Warn", About: "Tell the agent and keep the session going"},
					config.Option{Value: DegradedFail, Title: "Fail", About: "Stop the command with an error"},
				), Advanced: true},
			{Key: TermGrace, Title: "Stop grace period", About: "How long a background process asked to stop gets before it is killed.",
				Default: 2 * time.Second, Zero: "Kill at once", Env: []string{"TERMINAL_DAEMON_TERM_GRACE_SECONDS"}, Advanced: true},
			{Key: OneshotWait, Title: "Single-question wait", About: "How long a single-question run waits for background commands to finish.",
				Help: "Only commands that report when they end are waited for.", Default: 10 * time.Minute, Zero: "No wait", Advanced: true},
			{Key: ContainerCPU, Title: "Sandbox CPUs", About: "How many CPU cores a container or cloud sandbox gets.", Default: 1.0, Counts: "cores",
				Env: []string{"TERMINAL_CONTAINER_CPU"}, Advanced: true},
			{Key: ContainerMemory, Title: "Sandbox memory", About: "How much memory a container or cloud sandbox gets.", Default: 5120, Counts: "MB",
				Env: []string{"TERMINAL_CONTAINER_MEMORY"}, Advanced: true},
			{Key: ContainerDisk, Title: "Sandbox disk", About: "How much disk a container or cloud sandbox gets.", Default: 51200, Counts: "MB",
				Env: []string{"TERMINAL_CONTAINER_DISK"}, Advanced: true},
			{Key: LocalMemoryMax, Title: "Background memory cap", About: "The most memory that one background command may use.",
				Help: "Applies when icebath runs as a systemd service on Linux. Automatic is half of this computer's memory, at most 4 GB.", Default: 0,
				Zero: "Automatic", Counts: "MB", Least: config.Num(64), Env: []string{"TERMINAL_LOCAL_MEMORY_MAX_MB"}, Advanced: true},
			{Key: SyncBackMax, Title: "Copy-back limit", About: "The largest archive of files copied back from a remote sandbox.",
				Help: "A larger archive is not copied back.", Default: int64(2 << 30), Zero: "Nothing copied back", Counts: config.Bytes, Advanced: true},
			{Key: TempDir, Title: "Temp folder", About: "The folder that commands on this computer use for temporary files.", Default: "", Zero: "System temp folder",
				Path: &config.Path{Kind: config.PathFolder, Exists: true},
				Env:  []string{"TERMINAL_TEMP_DIR"}, Advanced: true},
			{Key: SandboxDir, Title: "Sandbox folder", About: "The folder of this computer where sandbox files are kept.", Default: "",
				Path: &config.Path{Kind: config.PathFolder},
				Zero: "Profile's sandboxes folder", Env: []string{"TERMINAL_SANDBOX_DIR"}, Advanced: true},
			{Key: ContainerProgram, Title: "Container program", About: "The docker or podman program that runs and builds container sandboxes.",
				Path: &config.Path{Kind: config.PathProgram, Exists: true},
				Help: "When not set, docker and then podman are looked for on PATH and in the usual places.", Default: "", Zero: "Found by itself",
				Env: []string{"ICEBATH_DOCKER_BINARY"}, Advanced: true},
		},
	}})
}
