package pm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
)

var nodeToolPackages = map[string]string{"node": "node", "npm": "npm", "npx": "npm"}

// Path of the PM selected node, npm or npx or a caller owned tool, empty when PM lacks it
func FindNodeExecutable(ctx context.Context, command string) (string, error) {
	if strings.ContainsAny(command, "/\\") {
		if runtime.GOOS == "windows" {
			if regularFile(command) {
				return command, nil
			}
			return "", nil
		}
		found, _ := pyshutil.Which(command, nil)
		return found, nil
	}
	base := strings.ToLower(command)
	for _, suffix := range []string{".cmd", ".exe", ".ps1"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if packageName, ok := nodeToolPackages[base]; ok {
		installed, err := Installed(ctx, packageName, false)
		if err != nil || installed == nil || installed.Binary == "" {
			return "", err
		}
		if base != "npx" {
			return installed.Binary, nil
		}
		for _, name := range home.CandidateNodeCommandNames("npx") {
			if candidate := filepath.Join(filepath.Dir(installed.Binary), name); regularFile(candidate) {
				return candidate, nil
			}
		}
		return "", nil
	}
	if runtime.GOOS != "windows" {
		found, _ := pyshutil.Which(command, nil)
		return found, nil
	}
	var directories []string
	for _, d := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		if d != "" {
			directories = append(directories, d)
		}
	}
	for _, name := range home.CandidateNodeCommandNames(command) {
		for _, directory := range directories {
			if candidate := filepath.Join(directory, name); regularFile(candidate) {
				return candidate, nil
			}
		}
	}
	return "", nil
}

// Whether path names a regular file through any symlink
func regularFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
