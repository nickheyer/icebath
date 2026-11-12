package winenv

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
)

const jsSpace = `\t\n\v\f\r \x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}`

var (
	regLine  = regexp.MustCompile(`^([^` + jsSpace + `]+)[` + jsSpace + `]+(?:REG_SZ|REG_EXPAND_SZ|REG_MULTI_SZ|REG_DWORD|REG_QWORD|REG_BINARY|REG_NONE)[` + jsSpace + `]+(.*)$`)
	envRef   = regexp.MustCompile(`%([^%]+)%`)
	lineEnds = regexp.MustCompile(`\r?\n`)
)

// Trims the whitespace JavaScript String.prototype.trim removes
func jsTrim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		switch r {
		case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
			return true
		}
		return r >= 0x2000 && r <= 0x200a
	})
}

// Raw value of name from reg query output, false when its line is absent
func ParseRegQueryValue(stdout, name string) (string, bool) {
	if stdout == "" || name == "" {
		return "", false
	}
	for _, raw := range lineEnds.Split(stdout, -1) {
		m := regLine.FindStringSubmatch(jsTrim(raw))
		if m != nil && strings.ToLower(m[1]) == strings.ToLower(name) {
			return m[2], true
		}
	}
	return "", false
}

// Expands %VAR% references case insensitively and leaves unknown references verbatim
func ExpandWindowsEnvRefs(value string, env map[string]string, order []string) string {
	if value == "" {
		return value
	}
	return envRef.ReplaceAllStringFunc(value, func(whole string) string {
		name := strings.ToUpper(whole[1 : len(whole)-1])
		for _, k := range order {
			if strings.ToUpper(k) == name {
				return env[k]
			}
		}
		return whole
	})
}

// Node process.platform name of the running OS
func HostPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// Runs a command and returns its standard output, failing on a non zero exit
type Exec func(name string, args []string) (string, error)

// Platform, environment and command runner a registry read uses
type Options struct {
	Platform string
	Env      map[string]string
	EnvOrder []string
	Exec     Exec
}

// Process environment as a map plus its key order
func ProcessEnv() (map[string]string, []string) {
	env := map[string]string{}
	var order []string
	for _, kv := range os.Environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if _, seen := env[k]; !seen {
			order = append(order, k)
		}
		env[k] = v
	}
	return env, order
}

// Runs reg.exe hidden with a five second timeout and decodes its output as UTF-8
func RunReg(name string, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return pycodecs.DecodeReplace(out, "utf-8")
}

// Reads a user variable from HKCU Environment on Windows, expanded, false when absent or empty
func ReadWindowsUserEnvVar(name string, o Options) (string, bool) {
	if o.Platform == "" {
		o.Platform = HostPlatform()
	}
	if o.Env == nil {
		o.Env, o.EnvOrder = ProcessEnv()
	}
	if o.Exec == nil {
		o.Exec = RunReg
	}
	if o.Platform != "win32" || name == "" {
		return "", false
	}
	stdout, err := o.Exec("reg", []string{"query", `HKCU\Environment`, "/v", name})
	if err != nil {
		return "", false
	}
	raw, ok := ParseRegQueryValue(stdout, name)
	if !ok {
		return "", false
	}
	expanded := jsTrim(ExpandWindowsEnvRefs(raw, o.Env, o.EnvOrder))
	return expanded, expanded != ""
}
