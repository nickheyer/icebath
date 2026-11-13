//go:build unix

package logging

import (
	"os"
	"strings"
)

// Reports whether the locale names a UTF-8 codeset, a C, POSIX or codeset-free locale counting as UTF-8
func stderrIsUTF8(*os.File) bool {
	locale := ""
	for _, name := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(name); v != "" {
			locale = v
			break
		}
	}
	_, codeset, ok := strings.Cut(locale, ".")
	if !ok {
		return true
	}
	codeset, _, _ = strings.Cut(codeset, "@")
	codeset = strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(codeset))
	return codeset == "utf8" || codeset == ""
}
