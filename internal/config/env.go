package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/fsx"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Variables a profile reads beyond its .env and this process's own, such as those the secret stores of its settings supply: what its
// settings, credentials and commands see
type Environment interface {
	// Value of the variable for the profile of the settings, and whether it has one
	Lookup(c *Config, name string) (string, bool)
	// Every variable of the profile
	Environ(c *Config) map[string]string
	// Store that supplied the variable, as a person reads its name, empty for one of the .env or of this process
	Origin(c *Config, name string) string
}

// Name a variable can have
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Whether the name can name a variable
func EnvName(name string) bool {
	return envName.MatchString(name)
}

// Variables the .env writer refuses: those that steer what programs run and how, and icebath's own location and safety
var unwritable = map[string]bool{
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true, "LD_DEBUG": true, "DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true,
	"DYLD_FRAMEWORK_PATH": true, "DYLD_FALLBACK_LIBRARY_PATH": true, "DYLD_FALLBACK_FRAMEWORK_PATH": true, "PYTHONPATH": true, "PYTHONHOME": true,
	"PYTHONSTARTUP": true, "PYTHONUSERBASE": true, "PYTHONEXECUTABLE": true, "PYTHONNOUSERSITE": true, "PYTHONBREAKPOINT": true,
	"PYTHONCASEOK": true, "NODE_OPTIONS": true, "NODE_PATH": true, "PERL5OPT": true, "PERL5LIB": true, "PERLLIB": true, "RUBYOPT": true,
	"RUBYLIB": true, "CLASSPATH": true, "JAVA_TOOL_OPTIONS": true, "_JAVA_OPTIONS": true, "JDK_JAVA_OPTIONS": true, "GOFLAGS": true,
	"RUSTFLAGS": true, "PATH": true, "SHELL": true, "BROWSER": true, "EDITOR": true, "VISUAL": true, "PAGER": true, "MANPAGER": true,
	"GIT_SSH_COMMAND": true, "GIT_EXEC_PATH": true, "GIT_SHELL": true, "GIT_SSH": true, "GIT_ASKPASS": true, "SSH_ASKPASS": true,
	"SUDO_ASKPASS": true, "GIT_EDITOR": true, "GIT_SEQUENCE_EDITOR": true, "GIT_PAGER": true, "GIT_EXTERNAL_DIFF": true, "GIT_PROXY_COMMAND": true,
	"GIT_TEMPLATE_DIR": true, "GIT_DIR": true, "BASH_ENV": true, "ENV": true, "ZDOTDIR": true, "PROMPT_COMMAND": true, "VIMINIT": true,
	"EXINIT": true, "ICEBATH_HOME": true, "ICEBATH_PROFILE": true, "ICEBATH_CONFIG": true, "ICEBATH_ENV": true, "ICEBATH_CONFIG_PATH": true,
	"ICEBATH_ENV_PATH": true, "ICEBATH_OPTIONAL_MCPS": true, "ICEBATH_YOLO_MODE": true, "ICEBATH_ACCEPT_HOOKS": true,
	"ICEBATH_REDACT_SECRETS": true, "ICEBATH_INTERACTIVE": true, "ICEBATH_EXEC_ASK": true, "ICEBATH_GATEWAY_SESSION": true,
	"ICEBATH_CRON_SESSION": true, "ICEBATH_SINGLE_QUERY_SESSION": true, "ICEBATH_SESSION_KEY": true, "ICEBATH_SESSION_PLATFORM": true,
	"ICEBATH_RUNTIME_DIR": true, "ICEBATH_DISABLE_LAZY_INSTALLS": true,
}

// Families of names the .env writer refuses every member of
var unwritablePrefixes = []string{"LD_", "DYLD_", "GIT_CONFIG_"}

// Endings of names whose values go out as credentials, in HTTP headers among other places
var credentialEndings = []string{"_API_KEY", "_TOKEN", "_SECRET", "_KEY"}

// Whether the variable holds a credential, by its name
func Credential(name string) bool {
	return slices.ContainsFunc(credentialEndings, func(ending string) bool { return strings.HasSuffix(name, ending) })
}

// Reads the variables of every profile through the environment from now on
func (c *Config) UseEnvironment(environment Environment) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.environment = environment
}

