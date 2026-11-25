//go:build !icebath_core && !icebath_noextras

package pm

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/errchain"
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
		return "", &ClassError{Class: "AttributeError", Message: fmt.Sprintf("the recorded toolchain must be a mapping, got %s", errchain.Kind(recorded))}
	}
	v, ok := m[key]
	if !ok {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", &ClassError{Class: "AttributeError", Message: fmt.Sprintf("the recorded toolchain value %s must be text, got %s", key, errchain.Kind(v))}
	}
	return s, nil
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
			if compilerName.MatchString(pathName(m[1])) {
				return fmt.Sprintf(compilerRemedy, "the build needs `"+m[1]+"`, which is not on PATH")
			}
		}
	}
	return ""
}
