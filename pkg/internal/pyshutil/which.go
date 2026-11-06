package pyshutil

import (
	"os"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Search path shutil.which falls back to when PATH is unset, os.defpath of the host
func defaultPath() string {
	switch runtime.GOOS {
	case "windows":
		return `.;C:\bin`
	case "darwin", "freebsd", "netbsd", "openbsd", "dragonfly":
		return "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	return "/bin:/usr/bin"
}

// Executable a command names on path or PATH the way shutil.which finds it, false when absent
func Which(cmd string, path *string) (string, bool) {
	dirname, base := pypath.Split(cmd)
	var dirs []string
	if dirname != "" {
		dirs = []string{dirname}
	} else {
		var search string
		if path != nil {
			search = *path
		} else if v, ok := os.LookupEnv("PATH"); ok {
			search = v
		} else {
			search = defaultPath()
		}
		if search == "" {
			return "", false
		}
		dirs = strings.Split(search, string(os.PathListSeparator))
		if runtime.GOOS == "windows" {
			if _, set := os.LookupEnv("NoDefaultCurrentDirectoryInExePath"); !set || strings.Contains(base, `\`) {
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
		for _, e := range strings.Split(source, string(os.PathListSeparator)) {
			if e != "" {
				exts = append(exts, strings.TrimRight(e, "."))
			}
		}
		files = nil
		upper := strings.ToUpper(base)
		for _, e := range exts {
			if strings.HasSuffix(upper, strings.ToUpper(e)) {
				files = []string{base}
				break
			}
		}
		for _, e := range exts {
			files = append(files, base+e)
		}
	}
	seen := map[string]bool{}
	for _, dir := range dirs {
		norm := pypath.Normcase(dir)
		if seen[norm] {
			continue
		}
		seen[norm] = true
		for _, f := range files {
			name := pypath.Join(dir, f)
			if st, err := os.Stat(name); err == nil && !st.IsDir() && accessExecutable(name) {
				return name, true
			}
		}
	}
	return "", false
}
