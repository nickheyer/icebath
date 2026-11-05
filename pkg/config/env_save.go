package config

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/home"
)

// Stream that receives env write refusals, standard error outside tests
var EnvRefusalOut io.Writer = os.Stderr

// Package manager owning the install from ICEBATH_MANAGED or the home marker, empty when unmanaged
func ManagedSystem(h string) string {
	marker := strings.ToLower(strings.TrimSpace(os.Getenv("ICEBATH_MANAGED")))
	set := marker != ""
	if !set {
		path := filepath.Join(h, ".managed")
		if _, err := os.Stat(path); err == nil {
			set = true
			if b, err := os.ReadFile(path); err == nil {
				marker = strings.ToLower(strings.TrimSpace(strings.ToValidUTF8(string(b), "�")))
			} else {
				marker = ""
			}
		}
	}
	switch {
	case !set, marker == "brew", marker == "homebrew", marker == "false", marker == "0", marker == "no", marker == "off":
		return ""
	case marker == "", marker == "true", marker == "1", marker == "yes":
		return "nixos"
	}
	return marker
}

// User facing refusal for an action on a package manager managed install
func FormatManagedMessage(h, action string) string {
	system := ManagedSystem(h)
	if system == "" {
		system = "a package manager"
	}
	return fmt.Sprintf("Cannot %s: this Icebath installation is managed by %s.\nUse your package manager to upgrade or reinstall Icebath.", action, system)
}

// Refusal text when a managed install or an administrator pinned key forbids writing key, empty when allowed
func EnvWriteRefusal(h, key, action string) string {
	if ManagedSystem(h) != "" {
		return FormatManagedMessage(h, action+" "+key)
	}
	if IsEnvManaged(key) {
		source := "the managed scope"
		if dir := ManagedDir(); dir != "" {
			source = filepath.Join(dir, ".env")
		}
		return fmt.Sprintf("Cannot %s %s: it is managed by your administrator (%s) and cannot be changed.", action, key, source)
	}
	return ""
}

// Dotenv quoting of the save path, quoting values with a hash, a quote or any whitespace
func quoteSavedEnvValue(value string) string {
	if value == "" || !(strings.ContainsAny(value, "#\"'") || strings.IndexFunc(value, unicode.IsSpace) >= 0) {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	return `"` + strings.ReplaceAll(escaped, `"`, `\"`) + `"`
}

// Families whose every member steers execution or config injection, matched by prefix
var envVarNameDenyPrefixes = []string{"LD_", "DYLD_", "GIT_CONFIG_"}

// Names the generic env writer refuses because they steer subprocess execution or icebath runtime and security policy
var envVarNameDenylist = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true, "LD_DEBUG": true,
	"DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true, "DYLD_FRAMEWORK_PATH": true,
	"DYLD_FALLBACK_LIBRARY_PATH": true, "DYLD_FALLBACK_FRAMEWORK_PATH": true,
	"PYTHONPATH": true, "PYTHONHOME": true, "PYTHONSTARTUP": true, "PYTHONUSERBASE": true,
	"PYTHONEXECUTABLE": true, "PYTHONNOUSERSITE": true, "PYTHONBREAKPOINT": true, "PYTHONCASEOK": true,
	"NODE_OPTIONS": true, "NODE_PATH": true,
	"PERL5OPT": true, "PERL5LIB": true, "PERLLIB": true, "RUBYOPT": true, "RUBYLIB": true, "CLASSPATH": true,
	"JAVA_TOOL_OPTIONS": true, "_JAVA_OPTIONS": true, "JDK_JAVA_OPTIONS": true,
	"GOFLAGS": true, "RUSTFLAGS": true,
	"PATH": true, "SHELL": true, "BROWSER": true, "EDITOR": true, "VISUAL": true, "PAGER": true, "MANPAGER": true,
	"GIT_SSH_COMMAND": true, "GIT_EXEC_PATH": true, "GIT_SHELL": true,
	"GIT_SSH": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true, "SUDO_ASKPASS": true,
	"GIT_EDITOR": true, "GIT_SEQUENCE_EDITOR": true, "GIT_PAGER": true, "GIT_EXTERNAL_DIFF": true,
	"GIT_PROXY_COMMAND": true, "GIT_TEMPLATE_DIR": true, "GIT_DIR": true,
	"BASH_ENV": true, "ENV": true, "ZDOTDIR": true, "PROMPT_COMMAND": true, "VIMINIT": true, "EXINIT": true,
	"ICEBATH_HOME": true, "ICEBATH_PROFILE": true, "ICEBATH_CONFIG": true, "ICEBATH_ENV": true,
	"ICEBATH_CONFIG_PATH": true, "ICEBATH_ENV_PATH": true,
	"ICEBATH_OPTIONAL_MCPS": true,
	"ICEBATH_YOLO_MODE":     true, "ICEBATH_ACCEPT_HOOKS": true, "ICEBATH_REDACT_SECRETS": true,
	"ICEBATH_INTERACTIVE": true, "ICEBATH_EXEC_ASK": true, "ICEBATH_GATEWAY_SESSION": true,
	"ICEBATH_CRON_SESSION": true, "ICEBATH_SINGLE_QUERY_SESSION": true,
	"ICEBATH_SESSION_KEY": true, "ICEBATH_SESSION_PLATFORM": true,
}

