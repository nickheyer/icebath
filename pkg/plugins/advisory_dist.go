package plugins

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var distNormalizeRe = regexp.MustCompile(`[-_.]+`)

// Name importlib metadata keys a distribution lookup by
func distNormalized(name string) string {
	return strings.ReplaceAll(strings.ToLower(distNormalizeRe.ReplaceAllString(name, "-")), "-", "_")
}

// Name an older installer wrote into a dist-info directory, lowercased with dashes as underscores
func distLegacyNormalized(name string) string {
	return strings.ReplaceAll(strings.ToLower(name), "-", "_")
}

// Version header of the first distribution on a path matching a name, found false when none is installed
func findDistributionVersion(name string, path []string) (version string, found bool) {
	want, legacy := distNormalized(name), distLegacyNormalized(name)
	for _, entry := range path {
		if entry == "" {
			entry = "."
		}
		children, err := os.ReadDir(entry)
		if err != nil {
			continue
		}
		var eggs []string
		entryBase := strings.ToLower(filepath.Base(entry))
		for _, child := range children {
			low := strings.ToLower(child.Name())
			if ext := filepath.Ext(low); ext == ".dist-info" || ext == ".egg-info" {
				if base, _, _ := strings.Cut(strings.TrimSuffix(low, ext), "-"); distNormalized(base) == want {
					return distributionVersion(filepath.Join(entry, child.Name())), true
				}
			} else if strings.HasSuffix(entryBase, ".egg") && low == "egg-info" {
				if base, _, _ := strings.Cut(strings.TrimSuffix(entryBase, ".egg"), "-"); distLegacyNormalized(base) == legacy {
					eggs = append(eggs, filepath.Join(entry, child.Name()))
				}
			}
		}
		if len(eggs) > 0 {
			return distributionVersion(eggs[0]), true
		}
	}
	return "", false
}

// Version header of an installed distribution's METADATA or PKG-INFO, empty when it has none
func distributionVersion(dir string) string { return distributionHeader(dir, "version") }

// One header of an installed distribution's METADATA or PKG-INFO, matched case insensitively, empty when absent
func distributionHeader(dir, header string) string {
	text := ""
	for _, name := range []string{"METADATA", "PKG-INFO"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			text = string(data)
			break
		}
	}
	if text == "" {
		if data, err := os.ReadFile(dir); err == nil {
			text = string(data)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			break
		}
		if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(key), header) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
