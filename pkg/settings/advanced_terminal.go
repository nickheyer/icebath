package settings

import "time"

// Delete old file snapshots automatically at startup
var CheckpointsAutoPrune = Register(&Bool{Meta: Meta{Key: "checkpoints.auto_prune", Label: "Auto-prune snapshots", Short: "Delete old file snapshots automatically at startup.", Group: GroupTerminal, Order: 1000, Advanced: true}, Default: true})

// Save file snapshots so edits can be rewound
var CheckpointsEnabled = Register(&Bool{Meta: Meta{Key: "checkpoints.enabled", Label: "File checkpoints", Short: "Save file snapshots so edits can be rewound.", Group: GroupTerminal, Order: 1010, Advanced: true, Apply: NewSession}, Default: false})

// Largest single file a snapshot will include
var CheckpointsMaxFileSizeMb = Register(&Int{Meta: Meta{Key: "checkpoints.max_file_size_mb", Label: "Max file size", Short: "Largest single file a snapshot will include.", Group: GroupTerminal, Order: 1020, Advanced: true, Apply: NewSession}, Default: 10, Unit: "MB"})

// How many snapshots are kept per project
var CheckpointsMaxSnapshots = Register(&Int{Meta: Meta{Key: "checkpoints.max_snapshots", Label: "Snapshots kept", Short: "How many snapshots are kept per project.", Group: GroupTerminal, Order: 1030, Advanced: true, Apply: NewSession}, Default: 20})

// Total disk space snapshots may use before pruning
var CheckpointsMaxTotalSizeMb = Register(&Int{Meta: Meta{Key: "checkpoints.max_total_size_mb", Label: "Snapshot disk limit", Short: "Total disk space snapshots may use before pruning.", Group: GroupTerminal, Order: 1040, Advanced: true, Apply: NewSession}, Default: 500, Unit: "MB"})

// Fewest hours between automatic snapshot cleanups
var CheckpointsMinIntervalHours = Register(&Duration{Meta: Meta{Key: "checkpoints.min_interval_hours", Label: "Prune interval", Short: "Fewest hours between automatic snapshot cleanups.", Group: GroupTerminal, Order: 1050, Advanced: true}, Default: 24 * time.Hour, Unit: time.Hour})

// Days an old snapshot is kept before cleanup removes it
var CheckpointsRetentionDays = Register(&Duration{Meta: Meta{Key: "checkpoints.retention_days", Label: "Snapshot retention", Short: "Days an old snapshot is kept before cleanup removes it.", Group: GroupTerminal, Order: 1060, Advanced: true}, Default: 7 * 24 * time.Hour, Unit: 24 * time.Hour})

// Use keys from the environment if the secret store is off
var ProxyAllowEnvFallback = Register(&Bool{Meta: Meta{Key: "proxy.allow_env_fallback", Label: "Allow key fallback", Short: "Use keys from the environment if the secret store is off.", Help: "Only matters when the credential source is Bitwarden.", Group: GroupTerminal, Order: 1070, Advanced: true}, Default: false})

// Download the egress proxy program when it is missing
var ProxyAutoInstall = Register(&Bool{Meta: Meta{Key: "proxy.auto_install", Label: "Auto-install proxy", Short: "Download the egress proxy program when it is missing.", Group: GroupTerminal, Order: 1080, Advanced: true}, Default: true})

// Where the egress proxy gets the real API keys it injects
var ProxyCredentialSource = Register(&Enum{Meta: Meta{Key: "proxy.credential_source", Label: "Proxy key source", Short: "Where the egress proxy gets the real API keys it injects.", Group: GroupTerminal, Order: 1090, Advanced: true}, Default: "env", Values: []Choice{
	{Value: "env", Label: "Environment", Meaning: "Read keys from this machine's environment."},
	{Value: "bitwarden", Label: "Bitwarden", Meaning: "Fetch keys from Bitwarden Secrets Manager."},
}})

