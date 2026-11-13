package backend

import (
	"encoding/json"
	"os"
	"path/filepath"
	"unicode/utf16"
)

const (
	BootstrapCompleteMarkerName  = ".icebath-bootstrap-complete"
	BootstrapMarkerSchemaVersion = 1
	canonicalInstallStampName    = "install-stamp.json"
)

// ActiveRuntimeState says whether the local install launches and whether a desktop bootstrap marked it
type ActiveRuntimeState struct {
	HasValidMarker         bool           `json:"hasValidMarker"`
	ShouldUseActiveRuntime bool           `json:"shouldUseActiveRuntime"`
	UsabilityReason        string         `json:"usabilityReason"`
	CanonicalInstallStamp  map[string]any `json:"canonicalInstallStamp"`
}

// Whether a bootstrap marker has the schema version and a pinned commit of at least seven characters
func HasValidBootstrapMarker(marker any, schemaVersion int) bool {
	fields, ok := marker.(map[string]any)
	if !ok {
		return false
	}
	if v, ok := fields["schemaVersion"].(float64); !ok || v != float64(schemaVersion) {
		return false
	}
	commit, ok := fields["pinnedCommit"].(string)
	return ok && len(utf16.Encode([]rune(commit))) >= 7
}

// Runtime usability decides the launch, the marker only records how the install was made
func ClassifyActiveRuntime(marker any, schemaVersion int, runtimeUsable bool) ActiveRuntimeState {
	hasValidMarker := HasValidBootstrapMarker(marker, schemaVersion)
	if !runtimeUsable {
		return ActiveRuntimeState{HasValidMarker: hasValidMarker, UsabilityReason: "unusable"}
	}
	return ActiveRuntimeState{HasValidMarker: hasValidMarker, ShouldUseActiveRuntime: true, UsabilityReason: "usable"}
}

// Parsed JSON of a file, nil when unreadable or invalid
func readJSONOrNil(path string) any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var v any
	if json.Unmarshal([]byte(utf8Lossy(raw)), &v) != nil {
		return nil
	}
	return v
}

// Install stamp the bootstrap wrote in the active root, nil unless it names a source
func readCanonicalInstallStamp(activeRoot string) map[string]any {
	stamp, ok := readJSONOrNil(filepath.Join(activeRoot, canonicalInstallStampName)).(map[string]any)
	if !ok {
		return nil
	}
	if _, ok := stamp["source"].(string); !ok {
		return nil
	}
	return stamp
}

// State of the active install root, classified from its marker and usability with its install stamp attached
func ActiveRuntimeStateOf(activeRoot string, runtimeUsable bool) ActiveRuntimeState {
	state := ClassifyActiveRuntime(readJSONOrNil(filepath.Join(activeRoot, BootstrapCompleteMarkerName)), BootstrapMarkerSchemaVersion, runtimeUsable)
	state.CanonicalInstallStamp = readCanonicalInstallStamp(activeRoot)
	return state
}
