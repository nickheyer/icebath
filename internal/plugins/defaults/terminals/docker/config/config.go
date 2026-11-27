package config

import terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"

const (
	Key = terminalconfig.Key + ".docker"
	// Image the sandbox starts from
	Image = Key + ".image"
	// The image counts as chosen even at its default, so sandboxes on another image are replaced rather than kept
	ImagePinned = Key + ".image_pinned"
	// host:container mounts
	Volumes = Key + ".volumes"
	// The session's folder is mounted at /workspace
	MountCwd = Key + ".mount_cwd_to_workspace"
	// Names of host variables passed in
	ForwardEnv = Key + ".forward_env"
	// Variables set inside, by name
	Env = Key + ".env"
	// The container runs as the host user's uid and gid
	RunAsHostUser = Key + ".run_as_host_user"
	// Flags added to docker run
	ExtraArgs = Key + ".extra_args"
	// Size of /dev/shm
	ShmSize = Key + ".shm_size"
	// The sandbox reaches the network
	Network = Key + ".network"
	// A container is found again and reused by later runs
	PersistAcrossProcesses = Key + ".persist_across_processes"
	// Name every profile's sessions share one container by
	SharedContainerKey = Key + ".shared_container_key"
	// Stopped containers of earlier runs are removed
	OrphanReaper = Key + ".orphan_reaper"
	// Runs without --init and no-new-privileges for Docker installed as a snap
	SnapCompat = Key + ".snap_compat"
)
