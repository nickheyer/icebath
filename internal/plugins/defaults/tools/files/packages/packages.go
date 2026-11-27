// Tools the file tools run: ripgrep, which search_files prefers
package packages

import (
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Name of the ripgrep package
const Ripgrep = "ripgrep"

//go:embed lock.json
var lock []byte

func Register(registry *packages.Registry) {
	registry.Register(packages.Declared{Tools: []packages.Package{ripgrep(packages.Pins(lock)[Ripgrep])}})
}

// BurntSushi's static build, the musl one serving glibc and Termux too, ARM64 Windows carrying the VC runtime it links from Python's entry
func ripgrep(pin packages.Pin) packages.Package {
	return packages.Package{
		Name: Ripgrep, Pin: pin,
		TargetDeps: map[string][]string{"win32-arm64": {packages.Python}},
		Binary:     map[string]string{"win32": "rg.exe", "posix": "rg"},
		URLs: func(version, target string) ([]string, error) {
			if target == packages.Bionic {
				target = "linux-arm64"
			}
			triple, ok := packages.RustTriple[target]
			if !ok {
				return nil, fmt.Errorf("no download for target %s", target)
			}
			triple = strings.ReplaceAll(triple, "-unknown-linux-gnu", "-unknown-linux-musl")
			ext := "tar.gz"
			if strings.HasPrefix(target, "win32") {
				ext = "zip"
			}
			return []string{fmt.Sprintf("https://github.com/BurntSushi/ripgrep/releases/download/%s/ripgrep-%s-%s.%s", version, version, triple, ext)}, nil
		},
		Latest: func(cfg *config.Config, target, locked string) ([]string, error) {
			return packages.GitHubTags(cfg, "BurntSushi/ripgrep", "")
		},
		Stage: func(b *packages.Build) error {
			if err := packages.Flatten(b.Dir); err != nil {
				return err
			}
			if b.Target != "win32-arm64" {
				return nil
			}
			python, err := b.Entry(packages.Python)
			if err != nil {
				return packages.Failure(Ripgrep, "ARM64 rg.exe needs vcruntime140.dll from python, which is not installed", "")
			}
			runtime := filepath.Join(python, "vcruntime140.dll")
			if !fsx.IsFile(runtime) {
				return packages.Failure(Ripgrep, "python's entry has no vcruntime140.dll", "")
			}
			return fsx.Copy(runtime, filepath.Join(b.Dir, "vcruntime140.dll"))
		},
		Verify: func(entry, target string, standard func() string) string {
			if target == packages.Bionic {
				return packages.Present(filepath.Join(entry, "rg"), entry, target)
			}
			return standard()
		},
	}
}
