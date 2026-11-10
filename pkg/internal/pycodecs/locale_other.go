//go:build !windows

package pycodecs

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// Only cp65001 resolves off Windows, mbcs and oem are Windows only modules
func platformCodePage(mod string) string {
	if mod == "cp65001" {
		return "utf_8"
	}
	return ""
}

var charmapCache sync.Map

// locale.getencoding after the PEP 538 coercion, the LC_CTYPE codeset or utf-8 for the C locale
func LocaleEncoding() string {
	name := ""
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(key); v != "" {
			name = v
			break
		}
	}
	if name == "" || name == "C" || name == "POSIX" {
		return "utf-8"
	}
	if v, ok := charmapCache.Load(name); ok {
		return v.(string)
	}
	codeset := localeCharmap(name)
	charmapCache.Store(name, codeset)
	return codeset
}

var codesetCache sync.Map

// C library codeset of a locale name through the locale tool, false when setlocale rejects the name
func localeCodeset(name string) (string, bool) {
	if v, ok := codesetCache.Load(name); ok {
		return v.(string), v.(string) != ""
	}
	cmd := exec.Command("locale", "charmap")
	cmd.Env = append(os.Environ(), "LC_ALL="+name)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	codeset := strings.TrimSpace(string(out))
	var exit *exec.ExitError
	switch {
	case err != nil && !errors.As(err, &exit):
		codeset = nameCodeset(name)
	case err != nil || stderr.Len() > 0:
		codeset = ""
	}
	codesetCache.Store(name, codeset)
	return codeset, codeset != ""
}

// Codeset a locale name spells when no locale tool can ask the C library, the C locale being ASCII
func nameCodeset(name string) string {
	if name == "C" || name == "POSIX" {
		return "ANSI_X3.4-1968"
	}
	if dot := strings.IndexByte(name, '.'); dot >= 0 {
		return strings.SplitN(name[dot+1:], "@", 2)[0]
	}
	return ""
}

// locale.getencoding, the LC_CTYPE codeset whatever the UTF-8 mode, a C locale coerced only while LC_ALL is unset
func GetEncoding() string {
	if runtime.GOOS == "android" {
		return "utf-8"
	}
	name := ""
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(key); v != "" {
			name = v
			break
		}
	}
	if name != "" && name != "C" && name != "POSIX" {
		if codeset, ok := localeCodeset(name); ok {
			return codeset
		}
	}
	if os.Getenv("LC_ALL") == "" {
		for _, target := range []string{"C.UTF-8", "C.utf8", "UTF-8"} {
			if codeset, ok := localeCodeset(target); ok {
				return codeset
			}
		}
	}
	codeset, _ := localeCodeset("C")
	return codeset
}

// nl_langinfo CODESET of a locale name through the locale tool, utf-8 when setlocale would fail
func localeCharmap(name string) string {
	cmd := exec.Command("locale", "charmap")
	cmd.Env = append(os.Environ(), "LC_ALL="+name)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	codeset := strings.TrimSpace(string(out))
	if err != nil || stderr.Len() > 0 || codeset == "" || codeset == "ANSI_X3.4-1968" {
		if err != nil && codeset == "" {
			if dot := strings.IndexByte(name, '.'); dot >= 0 {
				return strings.SplitN(name[dot+1:], "@", 2)[0]
			}
		}
		return "utf-8"
	}
	return codeset
}