// Windows env names compare case insensitively, swapped by tests to exercise both policies
var envPolicyWindows = runtime.GOOS == "windows"

// External provider launch env names, installed by pkg/provider so ACP argv authority stays unwritable
var EnvWriterLaunchDenylist = func() map[string]bool { return map[string]bool{} }

// Name used for env policy comparisons, upper cased under the Windows policy
func envVarPolicyName(key string) string {
	if envPolicyWindows {
		return strings.ToUpper(key)
	}
	return key
}

// Rejects an invalid name or one on the writer denylist before a generic persistence write
func ValidateEnvNameForWrite(key string) error {
	if !envNameRe.MatchString(key) {
		return fmt.Errorf("Invalid environment variable name: '%s'", key)
	}
	policy := envVarPolicyName(key)
	denied := envVarNameDenylist[policy]
	for name := range EnvWriterLaunchDenylist() {
		denied = denied || envVarPolicyName(name) == policy
	}
	for _, prefix := range envVarNameDenyPrefixes {
		denied = denied || strings.HasPrefix(policy, prefix)
	}
	if denied {
		return fmt.Errorf("Environment variable '%s' is on the writer denylist. "+
			"Names that influence subprocess execution (LD_PRELOAD, PYTHONPATH, PATH, EDITOR, ...) "+
			"or Icebath runtime location and security policy (ICEBATH_HOME, ICEBATH_YOLO_MODE, ...) "+
			"cannot be persisted via the env writer. If you really need this, edit the profile .env directly.", key)
	}
	return nil
}

// Saves or replaces one assignment in the home .env, refused writes printed and skipped
func SaveEnvValue(h, key, value string) error {
	return SaveEnvValueContext(context.Background(), h, key, value)
}

// Saves one .env assignment, matching export lines too, and publishes it to the scope or process env
func SaveEnvValueContext(ctx context.Context, h, key, value string) error {
	if envWriteBlocked(h, key, "set") {
		return nil
	}
	if err := ValidateEnvNameForWrite(key); err != nil {
		return err
	}
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", ""), "\r", "")
	if err := checkASCIICredential(key, value); err != nil {
		return err
	}
	if err := EnsureIcebathHome(h); err != nil {
		return err
	}
	path := home.EnvPath(h)
	err := withEnvWriteLock(path, func() error {
		_, statErr := os.Stat(path)
		exists := statErr == nil
		var lines []string
		if exists {
			read, err := readSanitizedEnvLines(path)
			if err != nil {
				return err
			}
			lines = read
		}
		assignment := key + "=" + quoteSavedEnvValue(value) + "\n"
		idx := slices.IndexFunc(lines, func(line string) bool { return envLineDefines(line, key) })
		if idx >= 0 {
			lines[idx] = assignment
		} else {
			if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
				lines[n-1] += "\n"
			}
			lines = append(lines, assignment)
		}
		return writeEnvLinesMode(h, path, lines, exists)
	})
	if err != nil {
		return err
	}
	publishEnvValue(ctx, key, &value)
	return nil
}

// Home .env lines decoded as utf-8-sig with replacement and normalized like the upstream reader
func readSanitizedEnvLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := readEnvTextLines(raw)
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return SanitizeEnvLines(lines), nil
}
