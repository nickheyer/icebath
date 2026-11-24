//go:build !icebath_core && !icebath_noextras

package pm

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// ForwardedUVSettings are the uv index and transport settings that survive into uv
var ForwardedUVSettings = map[string]bool{
	"UV_INDEX_URL": true, "UV_EXTRA_INDEX_URL": true, "UV_DEFAULT_INDEX": true, "UV_INDEX": true, "UV_NO_INDEX": true,
	"UV_FIND_LINKS": true, "UV_INDEX_STRATEGY": true, "UV_KEYRING_PROVIDER": true,
	"UV_NATIVE_TLS": true, "UV_INSECURE_HOST": true, "UV_HTTP_TIMEOUT": true,
}

var uvIndexKnobs = []string{"UV_INDEX_URL", "UV_DEFAULT_INDEX", "UV_INDEX"}

var pipToUV = [][2]string{
	{"PIP_EXTRA_INDEX_URL", "UV_EXTRA_INDEX_URL"},
	{"PIP_TRUSTED_HOST", "UV_INSECURE_HOST"},
}

// TimeoutHint tells a user whose uv run timed out how to reach a mirror
const TimeoutHint = "uv timed out. If your network needs a package mirror, set index-url in " +
	"pip.conf (bridged to uv automatically) or UV_INDEX_URL; raise UV_HTTP_TIMEOUT " +
	"for slow links."

// Reports whether an ambient uv setting crosses into uv, index credentials matching by prefix
func IsForwarded(key string) bool {
	return ForwardedUVSettings[key] || strings.HasPrefix(key, "UV_INDEX_")
}

// IndexRedirectSettings redirect where packages resolve from and are stripped by no config isolation
var IndexRedirectSettings = map[string]bool{
	"UV_INDEX_URL": true, "UV_EXTRA_INDEX_URL": true, "UV_DEFAULT_INDEX": true, "UV_INDEX": true, "UV_NO_INDEX": true,
	"UV_FIND_LINKS": true, "UV_INDEX_STRATEGY": true, "UV_KEYRING_PROVIDER": true,
}

// Reports whether a setting redirects package resolution rather than shaping transport
func IsIndexRedirect(key string) bool {
	return IndexRedirectSettings[key] || strings.HasPrefix(key, "UV_INDEX_")
}

// Null device path the way os.devnull names it
func devNull() string {
	if runtime.GOOS == "windows" {
		return "nul"
	}
	return "/dev/null"
}

// Pip config files lowest precedence first, sitePrefix naming the interpreter whose site file applies
func PipConfigCandidates(env map[string]string, sitePrefix string) []string {
	explicit := env["PIP_CONFIG_FILE"]
	if explicit == devNull() {
		return nil
	}
	home := userHome()
	var name string
	var globalFiles, userFiles []string
	switch runtime.GOOS {
	case "windows":
		name = "pip.ini"
		programData := env["ProgramData"]
		if programData == "" {
			programData = `C:\ProgramData`
		}
		appData := env["APPDATA"]
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		globalFiles = []string{filepath.Join(programData, "pip", name)}
		userFiles = []string{filepath.Join(home, "pip", name), filepath.Join(appData, "pip", name)}
	case "darwin":
		name = "pip.conf"
		globalFiles = []string{filepath.Join("/Library/Application Support/pip", name)}
		appSupport := filepath.Join(home, "Library", "Application Support", "pip")
		if st, err := os.Stat(appSupport); err != nil || !st.IsDir() {
			appSupport = filepath.Join(home, ".config", "pip")
		}
		userFiles = []string{filepath.Join(home, ".pip", name), filepath.Join(appSupport, name)}
	default:
		name = "pip.conf"
		xdgDirs := env["XDG_CONFIG_DIRS"]
		if xdgDirs == "" {
			xdgDirs = "/etc/xdg"
		}
		for _, d := range strings.Split(xdgDirs, string(os.PathListSeparator)) {
			if d != "" {
				globalFiles = append(globalFiles, filepath.Join(d, "pip", name))
			}
		}
		globalFiles = append(globalFiles, filepath.Join("/etc", name))
		xdgHome := env["XDG_CONFIG_HOME"]
		if xdgHome == "" {
			xdgHome = filepath.Join(home, ".config")
		}
		userFiles = []string{filepath.Join(home, ".pip", name), filepath.Join(xdgHome, "pip", name)}
	}
	var explicitFiles []string
	if explicit != "" {
		explicitFiles = []string{explicit}
		if st, err := os.Stat(explicit); err == nil && st.Mode().IsRegular() {
			userFiles = nil
		}
	}
	out := append(globalFiles, userFiles...)
	if sitePrefix != "" {
		out = append(out, filepath.Join(sitePrefix, name))
	}
	return append(out, explicitFiles...)
}

