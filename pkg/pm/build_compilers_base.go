package pm

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Executable a command resolves to on a search path the way shutil which finds it, nil path meaning PATH
func whichOn(cmd string, path *string) string {
	dirname, base := splitCommand(cmd)
	var dirs []string
	if dirname != "" {
		dirs = []string{dirname}
	} else {
		search := ""
		if path != nil {
			search = *path
		} else if value, set := os.LookupEnv("PATH"); set {
			search = value
		} else {
			search = defaultExecPath()
		}
		if search == "" {
			return ""
		}
		dirs = strings.Split(search, string(os.PathListSeparator))
		if runtime.GOOS == "windows" {
			if _, set := os.LookupEnv("NoDefaultCurrentDirectoryInExePath"); !set {
				dirs = append([]string{"."}, dirs...)
			}
		}
	}
	files := []string{base}
	if runtime.GOOS == "windows" {
		source := os.Getenv("PATHEXT")
		if source == "" {
			source = ".COM;.EXE;.BAT;.CMD;.VBS;.JS;.WS;.MSC"
		}
		var exts []string
		for _, ext := range strings.Split(source, string(os.PathListSeparator)) {
			if ext != "" {
				exts = append(exts, strings.TrimRight(ext, "."))
			}
		}
		files = nil
		for _, ext := range exts {
			if strings.HasSuffix(strings.ToUpper(base), strings.ToUpper(ext)) {
				files = []string{base}
				break
			}
		}
		for _, ext := range exts {
			files = append(files, base+ext)
		}
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		norm := dir
		if runtime.GOOS == "windows" {
			norm = strings.ToLower(strings.ReplaceAll(dir, "/", `\`))
		}
		if seen[norm] {
			continue
		}
		seen[norm] = true
		for _, file := range files {
			name := pypath.Join(dir, file)
			if dir == "" {
				name = file
			}
			if st, err := os.Stat(name); err == nil && !st.IsDir() && canExecute(name) {
				return name
			}
		}
	}
	return ""
}

// Directory and file parts of a command the way os path split divides them
func splitCommand(cmd string) (string, string) {
	i := strings.LastIndexAny(cmd, string(filepath.Separator)+"/")
	if i < 0 {
		return "", cmd
	}
	head, tail := cmd[:i+1], cmd[i+1:]
	if trimmed := strings.TrimRight(head, string(filepath.Separator)+"/"); trimmed != "" {
		head = trimmed
	}
	return head, tail
}
