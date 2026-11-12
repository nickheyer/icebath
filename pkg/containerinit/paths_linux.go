package containerinit

import (
	"os"
	"path/filepath"
	"strings"
)

// Image layout the init, the verbs and the bootstrap share, vars so tests can point them at a temp tree
var (
	installDir     = "/opt/icebath"
	defaultHome    = "/opt/data"
	scanDir        = "/run/service"
	baseDir        = "/run/s6/basedir"
	containerEnv   = "/run/s6/container_environment"
	commandDir     = "/command"
	provenanceFile = "/etc/icebath/image-provenance.json"
	chromiumPin    = "/etc/icebath/chromium-executable-path"
	flyAPISocket   = "/.fly/api"
	dockerSockets  = []string{"/var/run/docker.sock", "/run/docker.sock"}
	accountName    = "icebath"
	initTarget     = "/init"
)

// Pid file of the running supervisor, read by the service manager when the supervisor is not PID 1
func svscanPIDFile() string { return filepath.Join(baseDir, "svscan.pid") }

// Control fifo of the scan directory the svscanctl verb writes to
func svscanControl(dir string) string { return filepath.Join(dir, ".s6-svscan", "control") }

// Data volume the container keeps its home in
func DataHome() string {
	if h := os.Getenv("ICEBATH_HOME"); h != "" {
		return h
	}
	return defaultHome
}

// File the image pins the Chromium executable path in
func ChromiumPinPath() string { return chromiumPin }

// Product binary for the running executable, the icebath beside the separate init or the executable itself
func productBinaryFor(exe string) string {
	if filepath.Base(exe) == InitName {
		return filepath.Join(filepath.Dir(exe), productName)
	}
	return exe
}

// True for the yes spellings the image scripts accept
func truthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes":
		return true
	}
	return false
}

// True for the lowercase yes spellings Python env checks accept
func envFlag(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}
