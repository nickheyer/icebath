package selfupdate

import (
	"os"
	"path/filepath"

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
