package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"

	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

// Environment variable a current desktop update shim exports with its status file
const UpdateStatusFileEnv = "ICEBATH_UPDATE_STATUS_FILE"

// Status file of the watching desktop update shim, empty when no UI is discoverable
func UpdateStatusFile() string {
	if exported := pystr.Strip(os.Getenv(UpdateStatusFileEnv)); exported != "" {
		return exported
	}
	return statusFromMarker()
}

// Status file an old shim writes beside the temp dir, found through the pid on the update marker's first line
func statusFromMarker() string {
	data, err := os.ReadFile(updaterecord.UpdateMarkerPath())
	if err != nil {
		return ""
	}
	text, err := pystr.DecodeUTF8Sig(data)
	if err != nil {
		return ""
	}
	lines := pystr.Splitlines(text, false)
	if len(lines) == 0 {
		return ""
	}
	pid, ok := pyint.Parse(pystr.Strip(lines[0]))
	if !ok || pid.Sign() <= 0 {
		return ""
	}
	tmp, err := pypath.Gettempdir()
	if err != nil {
		return ""
	}
	candidate := filepath.Join(tmp, "icebath-update-status."+pid.String())
	if !isRegular(candidate) {
		return ""
	}
	return candidate
}

// Reports a running stage to a watching update UI through an atomic replace, never failing
func PublishStage(message string) {
	path := UpdateStatusFile()
	if path == "" {
		return
	}
	payload, err := pyjson.Dumps(pyjson.ObjectOf("status", "running", "message", message))
	if err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".update-stage-*")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, werr := f.WriteString(payload)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// Environment variable marking that an update progress panel was spawned for this update chain
const UpdateUIActiveEnv = "ICEBATH_UPDATE_UI_ACTIVE"

// True when the last forty handoff log lines show a browser window or status panel started
func uiPresentInLog(logText string) bool {
	lines := pystr.Splitlines(logText, false)
	if len(lines) > 40 {
		lines = lines[len(lines)-40:]
	}
	tail := strings.Join(lines, "\n")
	return strings.Contains(tail, "app window") || strings.Contains(tail, "status panel pid=")
}

// Starts the macOS status panel of the new tree when a shim watches with no renderer up, never failing
func ensurePanel(updateRoot string) {
	if os.Getenv(UpdateUIActiveEnv) != "" || runtime.GOOS != "darwin" {
		return
	}
	if UpdateStatusFile() == "" {
		return
	}
	panel := filepath.Join(updateRoot, "scripts", "desktop-update", "update-panel.js")
	if !isRegular(panel) {
		return
	}
	logPath := filepath.Join(home.Root(), "logs", "desktop-update-handoff.log")
	if isRegular(logPath) {
		raw, err := os.ReadFile(logPath)
		if err != nil {
			return
		}
		text := []rune(pystr.DecodeUTF8SigReplace(raw))
		if len(text) > 8000 {
			text = text[len(text)-8000:]
		}
		if uiPresentInLog(string(text)) {
			return
		}
	}
	child := exec.Command("/usr/bin/osascript", "-l", "JavaScript", panel, UpdateStatusFile())
	child.SysProcAttr = posixPauseDetachedAttr()
	if err := child.Start(); err != nil {
		return
	}
	go func() { _ = child.Wait() }()
	os.Setenv(UpdateUIActiveEnv, "1")
}