// Index URL from the global section of pip config files, a decode failure raising like Python
func PipConfIndexURL(env map[string]string, sitePrefix string) (string, error) {
	parser := newRawConfig()
	for _, path := range PipConfigCandidates(env, sitePrefix) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if err := strictUTF8(data); err != nil {
			return "", err
		}
		if err := parser.parse(path, string(data)); err != nil {
			return "", nil
		}
	}
	if !parser.hasSection("global") {
		return "", nil
	}
	return strings.TrimSpace(parser.get("global", "index-url")), nil
}

// rawConfig mirrors RawConfigParser, sections of lowercased options with a DEFAULT fallback
type rawConfig struct {
	sections map[string]map[string]string
	defaults map[string]string
}

func newRawConfig() *rawConfig {
	return &rawConfig{sections: map[string]map[string]string{}, defaults: map[string]string{}}
}

func (c *rawConfig) hasSection(name string) bool {
	_, ok := c.sections[name]
	return ok
}

// Option value of a section falling back to DEFAULT, empty when absent
func (c *rawConfig) get(section, option string) string {
	option = strings.ToLower(option)
	if v, ok := c.sections[section][option]; ok {
		return v
	}
	return c.defaults[option]
}

// One option set by the file being read, its value lines joined once the file ends
type rawOption struct {
	target map[string]string
	name   string
	lines  []string
}

// Reads one file the way RawConfigParser does, strict about duplicates within the file
func (c *rawConfig) parse(path, text string) error {
	var current map[string]string
	var currentName string
	var option *rawOption
	var options []*rawOption
	indentLevel := 0
	seenSections := map[string]bool{}
	seenOptions := map[string]bool{}
	defer func() {
		for _, o := range options {
			o.target[o.name] = strings.TrimRightFunc(strings.Join(o.lines, "\n"), pystr.IsSpace)
		}
	}()
	for i, raw := range strings.Split(universalNewlines(text), "\n") {
		lineno := i + 1
		stripped := strings.TrimSpace(raw)
		if stripped == "" || strings.HasPrefix(stripped, "#") || strings.HasPrefix(stripped, ";") {
			if stripped == "" && option != nil {
				option.lines = append(option.lines, "")
			}
			continue
		}
		indent := utf8.RuneCountInString(raw) - utf8.RuneCountInString(strings.TrimLeftFunc(raw, pystr.IsSpace))
		if option != nil && indent > indentLevel {
			option.lines = append(option.lines, stripped)
			continue
		}
		indentLevel = indent
		if end := strings.LastIndex(stripped, "]"); strings.HasPrefix(stripped, "[") && end > 1 {
			name := stripped[1:end]
			option = nil
			currentName = name
			if name == "DEFAULT" {
				current = c.defaults
				continue
			}
			if _, ok := c.sections[name]; !ok {
				c.sections[name] = map[string]string{}
			} else if seenSections[name] {
				return fmt.Errorf("%s line %d: section %s appears twice", path, lineno, name)
			}
			seenSections[name] = true
			current = c.sections[name]
			continue
		}
		if current == nil {
			return fmt.Errorf("%s line %d comes before any [section] header: %s", path, lineno, raw)
		}
		cut := strings.IndexAny(stripped, "=:")
		if cut < 0 {
			return fmt.Errorf("%s line %d has no = or : between a name and a value", path, lineno)
		}
		key := strings.ToLower(strings.TrimRightFunc(stripped[:cut], pystr.IsSpace))
		if key == "" {
			return fmt.Errorf("%s line %d has no name before its = or :", path, lineno)
		}
		if seenOptions[currentName+"\x00"+key] {
			return fmt.Errorf("%s line %d: option %s appears twice in section %s", path, lineno, key, currentName)
		}
		seenOptions[currentName+"\x00"+key] = true
		option = &rawOption{target: current, name: key, lines: []string{strings.TrimSpace(stripped[cut+1:])}}
		options = append(options, option)
	}
	return nil
}

// Uv index and transport settings the ambient environment asks for, pip knobs translated
func BridgedIndexSettings(ambient map[string]string, sitePrefix string) (map[string]string, error) {
	settings := map[string]string{}
	for k, v := range ambient {
		if IsForwarded(k) {
			settings[k] = v
		}
	}
	hasIndex := false
	for _, key := range uvIndexKnobs {
		if settings[key] != "" {
			hasIndex = true
		}
	}
	if !hasIndex {
		indexURL := strings.TrimSpace(ambient["PIP_INDEX_URL"])
		if indexURL == "" {
			fromConf, err := PipConfIndexURL(ambient, sitePrefix)
			if err != nil {
				return nil, err
			}
			indexURL = fromConf
		}
		if indexURL != "" {
			settings["UV_INDEX_URL"] = indexURL
		}
	}
	for _, pair := range pipToUV {
		value := strings.TrimSpace(ambient[pair[0]])
		if value != "" && settings[pair[1]] == "" {
			settings[pair[1]] = value
		}
	}
	return settings, nil
}