// Route sandbox network traffic through the key-hiding proxy
var ProxyEnabled = Register(&Bool{Meta: Meta{Key: "proxy.enabled", Label: "Egress proxy", Short: "Route sandbox network traffic through the key-hiding proxy.", Help: "Run the egress setup command first. Applies to Docker sandboxes.", Group: GroupTerminal, Order: 1100, Advanced: true, Apply: NewSession}, Default: false})

// Refuse to start a Docker sandbox if the proxy is not ready
var ProxyEnforceOnDocker = Register(&Bool{Meta: Meta{Key: "proxy.enforce_on_docker", Label: "Require proxy", Short: "Refuse to start a Docker sandbox if the proxy is not ready.", Help: "When off, the sandbox starts without the proxy and warns.", Group: GroupTerminal, Order: 1110, Advanced: true, Apply: NewSession}, Default: true})

// Extra sites the egress proxy lets sandboxes reach
var ProxyExtraAllowedHosts = Register(&List{Meta: Meta{Key: "proxy.extra_allowed_hosts", Label: "Extra allowed hosts", Short: "Extra sites the egress proxy lets sandboxes reach.", Help: "Written into the proxy when egress setup runs.", Group: GroupTerminal, Order: 1120, Advanced: true}})

// Local port the egress proxy listens on
var ProxyTunnelPort = Register(&Int{Meta: Meta{Key: "proxy.tunnel_port", Label: "Proxy port", Short: "Local port the egress proxy listens on.", Help: "Written into the proxy when egress setup runs.", Group: GroupTerminal, Order: 1130, Advanced: true}, Default: 9090})

// Network ranges the egress proxy refuses to connect to
var ProxyUpstreamDenyCidrs = Register(&List{Meta: Meta{Key: "proxy.upstream_deny_cidrs", Label: "Blocked networks", Short: "Network ranges the egress proxy refuses to connect to.", Help: "Leave unset for the built-in private network list. An empty list blocks nothing.", Group: GroupTerminal, Order: 1140, Advanced: true}, Null: "built-in private ranges"})

// Load your usual shell startup files before running commands
var TerminalAutoSourceBashrc = Register(&Bool{Meta: Meta{Key: "terminal.auto_source_bashrc", Label: "Load shell profile", Short: "Load your usual shell startup files before running commands.", Help: "Ignored when a shell init file list is set.", Group: GroupTerminal, Order: 1150, Advanced: true}, Default: true})

// Extra variable names copied from the connecting terminal
var TerminalClientEnvPassthrough = Register(&List{Meta: Meta{Key: "terminal.client_env_passthrough", Label: "Client variables", Short: "Extra variable names copied from the connecting terminal.", Help: "Names only. Secrets are filtered out.", Group: GroupTerminal, Order: 1160, Advanced: true, Apply: NewSession}})

// CPU cores given to a container or cloud sandbox
var TerminalContainerCpu = Register(&Float{Meta: Meta{Key: "terminal.container_cpu", Label: "Sandbox CPUs", Short: "CPU cores given to a container or cloud sandbox.", Group: GroupTerminal, Order: 1170, Advanced: true, Apply: NewSession}, Default: 1, Unit: "CPUs"})

// Disk space given to a container or cloud sandbox
var TerminalContainerDisk = Register(&Int{Meta: Meta{Key: "terminal.container_disk", Label: "Sandbox disk", Short: "Disk space given to a container or cloud sandbox.", Group: GroupTerminal, Order: 1180, Advanced: true, Apply: NewSession}, Default: 51200, Unit: "MB"})

// Memory given to a container or cloud sandbox
var TerminalContainerMemory = Register(&Int{Meta: Meta{Key: "terminal.container_memory", Label: "Sandbox memory", Short: "Memory given to a container or cloud sandbox.", Group: GroupTerminal, Order: 1190, Advanced: true, Apply: NewSession}, Default: 5120, Unit: "MB"})

// Keep a sandbox's files between sessions
var TerminalContainerPersistent = Register(&Bool{Meta: Meta{Key: "terminal.container_persistent", Label: "Keep sandbox files", Short: "Keep a sandbox's files between sessions.", Group: GroupTerminal, Order: 1200, Advanced: true, Apply: NewSession}, Default: true})

