package home

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Error for a host where no temp candidate accepts a new file
var errNoTempDir = errors.New("no usable temporary directory found")

// Temp directory candidates in the order the temp variables and then the platform fallbacks name them
func tempCandidates() []string {
	var out []string
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		if v := os.Getenv(name); v != "" {
			out = append(out, v)
		}
	}
	out = append(out, platformTempDirs()...)
	if wd, err := os.Getwd(); err == nil {
		out = append(out, wd)
	} else {
		out = append(out, ".")
	}
	return out
}

// Reports whether a file can be created, written and removed in dir
func acceptsFiles(dir string) bool {
	f, err := os.CreateTemp(dir, "tmp")
	if err != nil {
		return false
	}
	name := f.Name()
	_, werr := f.WriteString("blat")
	cerr := f.Close()
	rerr := os.Remove(name)
	return werr == nil && cerr == nil && rerr == nil
}

// First temp candidate made absolute that accepts a new file, errNoTempDir naming every candidate otherwise
func usableTempDir() (string, error) {
	candidates := tempCandidates()
	for _, c := range candidates {
		dir := c
		if dir != "." {
			if abs, err := filepath.Abs(dir); err == nil {
				dir = abs
			}
		}
		if acceptsFiles(dir) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w in %s", errNoTempDir, strings.Join(candidates, ", "))
}
