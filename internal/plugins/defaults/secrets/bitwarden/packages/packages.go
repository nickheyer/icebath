// Tools the Bitwarden store runs: bws, the Secrets Manager program
package packages

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Name of the bws package
const Bws = "bws"

//go:embed lock.json
var lock []byte

func Register(registry *packages.Registry) {
	registry.Register(packages.Declared{Tools: []packages.Package{bws(packages.Pins(lock)[Bws])}})
}

// Bitwarden's static musl build serving every Linux userland, the universal build every Mac; no Android build
func bws(pin packages.Pin) packages.Package {
	return packages.Package{
		Name: Bws, Pin: pin, Optional: true,
		Gaps:   map[string]string{packages.Bionic: "upstream does not ship an Android binary"},
		Binary: map[string]string{"posix": "bws", "win32": "bws.exe"},
		URLs: func(version, target string) ([]string, error) {
			triple, ok := packages.RustTriple[target]
			if !ok {
				return nil, fmt.Errorf("no download for target %s", target)
			}
			platform := strings.ReplaceAll(triple, "linux-gnu", "linux-musl")
			if strings.HasPrefix(target, "darwin") {
				platform = "macos-universal"
			}
			return []string{fmt.Sprintf("https://github.com/bitwarden/sdk-sm/releases/download/bws-v%s/bws-%s-%s.zip", version, platform, version)}, nil
		},
		// The zip keeps no mode bits, so the binary is made executable
		Stage: func(b *packages.Build) error {
			if err := packages.Flatten(b.Dir); err != nil {
				return err
			}
			bin := filepath.Join(b.Dir, "bws")
			if strings.HasPrefix(b.Target, "win32") {
				bin += ".exe"
			}
			if fsx.IsFile(bin) {
				return os.Chmod(bin, 0o755)
			}
			return nil
		},
	}
}
