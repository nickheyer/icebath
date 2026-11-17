package install

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Stage names of the install ladder in run order
func StageNames() []string {
	return []string{"prerequisites", "repository", "config", "products", "runtimes", "setup", "gateway", "complete"}
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
