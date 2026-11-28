package tools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/fsx"
)

// Binary of the package npx -y would run and the arguments after it, when npx already holds that package in its cache: the server then
// runs without a resident npm exec parent
func npxCached(args []string) (string, []string, bool) {
	rest := args
	for len(rest) > 0 && (rest[0] == "-y" || rest[0] == "--yes") {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return "", nil, false
	}
	for _, arg := range rest[1:] {
		if arg == "-y" || arg == "--yes" {
			return "", nil, false
		}
	}
	spec := rest[0]
	if spec == "" || strings.HasPrefix(spec, "-") || strings.Contains(strings.TrimPrefix(spec, "@"), "@") {
		return "", nil, false
	}
	cache := os.Getenv("npm_config_cache")
	if cache == "" {
		cache = filepath.Join(fsx.ExpandHome("~"), ".npm")
	}
	root := filepath.Join(cache, "_npx")
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", nil, false
	}
	for _, entry := range entries {
		var manifest struct {
			Dependencies map[string]any `json:"dependencies"`
		}
		if !readJSON(filepath.Join(root, entry.Name(), "package.json"), &manifest) {
			continue
		}
		if _, ok := manifest.Dependencies[spec]; !ok {
			continue
		}
		var pkg struct {
			Bin json.RawMessage `json:"bin"`
		}
		if !readJSON(filepath.Join(root, entry.Name(), "node_modules", spec, "package.json"), &pkg) {
			continue
		}
		name := ""
		var single string
		var many map[string]any
		switch {
		case json.Unmarshal(pkg.Bin, &single) == nil:
			name = filepath.Base(spec)
		case json.Unmarshal(pkg.Bin, &many) == nil && len(many) == 1:
			for k := range many {
				name = k
			}
		default:
			continue
		}
		bin := filepath.Join(root, entry.Name(), "node_modules", ".bin")
		candidates := []string{filepath.Join(bin, name)}
		if runtime.GOOS == "windows" {
			candidates = []string{filepath.Join(bin, name+".cmd"), filepath.Join(bin, name+".exe")}
		}
		for _, candidate := range candidates {
			if info, err := os.Stat(candidate); err == nil && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0) {
				return candidate, append([]string(nil), rest[1:]...), true
			}
		}
	}
	return "", nil, false
}

// Decodes a JSON file, a leading BOM dropped; false when it is missing or not JSON
func readJSON(path string, into any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")), into) == nil
}
