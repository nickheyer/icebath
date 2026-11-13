package updater

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

var fallbackCommitRE = regexp.MustCompile(`^0{7,40}$`)

// InstallStamp is the build stamp of this desktop artifact, install-stamp.json in its resources
type InstallStamp struct {
	Commit          *string                 `json:"commit"`
	Branch          *string                 `json:"branch"`
	BuiltAt         *string                 `json:"builtAt"`
	Source          *string                 `json:"source"`
	CommitDate      *float64                `json:"commitDate"`
	Tag             *string                 `json:"tag"`
	Payload         string                  `json:"payload"`
	UpdateMechanism string                  `json:"updateMechanism"`
	Runtime         *backend.PayloadRuntime `json:"runtime"`
	ChannelBuild    *ChannelBuild           `json:"channelBuild"`
}

// Stamp a resources directory carries, nil for a dev run or an unreadable stamp
func readInstallStamp(resourcesPath string) *InstallStamp {
	if resourcesPath == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(resourcesPath, "install-stamp.json"))
	if err != nil {
		return nil
	}
	var stamp InstallStamp
	if json.Unmarshal(raw, &stamp) != nil {
		return nil
	}
	return &stamp
}

// Bundled when the runtime ships inside the artifact, checkout for dev runs and bootstrap artifacts
func (s *Service) installShape() string {
	if s.stamp != nil && s.stamp.Payload == "bundled" {
		return "bundled"
	}
	return "checkout"
}

func isFallbackCommit(commit string) bool {
	return fallbackCommitRE.MatchString(commit)
}

func stringOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Whether the bundle on disk is a different build than the one running
func detectBundleSwap(running, onDisk *InstallStamp) bool {
	if running == nil || onDisk == nil || stringOrEmpty(running.Commit) == "" || stringOrEmpty(onDisk.Commit) == "" {
		return false
	}
	if stringOrEmpty(running.Source) == "fallback" || isFallbackCommit(*running.Commit) {
		return false
	}
	if stringOrEmpty(onDisk.Source) == "fallback" || isFallbackCommit(*onDisk.Commit) {
		return false
	}
	if *running.Commit != *onDisk.Commit {
		return true
	}
	if (running.BuiltAt == nil) != (onDisk.BuiltAt == nil) {
		return true
	}
	return running.BuiltAt != nil && *running.BuiltAt != *onDisk.BuiltAt
}

const (
	bundleSwapRelaunchFlag     = "--hermes-bundle-swap-relaunched"
	bundleSwapRelaunchFailsafe = 15 * time.Second
)

// Relaunches into a bundle the update swapped under this process, once per launch chain
func (s *Service) relaunchIntoSwappedBundle() bool {
	if !s.opts.IsPackaged {
		return false
	}
	for _, arg := range s.opts.Args {
		if arg == bundleSwapRelaunchFlag {
			return false
		}
	}
	if !detectBundleSwap(s.stamp, readInstallStamp(s.opts.ResourcesPath)) {
		return false
	}
	s.log("[updates] app bundle was swapped during the update; relaunching into the new build")
	var args []string
	if len(s.opts.Args) > 1 {
		args = append(args, s.opts.Args[1:]...)
	}
	args = append(args, bundleSwapRelaunchFlag)
	if err := s.host.Relaunch(args); err != nil {
		s.log("[updates] bundle-swap relaunch failed: " + err.Error() + "; continuing with the current build")
		return false
	}
	go s.host.ExitAfterBackendShutdown(0)
	return true
}
