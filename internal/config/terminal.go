package config

import "gopkg.in/yaml.v3"

// Image every container terminal backend uses unless the user pins one
const DefaultSandboxImage = "ghcr.io/nickheyer/icebath-sandbox:desktop"

// Vercel Sandbox managed image
const DefaultVercelImage = "vercel/sandbox/universal:latest"

// Seeded Vercel runtime of older configs, never a user choice, which the config migration clears
const LegacyVercelRuntime = "node24"

// Plain sandbox images that preceded the desktop stack, which the config migration unsets when a saved config still holds one
func LegacySandboxImages() []string {
	return []string{"nikolaik/python-nodejs:python3.11-nodejs20", "nikolaik/python-nodejs:python3.14-nodejs22"}
}

// Terminal section
type TerminalConfig struct {
	Backend                      TerminalBackend `yaml:"backend"`
	ModalMode                    ModalMode       `yaml:"modal_mode"`
	DegradedMode                 DegradedMode    `yaml:"degraded_mode"`
	Cwd                          string          `yaml:"cwd"`
	TempDir                      string          `yaml:"temp_dir"`
	FontFamily                   string          `yaml:"font_family"`
	Timeout                      int             `yaml:"timeout" coerce:"nofloat"`
	DaemonTermGraceSeconds       float64         `yaml:"daemon_term_grace_seconds"`
	OneshotCompletionWaitSeconds float64         `yaml:"oneshot_completion_wait_seconds"`
	ProcessWaitAutoNotify        bool            `yaml:"process_wait_auto_notify" coerce:"strict"`
	EnvPassthrough               []string        `yaml:"env_passthrough" coerce:"exact"`
	ClientEnvPassthrough         []string        `yaml:"client_env_passthrough" coerce:"exact"`
	SyncBackMaxBytes             int64           `yaml:"sync_back_max_bytes"`
	HomeMode                     HomeMode        `yaml:"home_mode"`
	ShellInitFiles               []string        `yaml:"shell_init_files"`
	AutoSourceBashrc             bool            `yaml:"auto_source_bashrc" coerce:"bool"`
	DockerImage                  string          `yaml:"docker_image"`
	DockerForwardEnv             []string        `yaml:"docker_forward_env"`
	DockerEnv                    Map[string]     `yaml:"docker_env"`
	ModalImage                   string          `yaml:"modal_image"`
	DaytonaImage                 string          `yaml:"daytona_image"`
	VercelImage                  string          `yaml:"vercel_image"`
	VercelRuntime                string          `yaml:"vercel_runtime"`
	ContainerCPU                 float64         `yaml:"container_cpu" coerce:"strict"`
	ContainerMemory              int             `yaml:"container_memory" coerce:"nofloat"`
	ContainerDisk                int             `yaml:"container_disk" coerce:"nofloat"`
	ContainerPersistent          bool            `yaml:"container_persistent" coerce:"strset"`
	DockerVolumes                []string        `yaml:"docker_volumes"`
	DockerMountCwdToWorkspace    bool            `yaml:"docker_mount_cwd_to_workspace" coerce:"strset"`
	DockerNetwork                bool            `yaml:"docker_network" coerce:"strset"`
	DockerExtraArgs              []string        `yaml:"docker_extra_args"`
	DockerShmSize                string          `yaml:"docker_shm_size"`
	DockerRunAsHostUser          bool            `yaml:"docker_run_as_host_user" coerce:"strset"`
	DockerSnapCompat             bool            `yaml:"docker_snap_compat" coerce:"strset"`
	DockerSharedContainerKey     string          `yaml:"docker_shared_container_key"`
	PersistentShell              bool            `yaml:"persistent_shell" coerce:"strset"`
	LifetimeSeconds              *int            `yaml:"lifetime_seconds,omitempty" coerce:"nofloat"`
	SSHHost                      string          `yaml:"ssh_host,omitempty"`
	SSHUser                      string          `yaml:"ssh_user,omitempty"`
	SSHPort                      int             `yaml:"ssh_port,omitempty" coerce:"nofloat"`
	SSHKey                       string          `yaml:"ssh_key,omitempty"`
	DockerPersistAcrossProcesses *bool           `yaml:"docker_persist_across_processes,omitempty" coerce:"strset"`
	DockerOrphanReaper           *bool           `yaml:"docker_orphan_reaper,omitempty" coerce:"strset"`
	SandboxDir                   string          `yaml:"sandbox_dir,omitempty"`
	SudoPassword                 *string         `yaml:"sudo_password,omitempty"`
	EnvType                      string          `yaml:"env_type,omitempty"`
	rest                         rest
}

