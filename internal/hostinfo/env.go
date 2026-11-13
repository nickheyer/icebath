package hostinfo

import (
	"os"
	"slices"
	"strings"
)

// Reports a process inside the Android terminal emulator app, read from the environment on every call
func Termux() bool {
	return os.Getenv("TERMUX_VERSION") != "" || strings.Contains(os.Getenv("PREFIX"), "com.termux/files/usr")
}

// Reports a process inside the Windows Subsystem for Linux, read once from the kernel version text
func (h *Host) WSL() bool {
	return h.wsl.value(func() bool {
		data, err := os.ReadFile("/proc/version")
		return err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
	})
}

// Evidence that this process runs inside a container, one field per probe
type Container struct {
	DockerEnv    bool
	ContainerEnv bool
	Kubernetes   bool
	Variable     string
	InitCgroup   []string
	RootMount    []string
}

var (
	initCgroupMarkers = []string{"docker", "podman", "/lxc/", "kubepods", "containerd", "crio", "libpod"}
	rootMountMarkers  = []string{"kubepods", "containerd", "crio"}
)

// Reports a container by marker file, Kubernetes, an init cgroup runtime or a root mount runtime
func (c Container) Inside() bool {
	if c.DockerEnv || c.ContainerEnv || c.Kubernetes || len(c.RootMount) > 0 {
		return true
	}
	return slices.ContainsFunc(c.InitCgroup, func(marker string) bool { return marker != "libpod" })
}

// Reports a container of the docker engine by its marker file or its init cgroup
func (c Container) Docker() bool { return c.DockerEnv || slices.Contains(c.InitCgroup, "docker") }

// Evidence of a container, read once
func (h *Host) Container() Container { return h.container.value(DetectContainer) }

// Container evidence read again from marker files, environment, init cgroup and root mount on every call
func DetectContainer() Container {
	c := Container{
		DockerEnv:    exists("/.dockerenv"),
		ContainerEnv: exists("/run/.containerenv"),
		Kubernetes:   os.Getenv("KUBERNETES_SERVICE_HOST") != "",
		Variable:     os.Getenv("container"),
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		c.InitCgroup = markersIn(string(data), initCgroupMarkers)
	}
	if data, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Fields(line); len(fields) >= 5 && fields[4] == "/" {
				c.RootMount = append(c.RootMount, markersIn(line, rootMountMarkers)...)
			}
		}
	}
	return c
}

// Markers that occur in text, in marker order
func markersIn(text string, markers []string) []string {
	var found []string
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			found = append(found, marker)
		}
	}
	return found
}

// Reports whether something is at path
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// How the user reaches this process, as its environment variables tell
type Session struct {
	SSHClient     bool
	SSHConnection bool
	SSHTTY        bool
	RemoteConsole string
	CI            bool
}

// Reports any of the variables sshd sets for a login
func (s Session) SSH() bool { return s.SSHClient || s.SSHConnection || s.SSHTTY }

var (
	remoteConsoleVariables = []string{"CLOUD_SHELL", "CODESPACES", "CODESPACE_NAME", "GITPOD_WORKSPACE_ID", "REPL_ID", "STACKBLITZ"}
	ciVariables            = []string{"CI", "GITHUB_ACTIONS"}
	offWords               = map[string]bool{"": true, "0": true, "false": true, "no": true, "off": true}
)

// Session facts read through lookup, which returns the value of an environment variable or empty
func ReadSession(lookup func(string) string) Session {
	s := Session{SSHClient: lookup("SSH_CLIENT") != "", SSHConnection: lookup("SSH_CONNECTION") != "", SSHTTY: lookup("SSH_TTY") != ""}
	for _, name := range remoteConsoleVariables {
		if lookup(name) != "" {
			s.RemoteConsole = name
			break
		}
	}
	for _, name := range ciVariables {
		if !offWords[strings.ToLower(strings.TrimSpace(lookup(name)))] {
			s.CI = true
		}
	}
	return s
}