// Files under the config folder copied into remote sandboxes
var TerminalCredentialFiles = Register(&List{Meta: Meta{Key: "terminal.credential_files", Label: "Credential files", Short: "Files under the config folder copied into remote sandboxes.", Help: "Paths are relative to the icebath home folder.", Group: GroupTerminal, Order: 1210, Advanced: true, Apply: NewSession}})

// Wait before force-killing a background process that was asked to stop
var TerminalDaemonTermGraceSeconds = Register(&Duration{Meta: Meta{Key: "terminal.daemon_term_grace_seconds", Label: "Stop grace period", Short: "Wait before force-killing a background process that was asked to stop.", Help: "Zero kills it immediately.", Group: GroupTerminal, Order: 1220, Advanced: true}, Default: 2 * time.Second})

// Image Daytona sandboxes start from
var TerminalDaytonaImage = Register(&String{Meta: Meta{Key: "terminal.daytona_image", Label: "Daytona image", Short: "Image Daytona sandboxes start from.", Group: GroupTerminal, Order: 1230, Advanced: true, Apply: NewSession}, Default: "localhost/icebath-sandbox:desktop"})

// What happens when the place commands run cannot be reached
var TerminalDegradedMode = Register(&Enum{Meta: Meta{Key: "terminal.degraded_mode", Label: "Backend failure mode", Short: "What happens when the place commands run cannot be reached.", Group: GroupTerminal, Order: 1240, Advanced: true, Apply: NewSession}, Default: "warn", Values: []Choice{
	{Value: "warn", Label: "Warn", Meaning: "Report the problem and keep the session going."},
	{Value: "fail", Label: "Fail", Meaning: "Stop the turn with an error."},
}})

// Variables set inside Docker sandboxes, as name and value
var TerminalDockerEnv = Register(&Map{Meta: Meta{Key: "terminal.docker_env", Label: "Docker variables", Short: "Variables set inside Docker sandboxes, as name and value.", Group: GroupTerminal, Order: 1250, Advanced: true, Apply: NewSession}})

// Extra flags added to the Docker run command
var TerminalDockerExtraArgs = Register(&List{Meta: Meta{Key: "terminal.docker_extra_args", Label: "Docker extra flags", Short: "Extra flags added to the Docker run command.", Group: GroupTerminal, Order: 1260, Advanced: true, Apply: NewSession}})

// Host variables passed into Docker sandboxes
var TerminalDockerForwardEnv = Register(&List{Meta: Meta{Key: "terminal.docker_forward_env", Label: "Docker forwarded variables", Short: "Host variables passed into Docker sandboxes.", Help: "Names only. The values come from your environment.", Group: GroupTerminal, Order: 1270, Advanced: true, Apply: NewSession}})

// Mount your current folder into the Docker sandbox
var TerminalDockerMountCwdToWorkspace = Register(&Bool{Meta: Meta{Key: "terminal.docker_mount_cwd_to_workspace", Label: "Mount working folder", Short: "Mount your current folder into the Docker sandbox.", Group: GroupTerminal, Order: 1280, Advanced: true, Apply: NewSession}, Default: false})

// Let Docker sandboxes reach the network
var TerminalDockerNetwork = Register(&Bool{Meta: Meta{Key: "terminal.docker_network", Label: "Docker network", Short: "Let Docker sandboxes reach the network.", Group: GroupTerminal, Order: 1290, Advanced: true, Apply: NewSession}, Default: true})

// Remove leftover Docker sandboxes from earlier runs
var TerminalDockerOrphanReaper = Register(&Bool{Meta: Meta{Key: "terminal.docker_orphan_reaper", Label: "Clean stray containers", Short: "Remove leftover Docker sandboxes from earlier runs.", Group: GroupTerminal, Order: 1300, Advanced: true, Apply: NewSession}, Null: "on"})