type terminalFields TerminalConfig

// Decodes the mapping and keeps unknown keys such as plugin backend settings
func (t *TerminalConfig) UnmarshalYAML(n *yaml.Node) error {
	return decodeMapping(n, (*terminalFields)(t), &t.rest)
}

// Writes the mapping with unknown keys after the named ones
func (t TerminalConfig) MarshalYAML() (any, error) {
	return encodeMapping(terminalFields(t), t.rest)
}

// Plugin backend setting kept among the unknown keys, such as singularity_image
func (t TerminalConfig) Extra(key string) *yaml.Node {
	return t.rest.copyOf(key)
}

// Execution environment of the terminal tool, a built-in name or a plugin backend name
type TerminalBackend string

// Built-in terminal backends
const (
	BackendLocal         TerminalBackend = "local"
	BackendDocker        TerminalBackend = "docker"
	BackendSSH           TerminalBackend = "ssh"
	BackendModal         TerminalBackend = "modal"
	BackendManagedModal  TerminalBackend = "managed_modal"
	BackendDaytona       TerminalBackend = "daytona"
	BackendVercelSandbox TerminalBackend = "vercel_sandbox"
)

// Built-in terminal backends, plugin backends add their own names
func TerminalBackends() []TerminalBackend {
	return []TerminalBackend{BackendLocal, BackendDocker, BackendSSH, BackendModal, BackendManagedModal, BackendDaytona, BackendVercelSandbox}
}

// Whether Modal runs on direct credentials or the managed gateway
type ModalMode string

// Modal modes
const (
	ModalAuto    ModalMode = "auto"
	ModalDirect  ModalMode = "direct"
	ModalManaged ModalMode = "managed"
)

// Accepted Modal modes, any other value reads as auto
func ModalModes() []ModalMode {
	return []ModalMode{ModalAuto, ModalDirect, ModalManaged}
}

// Reaction to remote backend connection failures
type DegradedMode string

// Degraded modes
const (
	DegradedWarn DegradedMode = "warn"
	DegradedFail DegradedMode = "fail"
)

// Accepted degraded modes, any other value reads as warn
func DegradedModes() []DegradedMode {
	return []DegradedMode{DegradedWarn, DegradedFail}
}

// HOME policy for host tool subprocesses
type HomeMode string

// Home modes
const (
	HomeAuto    HomeMode = "auto"
	HomeReal    HomeMode = "real"
	HomeProfile HomeMode = "profile"
)

// Accepted home modes
func HomeModes() []HomeMode {
	return []HomeMode{HomeAuto, HomeReal, HomeProfile}
}

// Defaults of the terminal section
func defaultTerminal() TerminalConfig {
	return TerminalConfig{
		Backend:                      BackendLocal,
		ModalMode:                    ModalAuto,
		DegradedMode:                 DegradedWarn,
		Cwd:                          ".",
		Timeout:                      180,
		DaemonTermGraceSeconds:       2.0,
		OneshotCompletionWaitSeconds: 600.0,
		ProcessWaitAutoNotify:        true,
		EnvPassthrough:               []string{},
		ClientEnvPassthrough:         []string{},
		SyncBackMaxBytes:             2 * 1024 * 1024 * 1024,
		HomeMode:                     HomeAuto,
		ShellInitFiles:               []string{},
		AutoSourceBashrc:             true,
		DockerImage:                  DefaultSandboxImage,
		DockerForwardEnv:             []string{},
		ModalImage:                   DefaultSandboxImage,
		DaytonaImage:                 DefaultSandboxImage,
		VercelImage:                  DefaultVercelImage,
		ContainerCPU:                 1,
		ContainerMemory:              5120,
		ContainerDisk:                51200,
		ContainerPersistent:          true,
		DockerVolumes:                []string{},
		DockerNetwork:                true,
		DockerExtraArgs:              []string{},
		DockerShmSize:                "1g",
		PersistentShell:              true,
	}
}