func (c *Config) layer() Environment {
	if c.plain {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.environment
}

// Settings of the same profile whose variables come from its .env and this process alone: what an Environment reads its own settings
// through
func (c *Config) Plain() *Config {
	return &Config{core: c.core, profile: c.profile, plain: true}
}

// Value of the variable for the profile: what the administrator's .env sets, else what the stores of its settings supply, else its .env,
// else this process's environment, else the files that fill in what neither sets
func (c *Config) LookupEnv(name string) (string, bool) {
	if value, ok := c.loadedAdminEnv()[name]; ok {
		return value, true
	}
	if environment := c.layer(); environment != nil {
		return environment.Lookup(c, name)
	}
	if value, ok := c.Dotenv()[name]; ok {
		return value, true
	}
	over, fill := c.Layers()
	if value, ok := over[name]; ok {
		return value, true
	}
	if value, ok := os.LookupEnv(name); ok {
		return value, true
	}
	value, ok := fill[name]
	return value, ok
}

// Value of the variable for the profile, empty when it has none
func (c *Config) Getenv(name string) string {
	value, _ := c.LookupEnv(name)
	return value
}

// Value of the setting the variable sets, which reads the variable before its file, else of the variable alone when no setting names it:
// how features that share a credential, such as a key a web backend and a browser service both use, find it by the variable that holds it
func (c *Config) Variable(name string) string {
	for _, section := range c.Sections() {
		for _, setting := range section.Settings {
			if slices.Contains(setting.Env, name) {
				return c.GetString(setting.Key)
			}
		}
	}
	return c.Getenv(name)
}

// Every variable of the profile: this process's with the files that fill in what it leaves unset, its .env over them, what the stores of
// its settings supply over all of those, and what the administrator's .env sets over everything
func (c *Config) Environ() map[string]string {
	var out map[string]string
	if environment := c.layer(); environment != nil {
		out = environment.Environ(c)
	} else {
		over, fill := c.Layers()
		out = maps.Clone(fill)
		maps.Copy(out, Process())
		maps.Copy(out, over)
		maps.Copy(out, c.Dotenv())
	}
	maps.Copy(out, c.loadedAdminEnv())
	return out
}

// Store that supplied the variable for the profile: your administrator for one the administrator's .env sets, empty for its .env and this
// process's environment
func (c *Config) EnvOrigin(name string) string {
	if _, ok := c.loadedAdminEnv()[name]; ok {
		return "your administrator"
	}
	if environment := c.layer(); environment != nil {
		return environment.Origin(c, name)
	}
	return ""
}

// Variable naming a 1Password service account token, which, once the shell or the profile's .env sets it, leaves the profile's .op.env unread
const opToken = "OP_SERVICE_ACCOUNT_TOKEN"

// File of the profile folder whose variables fill in those neither the shell nor the .env set, such as a 1Password service account token
const opFile = ".op.env"

// .env beside the icebath program, the variables of an install that every profile reads
func ProjectEnvPath() string {
	exe, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return filepath.Join(wd, ".env")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Join(filepath.Dir(exe), ".env")
}

// Variables the profile has beside its .env and this process's, read once and again as its settings are: over, those of the .env beside
// the icebath program when the profile has no .env, which then stand over this process's as the profile's own would; fill, those that fill
// in what neither the shell nor the .env sets: the profile's .op.env, unless either already sets a 1Password service account token, then the
// .env beside the program
func (c *Config) Layers() (over, fill map[string]string) {
	name := c.Profile()
	c.envMu.Lock()
	defer c.envMu.Unlock()
	if read, ok := c.layers[name]; ok {
		return read.over, read.fill
	}
	path := c.EnvPath()
	project, err := ReadDotenv(ProjectEnvPath())
	if err != nil {
		slog.Warn("The .env beside icebath could not be read; its variables are left out", "path", ProjectEnvPath(), "error", err)
	}
	over, fill = map[string]string{}, map[string]string{}
	dotenv, _ := ReadDotenv(path)
	if os.Getenv(opToken) == "" && dotenv[opToken] == "" {
		op, err := ReadDotenv(filepath.Join(filepath.Dir(path), opFile))
		if err != nil {
			slog.Warn("The profile's .op.env could not be read; its variables are left out", "error", err)
		}
		maps.Copy(fill, op)
	}
	if fsx.Exists(path) {
		for key, value := range project {
			if _, ok := fill[key]; !ok {
				fill[key] = value
			}
		}
	} else {
		over = project
	}
	c.layers[name] = envLayers{over: over, fill: fill}
	return over, fill
}

// Variables beside a profile's .env as last read
type envLayers struct {
	over, fill map[string]string
}

// Variables of this process as a map, the first of a repeated name winning
func Process() map[string]string {
	out := map[string]string{}
	for _, entry := range os.Environ() {
		if name, value, ok := strings.Cut(entry, "="); ok && name != "" {
			if _, seen := out[name]; !seen {
				out[name] = value
			}
		}
	}
	return out
}

// .env of the profile, the variables only it reads, kept beside its config.yaml
func (c *Config) EnvPath() string {
	return filepath.Join(c.ProfileDir(c.Profile()), ".env")
}

// .env of a profile as last read, by the version of the file it was read from
type envFile struct {
	stamp  string
	values map[string]string
}

// Variables the profile's .env assigns, read again whenever the version of the file changes
func (c *Config) Dotenv() map[string]string {
	path := c.EnvPath()
	stamp := version(path)
	c.envMu.Lock()
	defer c.envMu.Unlock()
	if read, ok := c.dotenvs[path]; ok && read.stamp == stamp {
		return read.values
	}
	read, err := ReadDotenv(path)
	if err != nil && c.once("dotenv", path, stamp) {
		slog.Warn("The profile's .env could not be read; its variables are left out", "path", path, "error", err)
	}
	c.dotenvs[path] = envFile{stamp: stamp, values: read}
	return read
}

// Setting whose pin lets a profile start with a .env it may not read: the administrator decides where commands run, so the variables the
// profile keeps for its own backend are not needed
const pinnedBackend = "terminal.backend"

// Why the profile can't start on its .env, nil when it reads or is not there: a file that does not read stops it, unless the file is one it
// may not read and the administrator pins terminal.backend, which loads it as absent and says so once
func (c *Config) readableEnv() error {
	path := c.EnvPath()
	_, err := ReadDotenv(path)
	if err == nil {
		return nil
	}
	pin, _ := c.pinnedAt(pinnedBackend)
	if backend, _ := pin.(string); !errors.Is(err, fs.ErrPermission) || strings.TrimSpace(backend) == "" {
		return fmt.Errorf("The keys file %s can't be read: %v. %s", path, err, readFix(err))
	}
	// Told as the read of this version of the file would tell it, so it is said once
	if c.once("dotenv", path, version(path)) {
		slog.Warn(fmt.Sprintf("Cannot read %s; managed scope pins terminal.backend, so it loads as absent", path))
	}
	return nil
}

// Forgets the variables read for the profile, every profile's and the administrator's for no name, so they are read again
func (c *core) forgetDotenv(name string) {
	c.envMu.Lock()
	defer c.envMu.Unlock()
	if name == "" {
		clear(c.dotenvs)
		clear(c.layers)
		c.adminEnv = nil
		return
	}
	delete(c.dotenvs, filepath.Join(c.dir, profiles, name, ".env"))
	delete(c.layers, name)
}

// Variables a .env file assigns, none when there is no file. Lines are NAME=value, optionally after export; blank lines and comments are
// skipped; a value in double quotes takes \" \\ \n escapes, one in single quotes is kept as it is, an unquoted one ends at a # after a space
func ReadDotenv(path string) (map[string]string, error) {
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for scanner.Scan() {
		name, value, ok := assignment(scanner.Text())
		if ok {
			out[name] = value
		}
	}
	return out, scanner.Err()
}

// Name and value of one .env line, false for a line that assigns nothing
func assignment(line string) (string, string, bool) {
	line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	if rest, ok := strings.CutPrefix(line, "export "); ok {
		line = strings.TrimLeftFunc(rest, unicode.IsSpace)
	}
	name, raw, ok := strings.Cut(line, "=")
	name = strings.TrimSpace(name)
	if !ok || !EnvName(name) {
		return "", "", false
	}
	raw = strings.TrimSpace(raw)
	switch {
	case len(raw) >= 2 && raw[0] == '"':
		if end := closing(raw); end > 0 {
			return name, strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(raw[1:end]), true
		}
	case len(raw) >= 2 && raw[0] == '\'':
		if end := strings.IndexByte(raw[1:], '\''); end >= 0 {
			return name, raw[1 : end+1], true
		}
	}
	if at := strings.Index(raw, " #"); at >= 0 {
		raw = strings.TrimSpace(raw[:at])
	}
	return name, raw, true
}

// Index of the double quote that closes a value opened by one, skipping escaped ones, -1 when none does
func closing(raw string) int {
	for i := 1; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// Value as a .env line writes it, quoted when it holds a hash, a quote or any space
func quote(value string) string {
	if value == "" || !strings.ContainsAny(value, "#\"'") && strings.IndexFunc(value, unicode.IsSpace) < 0 {
		return value
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// Names of variables that start a program of icebath's, such as the command and arguments of a provider that runs a program of its own,
// which the .env writer refuses as it refuses PATH: what the provider host tells
func (c *Config) Launching(names func() []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.launching = names
}

// Why a variable can't be written to the .env at the path, nil when it can
func (c *Config) envWritable(name, path string) error {
	if !EnvName(name) {
		return fmt.Errorf("%q can't be the name of a variable. A name is letters, digits and underscores, and does not start with a digit.", name)
	}
	policy := func(name string) string {
		if runtime.GOOS == "windows" {
			return strings.ToUpper(name)
		}
		return name
	}
	c.mu.RLock()
	launching := c.launching
	c.mu.RUnlock()
	launches := launching != nil && slices.ContainsFunc(launching(), func(launch string) bool { return policy(launch) == policy(name) })
	if launches || unwritable[policy(name)] || slices.ContainsFunc(unwritablePrefixes, func(prefix string) bool { return strings.HasPrefix(policy(name), prefix) }) {
		return fmt.Errorf("icebath does not save %s for you, because the variable changes how programs start or where icebath keeps its files and "+
			"what it allows. To set it anyway, edit %s yourself.", name, display(path))
	}
	return nil
}

// Value without its characters outside ASCII, which break the API requests a key goes out in, and the warning of what was taken out, empty
// when nothing was: what copying a key out of a PDF or a web page leaves behind
func NonASCII(name, value string) (string, string) {
	var cleaned strings.Builder
	var bad []string
	for i, r := range []rune(value) {
		if r > unicode.MaxASCII {
			bad = append(bad, fmt.Sprintf("character %d: %s, U+%04X", i+1, strconv.Quote(string(r)), r))
			continue
		}
		cleaned.WriteRune(r)
	}
	if len(bad) == 0 {
		return value, ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "⚠ %s held characters that an API key can't have, and they were taken out. That happens when a key is copied from a PDF or a\n"+
		"  web page that swaps letters for ones that look the same.\n", name)
	for i, line := range bad[:min(5, len(bad))] {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("  " + line)
	}
	if len(bad) > 5 {
		b.WriteString("\n  and " + strs.Number(float64(len(bad)-5)) + " more")
	}
	b.WriteString("\n  If the key is refused, copy it again from the provider's website.")
	return cleaned.String(), b.String()
}

// Refusal of a write of the profile's .env the administrator's .env or a package manager decides
func envRefusal(locked *LockedError) error {
	return errors.New(locked.Refusal())
}

// Saves the variable in the profile's .env, replacing the line that assigned it, the file kept 0600 and written at once. A value loses
// its line breaks and its characters outside ASCII, which a request can't carry, telling what it took out
func (c *Config) SaveEnv(name, value string) error {
	path := c.EnvPath()
	if locked := c.envLock(name); locked != nil {
		return envRefusal(locked)
	}
	if err := c.envWritable(name, path); err != nil {
		return err
	}
	value, warning := NonASCII(name, strings.NewReplacer("\r", "", "\n", "").Replace(value))
	if warning != "" {
		slog.Warn(strings.TrimSpace(warning))
	}
	var lines []string
	if data, err := os.ReadFile(path); err == nil {
		lines = strings.SplitAfter(string(data), "\n")
		if last := len(lines) - 1; last >= 0 && lines[last] == "" {
			lines = lines[:last]
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	line := name + "=" + quote(value) + "\n"
	if at := slices.IndexFunc(lines, func(l string) bool { n, _, ok := assignment(l); return ok && n == name }); at >= 0 {
		lines[at] = line
	} else {
		if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
			lines[n-1] += "\n"
		}
		lines = append(lines, line)
	}
	if err := fsx.Write(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		return err
	}
	c.forgetDotenv(c.Profile())
	return nil
}

// Removes every line of the profile's .env that assigns the variable, the file kept 0600 and written at once; whether any did
func (c *Config) RemoveEnv(name string) (bool, error) {
	if locked := c.envLock(name); locked != nil {
		return false, envRefusal(locked)
	}
	path := c.EnvPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var kept []string
	removed := false
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if assigned, _, ok := assignment(line); ok && assigned == name {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	if err := fsx.Write(path, []byte(strings.Join(kept, "")), 0o600); err != nil {
		return false, err
	}
	c.forgetDotenv(c.Profile())
	return true, nil
}