// Keep a Docker sandbox alive so later runs can reuse it
var TerminalDockerPersistAcrossProcesses = Register(&Bool{Meta: Meta{Key: "terminal.docker_persist_across_processes", Label: "Reuse Docker sandbox", Short: "Keep a Docker sandbox alive so later runs can reuse it.", Group: GroupTerminal, Order: 1310, Advanced: true, Apply: NewSession}, Null: "on"})

// Run Docker commands as your own user, not root
var TerminalDockerRunAsHostUser = Register(&Bool{Meta: Meta{Key: "terminal.docker_run_as_host_user", Label: "Run as me", Short: "Run Docker commands as your own user, not root.", Group: GroupTerminal, Order: 1320, Advanced: true, Apply: NewSession}, Default: false})

// Name that lets sessions share one Docker sandbox
var TerminalDockerSharedContainerKey = Register(&String{Meta: Meta{Key: "terminal.docker_shared_container_key", Label: "Shared sandbox name", Short: "Name that lets sessions share one Docker sandbox.", Help: "Empty means each session gets its own.", Group: GroupTerminal, Order: 1330, Advanced: true, Apply: NewSession}, Default: ""})

// Shared memory size for Docker sandboxes, such as 1g
var TerminalDockerShmSize = Register(&String{Meta: Meta{Key: "terminal.docker_shm_size", Label: "Docker shared memory", Short: "Shared memory size for Docker sandboxes, such as 1g.", Group: GroupTerminal, Order: 1340, Advanced: true, Apply: NewSession}, Default: "1g"})

// Work around Docker installed as a snap package
var TerminalDockerSnapCompat = Register(&Bool{Meta: Meta{Key: "terminal.docker_snap_compat", Label: "Snap Docker mode", Short: "Work around Docker installed as a snap package.", Group: GroupTerminal, Order: 1350, Advanced: true, Apply: NewSession}, Default: false})

// Folders mounted into Docker sandboxes, as host:container
var TerminalDockerVolumes = Register(&List{Meta: Meta{Key: "terminal.docker_volumes", Label: "Docker volumes", Short: "Folders mounted into Docker sandboxes, as host:container.", Group: GroupTerminal, Order: 1360, Advanced: true, Apply: NewSession}})

// Extra variable names commands may see from your environment
var TerminalEnvPassthrough = Register(&List{Meta: Meta{Key: "terminal.env_passthrough", Label: "Passthrough env names", Short: "Extra variable names commands may see from your environment.", Help: "Provider keys are refused here.", Group: GroupTerminal, Order: 1370, Advanced: true}})

// Which home folder commands see
var TerminalHomeMode = Register(&Enum{Meta: Meta{Key: "terminal.home_mode", Label: "Home folder mode", Short: "Which home folder commands see.", Group: GroupTerminal, Order: 1380, Advanced: true, Apply: NewSession}, Default: "auto", Values: []Choice{
	{Value: "auto", Label: "Automatic", Meaning: "Pick the safest home for where commands run."},
	{Value: "profile", Label: "Profile home", Meaning: "Use the isolated icebath profile folder."},
	{Value: "real", Label: "Real home", Meaning: "Use your own home folder."},
}})

// Image Modal sandboxes start from
var TerminalModalImage = Register(&String{Meta: Meta{Key: "terminal.modal_image", Label: "Modal image", Short: "Image Modal sandboxes start from.", Group: GroupTerminal, Order: 1390, Advanced: true, Apply: NewSession}, Default: "localhost/icebath-sandbox:desktop"})

// Longest wait for a one-shot background command to finish
var TerminalOneshotCompletionWaitSeconds = Register(&Duration{Meta: Meta{Key: "terminal.oneshot_completion_wait_seconds", Label: "One-shot wait limit", Short: "Longest wait for a one-shot background command to finish.", Group: GroupTerminal, Order: 1400, Advanced: true}, Default: 10 * time.Minute})

// Keep one shell open between remote commands over SSH
var TerminalPersistentShell = Register(&Bool{Meta: Meta{Key: "terminal.persistent_shell", Label: "Persistent SSH shell", Short: "Keep one shell open between remote commands over SSH.", Help: "Other backends have their own persistence settings.", Group: GroupTerminal, Order: 1410, Advanced: true, Apply: NewSession}, Default: true})

