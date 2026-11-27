// whisper.cpp's command line engine, the local speech recognition of ARM64 Windows, where faster-whisper has no build
package packages

import (
	_ "embed"
	"fmt"

	"github.com/nickheyer/icebath/internal/packages"
)

// Name of the package
const WhisperCpp = "whispercpp-cpu"

//go:embed lock.json
var lock []byte

func Register(registry *packages.Registry) {
	gaps := map[string]string{}
	for _, target := range packages.Targets {
		if target != "win32-arm64" {
			gaps[target] = "faster-whisper turns speech into text here, so whisper.cpp is not needed."
		}
	}
	registry.Register(packages.Declared{Tools: []packages.Package{{
		Name: WhisperCpp, Pin: packages.Pins(lock)[WhisperCpp], Optional: true, Gaps: gaps,
		Binary: map[string]string{"win32-arm64": "whisper-cli.exe"},
		Probe:  []string{"--help"},
		URLs: func(version, target string) ([]string, error) {
			if target != "win32-arm64" {
				return nil, fmt.Errorf("whisper.cpp has no download for %s", target)
			}
			return []string{"https://github.com/ggml-org/whisper.cpp/releases/download/" + version + "/whisper-bin-win-cpu-arm64.zip"}, nil
		},
		// Pinned by hand: the build tags of the ARM64 Windows archive are not the release tags upstream lists
	}}})
}
