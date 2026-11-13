package hostinfo

import (
	"os"
	"strings"
)

var localeConfigs = []string{"/etc/locale.conf", "/etc/default/locale"}

// System locale from the first LANG of the readable locale configs as a hyphenated tag without encoding, else empty
func hostLocale() string {
	for _, path := range localeConfigs {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimPrefix(string(data), "\xEF\xBB\xBF"), "\n") {
			key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
			if key == "LANG" && value != "" {
				tag, _, _ := strings.Cut(strings.Trim(value, `"'`), ".")
				return strings.ReplaceAll(tag, "_", "-")
			}
		}
	}
	return ""
}
