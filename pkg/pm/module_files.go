package pm

import (
	"os"
	"path/filepath"
	"strings"
)

// Folders a module can live in under one site folder, the folder itself and every folder its path files add
func moduleRoots(site string) []string {
	roots := []string{site}
	entries, err := os.ReadDir(site)
	if err != nil {
		return roots
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pth") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(site, entry.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "import ") || strings.HasPrefix(line, "import\t") {
				continue
			}
			if !filepath.IsAbs(line) {
				line = filepath.Join(site, line)
			}
			if st, err := os.Stat(line); err == nil && st.IsDir() {
				roots = append(roots, line)
			}
		}
	}
	return roots
}

// Whether a folder holds the named module as a package folder, a source file or a compiled extension
func holdsModule(dir, name string) bool {
	if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
		return true
	}
	if isRegularFile(filepath.Join(dir, name+".py")) {
		return true
	}
	for _, pattern := range []string{name + ".*.so", name + ".so", name + ".*.pyd", name + ".pyd"} {
		if matches, _ := filepath.Glob(filepath.Join(dir, pattern)); len(matches) > 0 {
			return true
		}
	}
	return false
}

// Whether a dotted module is installed under the given folders, each parent a package folder in at least one of them
func moduleInstalled(roots []string, module string) bool {
	parts := strings.Split(module, ".")
	for _, part := range parts[:len(parts)-1] {
		var next []string
		for _, root := range roots {
			if st, err := os.Stat(filepath.Join(root, part)); err == nil && st.IsDir() {
				next = append(next, filepath.Join(root, part))
			}
		}
		if roots = next; len(roots) == 0 {
			return false
		}
	}
	last := parts[len(parts)-1]
	if last == "" {
		return false
	}
	for _, root := range roots {
		if holdsModule(root, last) {
			return true
		}
	}
	return false
}
