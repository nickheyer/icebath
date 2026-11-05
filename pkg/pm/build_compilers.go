package pm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

var (
	compilerLinkers  = [][2]string{{"CC", "LDSHARED"}, {"CXX", "LDCXXSHARED"}}
	gccFamily        = map[string]string{"clang": "gcc", "clang++": "g++"}
	missingCompilers = []*regexp.Regexp{
		regexp.MustCompile(`No such file or directory: '([^']+)'`),
		regexp.MustCompile(`command '([^']+)' failed: No such file or directory`),
		regexp.MustCompile(`No CMAKE_(?:C|CXX)_COMPILER could be found`),
	}
	compilerName = regexp.MustCompile(`^(?:clang(?:\+\+)?|g?cc|g\+\+|c\+\+)(?:-[\p{Nd}.]+)?$`)

	recordedMu     sync.Mutex
	recordedByPath = map[string]any{}
)

const compilerRemedy = "%s: install a C/C++ compiler (Debian/Ubuntu/WSL: `sudo apt install build-essential`; " +
	"Fedora: `sudo dnf install gcc gcc-c++`; macOS: `xcode-select --install`) " +
	"or point CC/CXX at one, then retry"

const recordedToolchainCode = "import json, sysconfig; print(json.dumps({k: sysconfig.get_config_var(k) or '' for k in ('CC', 'CXX', 'LDSHARED', 'LDCXXSHARED', 'AR')}))"

// Build toolchain variables an interpreter recorded at its own build, cached per interpreter, empty when unreadable
func recordedToolchain(python string) any {
	recordedMu.Lock()
	defer recordedMu.Unlock()
	if cached, ok := recordedByPath[python]; ok {
		return cached
	}
	var recorded any = map[string]any{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", recordedToolchainCode)
	cmd.WaitDelay = time.Second
	if out, err := cmd.Output(); err == nil && utf8.Valid(out) {
		if v, jerr := loadJSON([]byte(universalNewlines(string(out)))); jerr == nil {
			recorded = v
		}
	}
	recordedByPath[python] = recorded
	return recorded
}

// Recorded value of a toolchain variable as text, refusing a value Python could not read or split
func recordedText(recorded any, key string) (string, error) {
	m, ok := recorded.(map[string]any)
	if !ok {
		return "", &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'get'", pyrepr.TypeName(recorded))}
	}
	v, ok := m[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &PyError{Class: "AttributeError", Message: fmt.Sprintf("'%s' object has no attribute 'split'", pyrepr.TypeName(v))}
	}
	return s, nil
}

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

// Compiler substitutions for a Linux host that lacks the recorded clang family but has gcc
func linuxDefaults(recorded any, env map[string]string) (map[string]string, error) {
	var search *string
	if value, ok := env["PATH"]; ok {
		search = &value
	}
	defaults := map[string]string{}
	for _, pair := range compilerLinkers {
		compilerVar, linkerVar := pair[0], pair[1]
		command, err := recordedText(recorded, compilerVar)
		if err != nil {
			return nil, err
		}
		program, _, _ := strings.Cut(command, " ")
		substitute, known := gccFamily[program]
		if _, set := env[compilerVar]; set || !known || whichOn(program, search) != "" || whichOn(substitute, search) == "" {
			continue
		}
		defaults[compilerVar] = substitute + command[len(program):]
		linker, err := recordedText(recorded, linkerVar)
		if err != nil {
			return nil, err
		}
		if _, set := env[linkerVar]; !set {
			if first, _, _ := strings.Cut(linker, " "); first == program {
				defaults[linkerVar] = substitute + linker[len(program):]
			}
		}
	}
	archiver, err := recordedText(recorded, "AR")
	if err != nil {
		return nil, err
	}
	if archiver != "" && whichOn(archiver, search) == "" && whichOn("ar", search) != "" {
		defaults["AR"] = "ar"
	}
	return defaults, nil
}

// Defaults for build variables the pinned interpreter records but this host lacks
func PinnedBuildDefaults(python string, env map[string]string) (map[string]string, error) {
	switch runtime.GOOS {
	case "darwin":
		return map[string]string{"AR": "/usr/bin/ar"}, nil
	case "linux":
		return linuxDefaults(recordedToolchain(python), env)
	}
	return map[string]string{}, nil
}

// Install hint for a build that failed because no compiler exists, empty otherwise
func MissingCompilerRemedy(output string) string {
	for _, pattern := range missingCompilers {
		for _, m := range pattern.FindAllStringSubmatch(output, -1) {
			if len(m) == 1 {
				return fmt.Sprintf(compilerRemedy, "this package compiles native code and no compiler was found")
			}
			if compilerName.MatchString(pyPathName(m[1])) {
				return fmt.Sprintf(compilerRemedy, "the build needs `"+m[1]+"`, which is not on PATH")
			}
		}
	}
	return ""
}
