package pm

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
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

// Home directory the way Path home resolves it, expanding a bare tilde
func userHome() string { return expandUser("~") }

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
	return pystr.Strip(parser.get("global", "index-url")), nil
}

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
			return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid start byte", data[i], i)
		}
		valid := 1
		for valid < need && i+valid < len(data) && utf8ContinuationOK(data[i], valid, data[i+valid]) {
			valid++
		}
		if i+valid >= len(data) && valid < need {
			if valid == 1 {
				return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: unexpected end of data", data[i], i)
			}
			return fmt.Errorf("'utf-8' codec can't decode bytes in position %d-%d: unexpected end of data", i, i+valid-1)
		}
		if valid == 1 {
			return fmt.Errorf("'utf-8' codec can't decode byte 0x%02x in position %d: invalid continuation byte", data[i], i)
		}
		return fmt.Errorf("'utf-8' codec can't decode bytes in position %d-%d: invalid continuation byte", i, i+valid-1)
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
		stripped := pystr.Strip(raw)
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
				return fmt.Errorf("While reading from %s [line %d]: section %s already exists", pyrepr.StrRepr(path), lineno, pyrepr.StrRepr(name))
			}
			seenSections[name] = true
			current = c.sections[name]
			continue
		}
		if current == nil {
			return fmt.Errorf("File contains no section headers.\nfile: %s, line: %d\n%s", pyrepr.StrRepr(path), lineno, pyrepr.StrRepr(raw+"\n"))
		}
		cut := strings.IndexAny(stripped, "=:")
		if cut < 0 {
			return errors.New("Source contains parsing errors: " + pyrepr.StrRepr(path))
		}
		key := strings.ToLower(strings.TrimRightFunc(stripped[:cut], pystr.IsSpace))
		if key == "" {
			return errors.New("Source contains parsing errors: " + pyrepr.StrRepr(path))
		}
		if seenOptions[currentName+"\x00"+key] {
			return fmt.Errorf("While reading from %s [line %d]: option %s in section %s already exists", pyrepr.StrRepr(path), lineno, pyrepr.StrRepr(key), pyrepr.StrRepr(currentName))
		}
		seenOptions[currentName+"\x00"+key] = true
		option = &rawOption{target: current, name: key, lines: []string{pystr.Strip(stripped[cut+1:])}}
		options = append(options, option)
	}
	return nil
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
		} else if u, err := user.Current(); err == nil {
			home = u.HomeDir
		} else {
			return path
		}
	} else if u, err := user.Lookup(path[1:i]); err == nil {
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

// Reports a Windows path separator
func isNTSep(c byte) bool { return c == '\\' || c == '/' }

// A public npm registry URL rebased onto the configured registry, other URLs unchanged
func NPMRegistryURL(rawURL string, env map[string]string) string {
	if !strings.HasPrefix(rawURL, NPMPublicRegistry) {
		return rawURL
	}
	return NPMRegistry(env) + rawURL[len(NPMPublicRegistry):]
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
		indexURL := pystr.Strip(ambient["PIP_INDEX_URL"])
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
		value := pystr.Strip(ambient[pair[0]])
		if value != "" && settings[pair[1]] == "" {
			settings[pair[1]] = value
		}
	}
	return settings, nil
}
