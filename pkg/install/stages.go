package install

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Stage names of the install ladder in run order, desktop staying dispatchable but never listed
func StageNames() []string {
	return []string{"prerequisites", "repository", "config", "products", "runtimes", "setup", "gateway", "complete"}
}

// Manifest record of one stage, false for an unknown name
func StageRecord(name string, includeDesktop bool) (StageInfo, bool) {
	info := StageInfo{Name: name, Category: "runtime"}
	switch name {
	case "prerequisites":
		info.Title = "System prerequisites"
	case "repository":
		info.Title = "Download Icebath Agent"
	case "config":
		info.Title, info.Category = "Prepare config and skills", "configuration"
	case "products":
		info.Title = "Install command and app"
		if includeDesktop {
			info.Title = "Install command and app + desktop"
		}
	case "runtimes":
		info.Title = "Install Python dependencies"
	case "setup":
		info.Title, info.Category, info.NeedsUserInput = "Configure API keys and settings", "configuration", true
	case "gateway":
		info.Title, info.Category, info.NeedsUserInput = "Configure gateway service", "configuration", true
	case "desktop":
		info.Title = "Install desktop app"
	case "complete":
		info.Title = "Finish install"
	default:
		return StageInfo{}, false
	}
	return info, true
}

// Manifest of the install ladder at the current protocol version
func InstallManifest(includeDesktop bool) Manifest {
	version := ProtocolVersion
	m := Manifest{ProtocolVersion: &version}
	for _, name := range StageNames() {
		info, _ := StageRecord(name, includeDesktop)
		m.Stages = append(m.Stages, info)
	}
	return m
}

// Single line manifest JSON the --manifest flag prints
func ManifestJSON(includeDesktop bool) string {
	data, _ := json.Marshal(InstallManifest(includeDesktop))
	return string(data)
}

// Result frame line the --json flag prints, the reason key present only when a reason exists
func FrameJSON(ok bool, stage string, skipped bool, reason string) string {
	if reason != "" {
		data, _ := json.Marshal(struct {
			OK      bool   `json:"ok"`
			Stage   string `json:"stage"`
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		}{ok, stage, skipped, reason})
		return string(data)
	}
	data, _ := json.Marshal(struct {
		OK      bool   `json:"ok"`
		Stage   string `json:"stage"`
		Skipped bool   `json:"skipped"`
	}{ok, stage, skipped})
	return string(data)
}

// Installer token a receipt names for this platform's script
func receiptInstaller() string {
	if runtime.GOOS == "windows" {
		return "install_ps1"
	}
	return "install_sh"
}

// Fresh 32 hex digit run id of one full ladder
func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x%08x%08x%08x", time.Now().UnixNano()&0xffffffff, os.Getpid(), time.Now().Unix(), time.Now().Nanosecond())
	}
	return hex.EncodeToString(b[:])
}

// Parks one local install receipt per ladder run under the home metrics dir, the first writer winning
func writeInstallReceipt(homeDir, id string, started int64, outcome, failedStage, failureClass string) {
	if id == "" || started <= 0 {
		return
	}
	dir := filepath.Join(homeDir, "telemetry", "shared_metrics", "pending_installs")
	final := filepath.Join(dir, id+".json")
	if _, err := os.Lstat(final); err == nil {
		return
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return
	}
	body := fmt.Sprintf(`{"id":"%s","installer":"%s","outcome":"%s","failed_stage":"%s","failure_class":"%s","started_at":%d,"finished_at":%d}`+"\n",
		id, receiptInstaller(), outcome, strings.ReplaceAll(failedStage, "-", "_"), failureClass, started, time.Now().Unix())
	tmp := filepath.Join(dir, "."+id+".tmp")
	if err := os.WriteFile(tmp, []byte(body), 0o666); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
	}
}
