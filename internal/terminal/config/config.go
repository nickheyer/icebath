package config

const (
	Key = "terminal"
	// Backend the agent's commands run on
	Backend = Key + ".backend"
	// Folder commands start in, the session's folder for one of the placeholders
	Cwd = Key + ".cwd"
	// Seconds a foreground command may run
	Timeout = Key + ".timeout"
	// Seconds an unused environment is kept before it is cleaned up
	Lifetime = Key + ".lifetime_seconds"
	// What a call gets when the backend can't be reached
	Degraded = Key + ".degraded_mode"
	// Names of variables commands may see although they look like credentials
	EnvPassthrough = Key + ".env_passthrough"
	// Names of variables of the client's shell a session's commands take, besides its toolchain
	ClientEnvPassthrough = Key + ".client_env_passthrough"
	// Seconds a one-shot session waits for its background processes that notify on completion before it ends
	OneshotWait = Key + ".oneshot_completion_wait_seconds"
	// Megabytes of memory a background command's systemd scope may use, 0 for the machine's own bound
	LocalMemoryMax = Key + ".local_memory_max_mb"
	// Files under the profile folder copied into remote sandboxes
	CredentialFiles = Key + ".credential_files"
	// Which home folder commands see
	HomeMode = Key + ".home_mode"
	// Folder commands keep their temporary files in
	TempDir = Key + ".temp_dir"
	// Host folder sandbox files are kept in
	SandboxDir = Key + ".sandbox_dir"
	// Seconds between asking a background process to stop and killing it
	TermGrace = Key + ".daemon_term_grace_seconds"
	// Resources and persistence of container and cloud sandboxes
	ContainerCPU        = Key + ".container_cpu"
	ContainerMemory     = Key + ".container_memory"
	ContainerDisk       = Key + ".container_disk"
	ContainerPersistent = Key + ".container_persistent"
	// docker or podman program container sandboxes run and build their images with
	ContainerProgram = Key + ".container_program"
	// Largest archive copied back from a remote sandbox
	SyncBackMax = Key + ".sync_back_max_bytes"
	// Password sudo is fed, asked for once per session when empty
	SudoPassword = Key + ".sudo_password"
	// How much the user is told of background processes, and whether the agent is
	Notifications = Key + ".notifications"
)

// Values of Cwd that name no folder, the session's being used
var Placeholders = []string{"", ".", "auto", "cwd"}

const (
	DegradedWarn = "warn"
	DegradedFail = "fail"
)

const (
	HomeAuto    = "auto"
	HomeProfile = "profile"
	HomeReal    = "real"
)

const (
	NotifyConcise = "concise"
	NotifyAll     = "all"
	NotifyResult  = "result"
	NotifyError   = "error"
	NotifyOff     = "off"
)
