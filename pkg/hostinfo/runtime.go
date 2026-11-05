package hostinfo

import (
	"os"
	"strings"
	"sync"
)

// IsTermux reports whether this process runs inside Termux
func IsTermux() bool {
	return os.Getenv("TERMUX_VERSION") != "" || strings.Contains(os.Getenv("PREFIX"), "com.termux/files/usr")
}

var (
	wslOnce       sync.Once
	wslDetected   bool
	containerOnce sync.Once
	containerSeen bool
)

// IsWSL reports whether this process runs inside WSL
func IsWSL() bool {
	wslOnce.Do(func() {
		data, err := os.ReadFile("/proc/version")
		wslDetected = err == nil && strings.Contains(strings.ToLower(string(data)), "microsoft")
	})
	return wslDetected
}

// IsContainer reports whether this process runs inside a container
func IsContainer() bool {
	containerOnce.Do(func() { containerSeen = detectContainer() })
	return containerSeen
}

// Whole file text, empty on error
func readProc(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// Reports whether the file holds any marker
func procFileHasMarker(path string, markers []string) bool {
	content := readProc(path)
	for _, m := range markers {
		if strings.Contains(content, m) {
			return true
		}
	}
	return false
}

// Container markers from the docker and podman env files, kubernetes env, and the init cgroup, then the root mount
func detectContainer() bool {
	if fileExistsAny("/.dockerenv", "/run/.containerenv") || os.Getenv("KUBERNETES_SERVICE_HOST") != "" ||
		procFileHasMarker("/proc/1/cgroup", []string{"docker", "podman", "/lxc/", "kubepods", "containerd", "crio"}) {
		return true
	}
	return rootMountHasMarker("/proc/self/mountinfo", []string{"kubepods", "containerd", "crio"})
}

// Reports whether any of the paths exists
func fileExistsAny(paths ...string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Reports whether the root mount line of mountinfo contains any marker
func rootMountHasMarker(path string, markers []string) bool {
	for _, line := range strings.Split(readProc(path), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[4] != "/" {
			continue
		}
		for _, m := range markers {
			if strings.Contains(line, m) {
				return true
			}
		}
	}
	return false
}
