package backend

import (
	"os"
	"path"
	"regexp"
	"strings"
)

// PathOps is the path flavor of a platform, posix or win32.
type PathOps struct {
	Sep   string
	Win   bool
	Cwd   func() string
	Delim string
}

// PathsFor returns the path flavor of an Electron platform name
func PathsFor(platform string) PathOps {
	if platform == "win32" {
		return PathOps{Sep: `\`, Win: true, Delim: ";", Cwd: func() string { c, _ := os.Getwd(); return c }}
	}
	return PathOps{Sep: "/", Delim: ":", Cwd: func() string { c, _ := os.Getwd(); return c }}
}

func (p PathOps) norm(s string) string {
	if p.Win {
		return strings.ReplaceAll(s, "/", `\`)
	}
	return s
}

func (p PathOps) volume(s string) string {
	if p.Win && len(s) >= 2 && s[1] == ':' {
		return s[:2]
	}
	return ""
}

// IsAbs reports an absolute path of the flavor
func (p PathOps) IsAbs(s string) bool {
	s = p.norm(s)
	if p.Win {
		v := p.volume(s)
		return (v != "" && len(s) > 2 && s[2] == '\\') || strings.HasPrefix(s, `\`)
	}
	return strings.HasPrefix(s, "/")
}

func (p PathOps) clean(s string) string {
	vol := p.volume(s)
	rest := strings.ReplaceAll(s[len(vol):], `\`, "/")
	c := path.Clean(rest)
	if p.Win {
		return vol + strings.ReplaceAll(c, "/", `\`)
	}
	return c
}

// Join joins segments and cleans the result
func (p PathOps) Join(parts ...string) string {
	var keep []string
	for _, x := range parts {
		if x != "" {
			keep = append(keep, p.norm(x))
		}
	}
	if len(keep) == 0 {
		return "."
	}
	return p.clean(strings.Join(keep, p.Sep))
}

// Resolve makes s absolute against the working directory and cleans it
func (p PathOps) Resolve(s string) string {
	s = p.norm(s)
	if !p.IsAbs(s) {
		s = p.Join(p.norm(p.Cwd()), s)
	}
	return p.clean(s)
}

// Dir returns the parent directory
func (p PathOps) Dir(s string) string {
	s = p.clean(p.norm(s))
	vol := p.volume(s)
	rest := strings.ReplaceAll(s[len(vol):], `\`, "/")
	d := path.Dir(rest)
	if p.Win {
		return vol + strings.ReplaceAll(d, "/", `\`)
	}
	return d
}

// Base returns the last element
func (p PathOps) Base(s string) string {
	s = p.clean(p.norm(s))
	return path.Base(strings.ReplaceAll(s[len(p.volume(s)):], `\`, "/"))
}

// Rel returns target relative to base, starting with .. when outside it
func (p PathOps) Rel(base, target string) string {
	b, t := p.Resolve(base), p.Resolve(target)
	fold := func(s string) string {
		if p.Win {
			return strings.ToLower(s)
		}
		return s
	}
	if p.Win && fold(p.volume(b)) != fold(p.volume(t)) {
		return t
	}
	split := func(s string) []string {
		s = strings.Trim(strings.ReplaceAll(s[len(p.volume(s)):], `\`, "/"), "/")
		if s == "" {
			return nil
		}
		return strings.Split(s, "/")
	}
	bs, ts := split(b), split(t)
	i := 0
	for i < len(bs) && i < len(ts) && fold(bs[i]) == fold(ts[i]) {
		i++
	}
	var out []string
	for range bs[i:] {
		out = append(out, "..")
	}
	out = append(out, ts[i:]...)
	return strings.Join(out, p.Sep)
}

// PosixSanePathEntries are appended to a POSIX backend PATH when missing.
var PosixSanePathEntries = []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// DelimiterForPlatform is the PATH list separator
func DelimiterForPlatform(platform string) string { return PathsFor(platform).Delim }

// PathEnvKey finds the PATH key in its platform casing
func PathEnvKey(env map[string]string, platform string) string {
	if platform != "win32" {
		return "PATH"
	}
	for k := range env {
		if strings.ToUpper(k) == "PATH" {
			return k
		}
	}
	return "PATH"
}

// AppendUniquePathEntries flattens entries split on delimiter, keeping the first occurrence of each
func AppendUniquePathEntries(entries [][]string, delimiter string) string {
	seen := map[string]bool{}
	var ordered []string
	for _, group := range entries {
		for _, part := range group {
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			ordered = append(ordered, part)
		}
	}
	return strings.Join(ordered, delimiter)
}

// SplitPathList splits a PATH value on delimiter
func SplitPathList(v, delimiter string) []string {
	if v == "" {
		return nil
	}
	return strings.Split(v, delimiter)
}

func expandHome(home string, p PathOps, homedir string) string {
	raw := home
	if raw == "~" || strings.HasPrefix(raw, "~/") || (p.Win && strings.HasPrefix(raw, `~\`)) {
		raw = p.Join(homedir, raw[1:])
	}
	return p.Resolve(raw)
}

func isProfileHome(resolved string, p PathOps) bool {
	return strings.ToLower(p.Base(p.Dir(resolved))) == "profiles"
}

// NormalizeHermesHomeRoot expands a leading tilde and maps a profile home back to the global root
func NormalizeHermesHomeRoot(home string, platform, homedir string) string {
	if home == "" {
		return home
	}
	p := PathsFor(platform)
	resolved := expandHome(home, p, homedir)
	if isProfileHome(resolved, p) {
		return p.Dir(p.Dir(resolved))
	}
	return resolved
}

var processEnvNames = map[string]bool{
	"APPDATA": true, "COMSPEC": true, "ICEBATH_HOME": true, "HOME": true, "LANG": true, "LC_ALL": true,
	"LOCALAPPDATA": true, "PATH": true, "PWD": true, "PYTHONPATH": true, "SHELL": true, "SSL_CERT_FILE": true,
	"SYSTEMROOT": true, "TEMP": true, "TMP": true, "TMPDIR": true, "TZ": true, "USER": true, "USERPROFILE": true,
	"VIRTUAL_ENV": true,
}

var dotenvKey = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// DotenvKeyNames lists the variable names a dotenv file assigns
func DotenvKeyNames(contents string) []string {
	contents = strings.TrimPrefix(contents, "\uFEFF")
	var out []string
	for _, line := range regexp.MustCompile(`\r?\n`).Split(contents, -1) {
		if m := dotenvKey.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// FileReader reads a file or returns empty text.
type FileReader func(name string) string

// ReadTextOrEmpty reads name from disk, empty when unreadable
func ReadTextOrEmpty(name string) string {
	b, err := os.ReadFile(name)
	if err != nil {
		return ""
	}
	return string(b)
}

// ProfileBackendParentEnv drops the secrets the launch profile dotenv loaded when spawning a different profile backend
func ProfileBackendParentEnv(hermesHome, profile string, current map[string]string, platform string, read FileReader) map[string]string {
	env := make(map[string]string, len(current))
	for k, v := range current {
		env[k] = v
	}
	if hermesHome == "" {
		return env
	}
	p := PathsFor(platform)
	fold := func(s string) string {
		if platform == "win32" {
			return strings.ToUpper(s)
		}
		return s
	}
	launchHome := hermesHome
	if inherited := current["ICEBATH_HOME"]; inherited != "" {
		if r := p.Resolve(inherited); isProfileHome(r, p) {
			launchHome = r
		}
	}
	name := profile
	if name == "" {
		name = strings.TrimSpace(read(p.Join(hermesHome, "active_profile")))
	}
	target := hermesHome
	if name != "" && name != "default" {
		target = p.Join(hermesHome, "profiles", name)
	}
	if fold(p.Resolve(launchHome)) == fold(p.Resolve(target)) {
		return env
	}
	owned := map[string]bool{}
	for _, file := range []string{".env", ".op.env"} {
		for _, key := range DotenvKeyNames(read(p.Join(launchHome, file))) {
			if !processEnvNames[strings.ToUpper(key)] {
				owned[fold(key)] = true
			}
		}
	}
	for k := range env {
		if owned[fold(k)] {
			delete(env, k)
		}
	}
	return env
}

// PlatformDefaultHermesHome is LOCALAPPDATA icebath on Windows and ~/.icebath elsewhere, plus the data suffix
func PlatformDefaultHermesHome(home string, env map[string]string, platform string) string {
	suffix := env["ICEBATH_DATA_DIR_SUFFIX"]
	if platform == "win32" {
		p := PathsFor(platform)
		base := strings.TrimSpace(env["LOCALAPPDATA"])
		if base == "" {
			base = p.Join(home, "AppData", "Local")
		}
		return p.Join(base, "icebath") + suffix
	}
	return path.Join(home, ".icebath") + suffix
}

// ResolveDesktopUserData honours ICEBATH_DESKTOP_USER_DATA_DIR, else the default path plus the data suffix
func ResolveDesktopUserData(defaultPath string, env map[string]string, platform string) string {
	if v := env["ICEBATH_DESKTOP_USER_DATA_DIR"]; v != "" {
		return PathsFor(platform).Resolve(v)
	}
	return defaultPath + env["ICEBATH_DATA_DIR_SUFFIX"]
}

// HomeInputs carries what desktop home resolution reads.
type HomeInputs struct {
	Home            string
	Env             map[string]string
	Platform        string
	DirectoryExists func(string) bool
	ReadWindowsHome func() string
}

// ResolveDesktopHermesHome picks ICEBATH_HOME, the user data home, the Windows registry home, a legacy dot dir or the default
func ResolveDesktopHermesHome(in HomeInputs) string {
	p := PathsFor(in.Platform)
	if v := in.Env["ICEBATH_HOME"]; v != "" {
		return NormalizeHermesHomeRoot(v, in.Platform, in.Home)
	}
	if v := in.Env["ICEBATH_DESKTOP_USER_DATA_DIR"]; v != "" {
		return p.Join(p.Resolve(v), "hermes-home")
	}
	if _, set := in.Env["ICEBATH_HOME"]; in.Platform == "win32" && !set {
		if reg := in.ReadWindowsHome(); reg != "" {
			return NormalizeHermesHomeRoot(reg, in.Platform, in.Home)
		}
	}
	def := PlatformDefaultHermesHome(in.Home, in.Env, in.Platform)
	if in.Platform == "win32" && in.Env["ICEBATH_DATA_DIR_SUFFIX"] == "" {
		legacy := p.Join(in.Home, ".icebath")
		if !in.DirectoryExists(def) && in.DirectoryExists(legacy) {
			return legacy
		}
	}
	return def
}

// StoreFirstPath moves the Icebath owned runtime and tools entries of a PATH to its front
func StoreFirstPath(pathValue string, current map[string]string, platform string, home HomeInputs) string {
	p := PathsFor(platform)
	delim := p.Delim
	hermesHome := ResolveDesktopHermesHome(home)
	var roots []string
	if v := current["ICEBATH_RUNTIME_DIR"]; v != "" {
		roots = append(roots, v)
	}
	roots = append(roots, p.Join(hermesHome, "tools"))
	owned := func(entry string) bool {
		for _, root := range roots {
			rel := p.Rel(p.Resolve(root), p.Resolve(entry))
			if !strings.HasPrefix(rel, "..") && !p.IsAbs(rel) {
				return true
			}
		}
		return false
	}
	entries := SplitPathList(pathValue, delim)
	var first []string
	for _, e := range entries {
		if e != "" && owned(e) {
			first = append(first, e)
		}
	}
	return AppendUniquePathEntries([][]string{first, entries}, delim)
}

// BuildDesktopBackendEnv scrubs Python paths, forces UTF-8 unless set, and builds a store first PATH with sane entries
func BuildDesktopBackendEnv(current map[string]string, platform string, home HomeInputs) map[string]string {
	delim := DelimiterForPlatform(platform)
	key := PathEnvKey(current, platform)
	var sane []string
	if platform != "win32" {
		sane = PosixSanePathEntries
	}
	inherited := StoreFirstPath(current[key], current, platform, home)
	utf8 := "1"
	if v, ok := current["PYTHONUTF8"]; ok {
		utf8 = v
	}
	return map[string]string{
		"PYTHONPATH": "",
		"PYTHONHOME": "",
		"PYTHONUTF8": utf8,
		key:          AppendUniquePathEntries([][]string{SplitPathList(inherited, delim), sane}, delim),
	}
}