// Tell the agent when a waited-on background process ends
var TerminalProcessWaitAutoNotify = Register(&Bool{Meta: Meta{Key: "terminal.process_wait_auto_notify", Label: "Notify on finish", Short: "Tell the agent when a waited-on background process ends.", Group: GroupTerminal, Order: 1420, Advanced: true}, Default: true})

// Host folder where sandbox files are stored
var TerminalSandboxDir = Register(&String{Meta: Meta{Key: "terminal.sandbox_dir", Label: "Sandbox folder", Short: "Host folder where sandbox files are stored.", Help: "Unset uses the sandboxes folder in the icebath home.", Group: GroupTerminal, Order: 1430, Advanced: true, Apply: NewSession}, Null: "the sandboxes folder in the icebath home"})

// Files sourced before commands run, instead of the usual ones
var TerminalShellInitFiles = Register(&List{Meta: Meta{Key: "terminal.shell_init_files", Label: "Shell init files", Short: "Files sourced before commands run, instead of the usual ones.", Help: "Empty falls back to the profile and bashrc files.", Group: GroupTerminal, Order: 1440, Advanced: true}})

// Largest archive copied back from a remote sandbox
var TerminalSyncBackMaxBytes = Register(&Int{Meta: Meta{Key: "terminal.sync_back_max_bytes", Label: "Sync-back size cap", Short: "Largest archive copied back from a remote sandbox.", Group: GroupTerminal, Order: 1450, Advanced: true}, Default: 2147483648, Unit: "bytes"})

// Folder commands use for temporary files
var TerminalTempDir = Register(&String{Meta: Meta{Key: "terminal.temp_dir", Label: "Temp folder", Short: "Folder commands use for temporary files.", Help: "Empty uses the normal system temp folder.", Group: GroupTerminal, Order: 1460, Advanced: true, Apply: NewSession}, Default: ""})

// Longest a foreground command may run before it is stopped
var TerminalTimeout = Register(&Duration{Meta: Meta{Key: "terminal.timeout", Label: "Command timeout", Short: "Longest a foreground command may run before it is stopped.", Group: GroupTerminal, Order: 1470, Advanced: true, Apply: NewSession}, Default: 3 * time.Minute})

// Image Vercel sandboxes start from
var TerminalVercelImage = Register(&String{Meta: Meta{Key: "terminal.vercel_image", Label: "Vercel image", Short: "Image Vercel sandboxes start from.", Group: GroupTerminal, Order: 1480, Advanced: true, Apply: NewSession}, Default: "vercel/sandbox/universal:latest"})

// Language runtime for Vercel sandboxes
var TerminalVercelRuntime = Register(&String{Meta: Meta{Key: "terminal.vercel_runtime", Label: "Vercel runtime", Short: "Language runtime for Vercel sandboxes.", Help: "Empty uses the image setting instead.", Group: GroupTerminal, Order: 1490, Advanced: true, Apply: NewSession}, Default: ""})

// Save a patch of unsaved work before removing a worktree
var WorktreesArchiveDirty = Register(&Bool{Meta: Meta{Key: "worktrees.archive_dirty", Label: "Archive dirty trees", Short: "Save a patch of unsaved work before removing a worktree.", Group: GroupTerminal, Order: 1500, Advanced: true}, Default: true})

// Largest file included in a worktree archive
var WorktreesArchiveMaxFileMb = Register(&Int{Meta: Meta{Key: "worktrees.archive_max_file_mb", Label: "Archive file limit", Short: "Largest file included in a worktree archive.", Group: GroupTerminal, Order: 1510, Advanced: true}, Default: 50, Unit: "MB"})

// Days a removed worktree's archive is kept
var WorktreesArchiveRetentionDays = Register(&Duration{Meta: Meta{Key: "worktrees.archive_retention_days", Label: "Archive retention", Short: "Days a removed worktree's archive is kept.", Group: GroupTerminal, Order: 1520, Advanced: true}, Default: 30 * 24 * time.Hour, Unit: 24 * time.Hour})

