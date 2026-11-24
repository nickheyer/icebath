//go:build !windows

package charsetutil

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Canonical name of the process locale's character set: the codeset LC_ALL, LC_CTYPE or LANG names, the C library's codeset for a locale that names none, and UTF-8 for an unset, C or POSIX locale, on Android, or for a codeset this package does not know
func Locale() string {
	if runtime.GOOS == "android" {
		return "UTF-8"
	}
	name := ""
	for _, key := range [...]string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if name = os.Getenv(key); name != "" {
			break
		}
	}
	if name == "" || name == "C" || name == "POSIX" {
		return "UTF-8"
	}
	codeset, _, _ := strings.Cut(name, "@")
	if _, after, ok := strings.Cut(codeset, "."); ok {
		codeset = after
	} else {
		codeset = localeCharmap(name)
	}
	if cs, ok := find(codeset); ok {
		return cs.name
	}
	return "UTF-8"
}

var charmaps sync.Map

// Codeset the C library gives a locale name, asked of the locale tool once per name, empty when the tool cannot answer or answers ASCII, the codeset of the C locale an uninstalled locale falls back to
func localeCharmap(name string) string {
	if v, ok := charmaps.Load(name); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "locale", "charmap")
	cmd.Env = append(os.Environ(), "LC_ALL="+name)
	out, err := cmd.Output()
	codeset := strings.TrimSpace(string(out))
	if cs, ok := find(codeset); err != nil || ok && cs.name == "US-ASCII" {
		codeset = ""
	}
	charmaps.Store(name, codeset)
	return codeset
}

// Windows names for the system code pages, which name nothing on other systems
func platformLabel(string) (string, bool) { return "", false }
