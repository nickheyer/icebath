package pm

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/homeguard"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Home directory the way Path home resolves it, expanding a bare tilde
func userHome() string { return expandUser("~") }

// UnicodeDecodeError text of the first invalid UTF-8 sequence, nil for valid input
func strictUTF8(data []byte) error {
	for i := 0; i < len(data); {
		if data[i] < 0x80 {
			i++
			continue
		}
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		need := 0
		switch b := data[i]; {
		case b >= 0xC2 && b <= 0xDF:
			need = 2
		case b >= 0xE0 && b <= 0xEF:
			need = 3
		case b >= 0xF0 && b <= 0xF4:
			need = 4
		}
		if need == 0 {
			return fmt.Errorf("not valid UTF-8: byte 0x%02x at position %d cannot start a character", data[i], i)
		}
		valid := 1
		for valid < need && i+valid < len(data) && utf8ContinuationOK(data[i], valid, data[i+valid]) {
			valid++
		}
		if i+valid >= len(data) && valid < need {
			if valid == 1 {
				return fmt.Errorf("not valid UTF-8: byte 0x%02x at position %d begins a character the text ends inside", data[i], i)
			}
			return fmt.Errorf("not valid UTF-8: bytes at positions %d-%d begin a character the text ends inside", i, i+valid-1)
		}
		if valid == 1 {
			return fmt.Errorf("not valid UTF-8: byte 0x%02x at position %d begins a character that the next byte breaks", data[i], i)
		}
		return fmt.Errorf("not valid UTF-8: bytes at positions %d-%d begin a character that the next byte breaks", i, i+valid-1)
	}
	return nil
}

// Reports whether byte b may stand at offset of a sequence led by lead, including the lead specific ranges
func utf8ContinuationOK(lead byte, offset int, b byte) bool {
	lo, hi := byte(0x80), byte(0xBF)
	if offset == 1 {
		switch lead {
		case 0xE0:
			lo = 0xA0
		case 0xED:
			hi = 0x9F
		case 0xF0:
			lo = 0x90
		case 0xF4:
			hi = 0x8F
		}
	}
	return b >= lo && b <= hi
}

// NPMPublicRegistry is the registry lock.json records npm tarballs against
const NPMPublicRegistry = "https://registry.npmjs.org/"

// Registry npm resolves for this environment, npm_config_registry beating the user npmrc
func NPMRegistry(env map[string]string) string {
	lowered := map[string]string{}
	for k, v := range env {
		lowered[strings.ToLower(k)] = v
	}
	value := pystr.Strip(lowered["npm_config_registry"])
	if value == "" {
		npmrc := lowered["npm_config_userconfig"]
		if npmrc == "" {
			npmrc = filepath.Join(userHome(), ".npmrc")
		}
		npmrc = expandUser(npmrc)
		if data, err := os.ReadFile(npmrc); err == nil && utf8.Valid(data) {
			for _, line := range pystr.Splitlines(string(trimBOM(data)), false) {
				key, candidate, found := strings.Cut(line, "=")
				if found && strings.ToLower(pystr.Strip(key)) == "registry" {
					value = strings.Trim(pystr.Strip(candidate), `"'`)
				}
			}
		}
	}
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") {
		return strings.TrimRight(value, "/") + "/"
	}
	return NPMPublicRegistry
}

// Expands a leading tilde or tilde user the way os path expanduser does, unresolvable paths unchanged
func expandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	if runtime.GOOS == "windows" {
		return ntExpandUser(path)
	}
	i := strings.Index(path[1:], "/") + 1
	if i == 0 {
		i = len(path)
	}
	var home string
	if i == 1 {
		if v, ok := os.LookupEnv("HOME"); ok {
			home = v
		} else if u, err := homeguard.Current(); err == nil {
			home = u.HomeDir
		} else {
			return path
		}
	} else if u, err := homeguard.Lookup(path[1:i]); err == nil {
		home = u.HomeDir
	} else {
		return path
	}
	if expanded := strings.TrimRight(home, "/") + path[i:]; expanded != "" {
		return expanded
	}
	return "/"
}

// Windows tilde expansion from USERPROFILE or HOMEDRIVE and HOMEPATH, another user guessed beside it
func ntExpandUser(path string) string {
	i := 1
	for i < len(path) && path[i] != '\\' && path[i] != '/' {
		i++
	}
	home, ok := os.LookupEnv("USERPROFILE")
	if !ok {
		homePath, ok := os.LookupEnv("HOMEPATH")
		if !ok {
			return path
		}
		home = pypath.NtJoin(os.Getenv("HOMEDRIVE"), homePath)
	}
	if i != 1 {
		target := path[1:i]
		if current := os.Getenv("USERNAME"); target != current {
			if current != pypath.NtBasename(home) {
				return path
			}
			home = pypath.NtJoin(pypath.NtDirname(home), target)
		}
	}
	return home + path[i:]
}

// A public npm registry URL rebased onto the configured registry, other URLs unchanged
func NPMRegistryURL(rawURL string, env map[string]string) string {
	if !strings.HasPrefix(rawURL, NPMPublicRegistry) {
		return rawURL
	}
	return NPMRegistry(env) + rawURL[len(NPMPublicRegistry):]
}