// Days unused dependency install data is kept
var WorktreesInstallStateRetentionDays = Register(&Duration{Meta: Meta{Key: "worktrees.install_state_retention_days", Label: "Install cache retention", Short: "Days unused dependency install data is kept.", Help: "Zero or less turns cleanup off.", Group: GroupTerminal, Order: 1530, Advanced: true}, Default: 14 * 24 * time.Hour, Unit: 24 * time.Hour})

// Total disk space worktrees may use before cleanup
var WorktreesMaxTotalGb = Register(&Int{Meta: Meta{Key: "worktrees.max_total_gb", Label: "Worktree disk limit", Short: "Total disk space worktrees may use before cleanup.", Help: "Zero means no limit.", Group: GroupTerminal, Order: 1540, Advanced: true}, Default: 0, Unit: "GB"})

// New terminal sessions start in their own git work tree
var Worktree = Register(&Bool{Meta: Meta{Key: "worktree", Label: "Own work tree", Short: "Start each terminal session in its own git work tree.", Group: GroupTerminal, Order: 1600, Advanced: true, Apply: NewSession}, Default: false})

// New work trees branch from the latest upstream commit
var WorktreeSync = Register(&Bool{Meta: Meta{Key: "worktree_sync", Label: "Fetch before branching", Short: "Fetch the upstream branch before making a new work tree.", Help: "Off branches from your local HEAD.", Group: GroupTerminal, Order: 1610, Advanced: true, Apply: NewSession}, Default: true})

// Idle time after which an unused sandbox is cleaned up
var TerminalLifetimeSeconds = Register(&Duration{Meta: Meta{Key: "terminal.lifetime_seconds", Label: "Sandbox idle limit", Short: "Idle time after which an unused sandbox is shut down.", Group: GroupTerminal, Order: 1500, Advanced: true, Apply: NewSession}, Default: 5 * time.Minute, Min: at(time.Duration(0))})

// Machine the agent's commands run on over SSH
var TerminalSSHHost = Register(&String{Meta: Meta{Key: "terminal.ssh_host", Label: "SSH host", Short: "Machine the agent's commands run on when they run over SSH.", Group: GroupTerminal, Order: 1510, Advanced: true, Apply: NewSession}})

// Account the SSH backend logs in as
var TerminalSSHUser = Register(&String{Meta: Meta{Key: "terminal.ssh_user", Label: "SSH user", Short: "Account to log in as over SSH.", Group: GroupTerminal, Order: 1520, Advanced: true, Apply: NewSession}})

// Port the SSH backend connects to
var TerminalSSHPort = Register(&Int{Meta: Meta{Key: "terminal.ssh_port", Label: "SSH port", Short: "Port to connect to over SSH.", Group: GroupTerminal, Order: 1530, Advanced: true, Apply: NewSession}, Default: 22, Min: at(1), Max: at(65535)})

// Private key file the SSH backend logs in with
var TerminalSSHKey = Register(&Path{Meta: Meta{Key: "terminal.ssh_key", Label: "SSH key file", Short: "Private key file used to log in over SSH.", Help: "Empty uses your SSH agent and default keys.", Group: GroupTerminal, Order: 1540, Advanced: true, Apply: NewSession}})

// Image the Apptainer sandbox starts from
var TerminalSingularityImage = Register(&String{Meta: Meta{Key: "terminal.singularity_image", Label: "Apptainer image", Short: "Image the Apptainer sandbox starts from.", Group: GroupTerminal, Order: 1550, Advanced: true, Apply: NewSession}, Null: "the default sandbox image"})

// One shell kept open between commands on this machine
var TerminalLocalPersistent = Register(&Bool{Meta: Meta{Key: "terminal.local_persistent", Label: "Persistent local shell", Short: "Keep one shell open between commands run on this machine.", Help: "Off starts a fresh shell for every command.", Group: GroupTerminal, Order: 1560, Advanced: true, Apply: NewSession}, Default: true})
