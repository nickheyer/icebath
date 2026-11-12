package dashboard

import (
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const whatsappBridgeUnknown = "Cannot verify the WhatsApp bridge is stopped. Check its bridge.pid before re-pairing."

// Bridge liveness probes and the session move of the backup, the attributes hermes tests patch
var (
	whatsappRename           = os.Rename
	whatsappPidExists        = gateway.PidExists
	whatsappProcessStartTime = gateway.GetProcessStartTime
)

// Positive integer of one bridge.pid line, nil for a missing, malformed or non positive line
func bridgePidLine(lines []string, index int) *big.Int {
	if index >= len(lines) {
		return nil
	}
	n, ok := pyint.Parse(lines[index])
	if !ok || n.Sign() <= 0 {
		return nil
	}
	return n
}

// 409 while the bridge recorded in bridge.pid may still own the session, nil when it is gone or recycled
func assertWhatsappBridgeStopped(sessionPath string) error {
	pidFile := filepath.Join(sessionPath, "bridge.pid")
	if _, err := os.Stat(pidFile); err != nil {
		return nil
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return NewHTTPException(409, whatsappBridgeUnknown)
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return NewHTTPException(409, whatsappBridgeUnknown)
	}
	lines := pystr.Splitlines(text, false)
	bigPid := bridgePidLine(lines, 0)
	if bigPid == nil {
		return NewHTTPException(409, whatsappBridgeUnknown)
	}
	if !bigPid.IsInt64() {
		return errors.New("Python int too large to convert to C int")
	}
	pid := int(bigPid.Int64())
	if !whatsappPidExists(pid) {
		return nil
	}
	recordedStart := bridgePidLine(lines, 1)
	if recordedStart == nil {
		return NewHTTPException(409, whatsappBridgeUnknown)
	}
	currentStart := whatsappProcessStartTime(pid)
	if currentStart == nil {
		return NewHTTPException(409, whatsappBridgeUnknown)
	}
	if pyrepr.Equal(currentStart, recordedStart) {
		return NewHTTPException(409, "The WhatsApp bridge is still running. Stop it before re-pairing.")
	}
	return nil
}

// Moves the session aside into a fresh backup dir and recreates it empty, never deleting keys
func backupWhatsappSession(sessionPath string) error {
	if err := assertWhatsappBridgeStopped(sessionPath); err != nil {
		return err
	}
	if st, err := os.Lstat(sessionPath); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return NewHTTPException(409, "The WhatsApp session is a symbolic link. Back it up manually before re-pairing.")
	}
	backup := ""
	err := func() error {
		if _, err := os.Stat(sessionPath); err != nil {
			return nil
		}
		stamp := time.Now().UTC().Format("20060102T150405Z")
		dir, err := os.MkdirTemp(filepath.Dir(sessionPath), "session-backup-"+stamp+"-")
		if err != nil {
			return err
		}
		backup = filepath.Join(dir, "session")
		if err := whatsappRename(sessionPath, backup); err != nil {
			return err
		}
		return os.Mkdir(sessionPath, 0o700)
	}()
	if err == nil {
		return nil
	}
	detail := "Could not back up the WhatsApp session: " + pypath.OSErrorText(err) + ". Pairing was not started."
	if backup != "" {
		if _, statErr := os.Stat(backup); statErr == nil {
			detail += " The previous session is preserved at " + backup + "."
		}
	}
	return NewHTTPException(500, detail)
}
