//go:build unix

package home

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Name of the default home directory below the account home
const defaultDirName = ".icebath"

// Variable a foreign POSIX environment names its account home with
const profileVar = "HOME"

// POSIX scratch directories take the secure directory policy
const securesScratch = true

// Native home of the invoking account when this process is root under sudo, false otherwise
func SudoInvokerDefaultHome() (string, bool) {
	if os.Geteuid() != 0 {
		return "", false
	}
	name := strings.TrimSpace(os.Getenv("SUDO_USER"))
	if name == "" || name == "root" {
		return "", false
	}
	h, ok := accountHome(name, -1)
	if !ok {
		return "", false
	}
	return filepath.Join(h, ".icebath"), true
}

// Home directory the account database records for this uid, false without a record
func accountDatabaseHome() (string, bool) { return accountHome("", os.Getuid()) }

// Variable references POSIX home paths expand, $NAME and ${NAME}
var posixReference = regexp.MustCompile(`\$([A-Za-z0-9_]+|\{[^}]*\})`)

// Platform default under the account home that userHome names, with the literal data directory suffix
func platformDefaultFrom(lookup func(string) string, userHome func() (string, error)) (string, error) {
	h, err := userHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(h, defaultDirName+lookup(envSuffix)), nil
}

// Account home from HOME when set, else the account database, false when neither answers
func userHome() (string, bool) {
	if h, ok := os.LookupEnv("HOME"); ok {
		return trimHome(h), true
	}
	h, ok := accountDatabaseHome()
	if !ok {
		return "", false
	}
	return trimHome(h), true
}

// Home text without trailing slashes, the filesystem root when nothing else is left
func trimHome(h string) string {
	if h = strings.TrimRight(h, "/"); h == "" {
		return "/"
	}
	return h
}

// Path with a leading tilde or tilde-user replaced by that account home, unchanged when the account is unknown
func ExpandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	i := strings.IndexByte(path, '/')
	if i < 0 {
		i = len(path)
	}
	var h string
	if i == 1 {
		found, ok := userHome()
		if !ok {
			return path
		}
		h = found
	} else {
		found, ok := accountHome(path[1:i], -1)
		if !ok {
			return path
		}
		h = trimHome(found)
	}
	if h == "/" {
		h = ""
	}
	if out := h + path[i:]; out != "" {
		return out
	}
	return "/"
}

// Path with each defined $NAME or ${NAME} replaced by its value, undefined references left as written
func expandReferences(path string) string {
	if !strings.Contains(path, "$") {
		return path
	}
	return posixReference.ReplaceAllStringFunc(path, func(m string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(m[1:], "{"), "}")
		if v, ok := os.LookupEnv(name); ok {
			return v
		}
		return m
	})
}

// Reports whether two cleaned paths name the same location on a case sensitive filesystem
func samePath(a, b string) bool { return a == b }

// Path as the home key compares it, unchanged on a case sensitive filesystem
func normCase(p string) string { return p }
