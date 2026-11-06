//go:build !windows

package pycodecs

import (
	"os"
	"os/exec"
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