// Documentation of the terminal section
func terminalDocs() map[string]string {
	return map[string]string{
		"terminal.backend":                         "Execution environment: local, docker, ssh, modal, daytona, vercel_sandbox, or a plugin backend such as singularity.",
		"terminal.modal_mode":                      "Modal routing: auto, direct (own credentials) or managed (tool gateway).",
		"terminal.degraded_mode":                   "Remote-backend connection-class failures (SSH host unreachable, Docker daemon down): \"warn\" = structured degraded tool result with reason + retry hint; \"fail\" = raise error + traceback.",
		"terminal.cwd":                             "Working directory. For the local backend \".\" = current directory; for remote backends the path on the remote host or inside the container.",
		"terminal.temp_dir":                        "Root for terminal session temp files (background logs, pid and exit files, code-exec sandboxes). Empty = TMPDIR/TMP/TEMP if set, else ICEBATH_HOME/cache/terminal (auto-pruned after 72h), NOT tmpfs /tmp, which is RAM-capped and fills under load. Must be an existing absolute POSIX path; user-set paths are never auto-pruned.",
		"terminal.font_family":                     "CSS font-family for the desktop app's terminal (e.g. \"'CaskaydiaCoveNerdFont', monospace\"). Empty = built-in default (\"'JetBrains Mono', 'Cascadia Code', 'SF Mono', Menlo, Consolas, monospace\"). Lets users use a Nerd Font without patching the app.",
		"terminal.timeout":                         "Per-command timeout in seconds.",
		"terminal.daemon_term_grace_seconds":       "Seconds between SIGTERM and escalated SIGKILL for host process trees (browser daemons). 0 = SIGTERM only.",
		"terminal.oneshot_completion_wait_seconds": "Max seconds a one-shot CLI run (-q/-Q/-z) lingers for tracked notify_on_complete background processes to finish. The dying parent owns their stdout pipes, so exiting immediately kills the delivery (e.g. Bot Mode handoff replies via message_agent or bot_relay). Plain background processes without notify_on_complete are never waited on. 0 disables.",
		"terminal.process_wait_auto_notify":        "Two unchanged wait/poll calls on a running background process turn on notify_on_complete for it and tell the agent to stop checking; false = poll as before.",
		"terminal.env_passthrough":                 "Env vars passed into sandboxed terminal and execute_code (plus skill-declared required_environment_variables).",
		"terminal.client_env_passthrough":          "Extra non-secret toolchain names a CLI or TUI client sends for its tools, e.g. [\"GEM_HOME\", \"POETRY_HOME\"]. Credential-shaped names and ICEBATH_* names are never sent; pass secrets with env_passthrough instead.",
		"terminal.sync_back_max_bytes":             "Remote-backend sync-back refuses to extract a downloaded state archive larger than this (bytes); raise it for a ~/.icebath tree that legitimately exceeds 2 GiB.",
		"terminal.home_mode":                       "HOME for host tool subprocesses: \"auto\" = host keeps the real OS-user HOME, containers use ICEBATH_HOME/home; \"real\" = force real HOME; \"profile\" = force ICEBATH_HOME/home when it exists (strict per-profile isolation).",
		"terminal.shell_init_files":                "Extra files sourced in the login shell when building the per-session env snapshot, for nvm/pyenv/asdf/PATH entries registered by files a bash login shell skips (~/.bashrc, ~/.zshrc, ~/.zprofile). Supports ~ and ${VAR}; missing files skipped. When empty and the shell is bash, ~/.profile, ~/.bash_profile, ~/.bashrc are auto-sourced in that order (see auto_source_bashrc).",
		"terminal.auto_source_bashrc":              "Source ~/.profile, ~/.bash_profile, ~/.bashrc in the snapshot login shell to capture PATH additions, functions, and aliases that `bash -l -c` misses (bash skips bashrc when non-interactive; Debian/Ubuntu ~/.bashrc short-circuits). ~/.profile and ~/.bash_profile go first because n/nvm/asdf write PATH exports there without an interactivity guard. Turn off if an rc file misbehaves when sourced non-interactively (exits on TTY check).",
		"terminal.docker_image":                    "The default sandbox for every container backend: the nikolaik/python-nodejs base (3.13 / Node 26) plus a display stack, so Bot Screen, computer_use and the bot's browser run INSIDE the sandbox and the pane can watch them (see bot_desktop).",
		"terminal.docker_forward_env":              "Host env vars forwarded into Docker, read from the current shell first, then ~/.icebath/.env. Anything forwarded is visible to commands run in the container.",
		"terminal.docker_env":                      "Exact key-value env pairs set inside Docker containers (unlike docker_forward_env, which reads host values), useful under systemd without the user's shell env. Example: {\"SSH_AUTH_SOCK\": \"/run/user/1000/ssh-agent.sock\"}.",
		"terminal.modal_image":                     "Sandbox image for the modal backend.",
		"terminal.daytona_image":                   "Sandbox image for the daytona backend.",
		"terminal.vercel_image":                    "vercel_sandbox backend only: a Vercel-managed or VCR image.",
		"terminal.vercel_runtime":                  "Deprecated by Vercel; a legacy runtime pin (node24 | node22 | python3.13) overrides vercel_image.",
		"terminal.container_cpu":                   "CPU cores for container backends (docker, modal, daytona, vercel_sandbox, container plugin backends; not local or ssh).",
		"terminal.container_memory":                "Container memory in MB (default 5GB).",
		"terminal.container_disk":                  "Container disk in MB (default 50GB).",
		"terminal.container_persistent":            "Persist the container filesystem across sessions (false = ephemeral).",
		"terminal.docker_volumes":                  "Docker volume mounts, \"host_path:container_path\" (docker -v syntax), e.g. [\"/home/user/.icebath/cache/documents:/output\"]. For gateway MEDIA delivery, write to /output/... inside Docker and emit the host-visible path in MEDIA:, not the container one.",
		"terminal.docker_mount_cwd_to_workspace":   "Mount the host cwd at /workspace (weakens isolation).",
		"terminal.docker_network":                  "false = --network=none, no network access from commands.",
		"terminal.docker_extra_args":               "Extra flags passed verbatim to docker run.",
		"terminal.docker_shm_size":                 "/dev/shm size for the Docker sandbox. Docker's 64 MB default silently breaks Chromium/Playwright and PyTorch DataLoader workers; tmpfs is lazily allocated so the higher ceiling is free until used. \"\" or \"0\" = omit the flag (Docker default).",
		"terminal.docker_run_as_host_user":         "Run the container as the host uid:gid (`--user`) so files written to bind mounts (docker_volumes, persistent workspace, mounted cwd) are owned by you, not root. Off by default for images whose entrypoints must start as root (e.g. the bundled icebath image, which drops to `icebath` via s6-setuidgid). When on, SETUID/SETGID caps are omitted.",
		"terminal.docker_snap_compat":              "Snap-packaged Docker under AppArmor (Ubuntu cloud images; LP#1908448) refuses to exec anything under `--init` or `--security-opt no-new-privileges` (\"operation not permitted\"). true drops those two flags; every other hardening stays.",
		"terminal.docker_shared_container_key":     "Trusted profiles sharing one Docker container identity; empty = per-profile boundary.",
		"terminal.persistent_shell":                "SSH: carry exported env vars between commands (cwd always carries). false = every command starts from the login env, profile functions included. Local: TERMINAL_LOCAL_PERSISTENT.",
		"terminal.lifetime_seconds":                "Idle seconds before a sandbox environment is reclaimed.",
		"terminal.ssh_host":                        "SSH backend host.",
		"terminal.ssh_user":                        "SSH backend user.",
		"terminal.ssh_port":                        "SSH backend port (default 22).",
		"terminal.ssh_key":                         "SSH private key path; uses ssh-agent when unset.",
		"terminal.sudo_password":                   "Password piped to sudo -S for every backend (plaintext). An explicit empty string tries an empty password and never opens the interactive prompt; unset prompts interactively.",
	}
}
