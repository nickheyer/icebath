package terminal

import (
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// Credentials and messaging settings never handed to a command unless passed through by name
var blocked = []string{
	"OPENAI_BASE_URL", "OPENAI_API_KEY", "OPENAI_API_BASE", "OPENAI_ORG_ID", "OPENAI_ORGANIZATION", "OPENROUTER_API_KEY", "ANTHROPIC_BASE_URL",
	"ANTHROPIC_API_KEY", "ANTHROPIC_TOKEN", "LLM_MODEL", "GOOGLE_API_KEY", "VERTEX_CREDENTIALS_PATH", "GOOGLE_APPLICATION_CREDENTIALS",
	"DEEPSEEK_API_KEY", "MISTRAL_API_KEY", "GROQ_API_KEY", "TOGETHER_API_KEY", "PERPLEXITY_API_KEY", "COHERE_API_KEY", "FIREWORKS_API_KEY",
	"XAI_API_KEY", "HELICONE_API_KEY", "PARALLEL_API_KEY", "FIRECRAWL_API_KEY", "FIRECRAWL_API_URL", "TELEGRAM_HOME_CHANNEL",
	"TELEGRAM_HOME_CHANNEL_NAME", "DISCORD_HOME_CHANNEL", "DISCORD_HOME_CHANNEL_NAME", "DISCORD_REQUIRE_MENTION", "DISCORD_FREE_RESPONSE_CHANNELS",
	"DISCORD_AUTO_THREAD", "SLACK_HOME_CHANNEL", "SLACK_HOME_CHANNEL_NAME", "SLACK_ALLOWED_USERS", "WHATSAPP_ENABLED", "WHATSAPP_MODE",
	"WHATSAPP_ALLOWED_USERS", "SIGNAL_HTTP_URL", "SIGNAL_ACCOUNT", "SIGNAL_ALLOWED_USERS", "SIGNAL_GROUP_ALLOWED_USERS", "SIGNAL_HOME_CHANNEL",
	"SIGNAL_HOME_CHANNEL_NAME", "SIGNAL_IGNORE_STORIES", "EMAIL_ADDRESS", "EMAIL_PASSWORD", "EMAIL_IMAP_HOST", "EMAIL_SMTP_HOST",
	"EMAIL_HOME_ADDRESS", "EMAIL_HOME_ADDRESS_NAME", "ICEBATH_DASHBOARD_SESSION_TOKEN", "GATEWAY_ALLOWED_USERS", "GH_TOKEN", "GITHUB_TOKEN",
	"GITHUB_APP_ID", "GITHUB_APP_PRIVATE_KEY_PATH", "GITHUB_APP_INSTALLATION_ID", "MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "DAYTONA_API_KEY",
	"GATEWAY_RELAY_ID", "GATEWAY_RELAY_SECRET", "GATEWAY_RELAY_DELIVERY_KEY", "VERCEL_OIDC_TOKEN", "VERCEL_TOKEN", "VERCEL_PROJECT_ID",
	"VERCEL_TEAM_ID", "QWEN_API_KEY", "ICEBATH_DASHBOARD_BASIC_AUTH_PASSWORD", "ICEBATH_DASHBOARD_BASIC_AUTH_SECRET",
	"ICEBATH_DASHBOARD_DRAIN_SECRET", "ICEBATH_DASHBOARD_OIDC_CLIENT_SECRET", "ICEBATH_MEET_REALTIME_KEY", "HASS_TOKEN", "HASS_URL",
	"ICEBATH_LANGFUSE_SECRET_KEY", "ICEBATH_LANGFUSE_PUBLIC_KEY", "ICEBATH_LANGFUSE_BASE_URL", "CAMOFOX_API_KEY", "CAMOFOX_URL",
	"COPILOT_GITHUB_TOKEN", "COPILOT_API_BASE_URL", "COPILOT_ACP_BASE_URL", "AWS_BEARER_TOKEN_BEDROCK", "MSGRAPH_CLIENT_SECRET",
	"MSGRAPH_WEBHOOK_CLIENT_STATE", "QQ_STT_API_KEY", "FEISHU_ENCRYPT_KEY", "FEISHU_VERIFICATION_TOKEN", "YUANBAO_APP_KEY",
	"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_SIGNING_SECRET", "GATEWAY_ALLOW_ALL_USERS",
	"SUDO_PASSWORD",
}

// Prefix that forces a credential through when a backend sets it on purpose
const forcePrefix = "_ICEBATH_FORCE_"

// Variables a supervisor hands the one process it started, never its children
var supervisorIdentity = []string{"ICEBATH_LAUNCHD_LABEL", "ICEBATH_GATEWAY_EXTERNAL_SUPERVISOR", "NOTIFY_SOCKET", "WATCHDOG_USEC", "WATCHDOG_PID"}

// Credentials named by a pattern no list can enumerate
func internalSecret(name string) bool {
	upper := strings.ToUpper(name)
	switch {
	case strings.HasPrefix(upper, "AUXILIARY_") && (strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_BASE_URL")):
		return true
	case strings.HasPrefix(upper, "GATEWAY_RELAY_") && (strings.HasSuffix(upper, "_SECRET") || strings.HasSuffix(upper, "_KEY") || strings.HasSuffix(upper, "_TOKEN")):
		return true
	}
	return false
}

// Names of variables that hold credentials for the profile: the fixed list, the variables of every secret setting, every backend's own
// credentials, and any name ending in _API_KEY or _AUTH_TOKEN
type secrets struct {
	names map[string]bool
}

func (t *Terminal) secrets(cfg *config.Config) secrets {
	names := map[string]bool{}
	for _, name := range blocked {
		names[name] = true
	}
	for _, section := range cfg.Sections() {
		for _, setting := range section.Settings {
			if setting.Secret {
				for _, variable := range setting.Env {
					names[strings.ToUpper(variable)] = true
				}
			}
		}
	}
	for _, backend := range t.Backends() {
		for _, variable := range backend.Secrets {
			names[strings.ToUpper(variable)] = true
		}
	}
	return secrets{names: names}
}

func (s secrets) has(name string) bool {
	upper := strings.ToUpper(name)
	return s.names[upper] || internalSecret(name) || strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_AUTH_TOKEN")
}

// Whether the name is a credential icebath itself manages, which no passthrough hands a command
func (s secrets) managed(name string) bool {
	return s.names[strings.ToUpper(name)] || internalSecret(name)
}

// Names the profile lets commands see although they look like credentials, credentials icebath itself manages refused
func (t *Terminal) Passthrough(cfg *config.Config) map[string]bool {
	known := t.secrets(cfg)
	out := map[string]bool{}
	for _, name := range cfg.GetStringSlice(terminalconfig.EnvPassthrough) {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		if known.managed(name) {
			slog.Warn("env passthrough: refusing to pass an icebath provider credential to commands; it stays hidden", "name", name)
			continue
		}
		out[name] = true
	}
	return out
}

// Values of variables skills need, those that are credentials icebath itself manages refused: a skill must never override the scrub of
// the keys icebath holds
func (t *Terminal) skillValues(cfg *config.Config, values map[string]string) map[string]string {
	known := t.secrets(cfg)
	out := map[string]string{}
	for name, value := range values {
		if known.managed(name) {
			slog.Warn("env passthrough: refusing a skill's variable that is an icebath provider credential; it stays hidden", "name", name)
			continue
		}
		out[name] = value
	}
	return out
}

// Variables of this process as a map, the first of a repeated name winning
func ProcessEnv() map[string]string {
	return config.Process()
}

// Environment of a command run on this machine for the profile: the profile's own, its .env and what its secret stores supply over this
// process's, without the credentials commands may not see, HOME by the home mode, temp files in the profile's scratch folder and a
// complete PATH, extra on top; a name in extra prefixed _ICEBATH_FORCE_ is set even when it is a credential
func (t *Terminal) ChildEnv(cfg *config.Config, extra map[string]string) map[string]string {
	return t.childEnv(cfg, cfg.Environ(), extra, nil)
}

// ChildEnv of a session's command that runs on this machine outside a backend's shell, such as an interpreter a tool starts: from the
// profile's own environment with the toolchain of the session's client in place of its own
func (t *Terminal) SessionEnv(cfg *config.Config, session string) map[string]string {
	return t.childEnv(cfg, t.Client(cfg, session).Toolchain(cfg.Environ()), nil, nil)
}

// ChildEnv from base, the variables of needed set and passed through as those the profile passes through are
func (t *Terminal) childEnv(cfg *config.Config, base, extra, needed map[string]string) map[string]string {
	known := t.secrets(cfg)
	passed := t.Passthrough(cfg)
	for name := range needed {
		passed[name] = true
	}
	out := map[string]string{}
	for name, value := range base {
		if !known.has(name) || passed[name] {
			out[name] = value
		}
	}
	for _, layer := range []map[string]string{needed, extra} {
		for name, value := range layer {
			if forced, ok := strings.CutPrefix(name, forcePrefix); ok {
				if !internalSecret(forced) {
					out[forced] = value
				}
				continue
			}
			if !known.has(name) || passed[name] {
				out[name] = value
			}
		}
	}
	for _, name := range supervisorIdentity {
		delete(out, name)
	}
	if key, ok := PathKey(out); ok {
		out[key] = completePath(cfg, out[key])
	}
	dir := cfg.ProfileDir(cfg.Profile())
	if real := RealHome(cfg, out); real != "" {
		out["ICEBATH_REAL_HOME"] = real
	}
	if home := SubprocessHome(cfg, out); home != "" {
		out["HOME"] = home
	}
	scratchTemp(dir, out)
	if runtime.GOOS == "windows" {
		for name, value := range map[string]string{"MSYS_NO_PATHCONV": "1", "MSYS2_ARG_CONV_EXCL": "*"} {
			if _, set := out[name]; !set {
				out[name] = value
			}
		}
	}
	return out
}

// Values a remote backend forwards into its sandbox from the profile's environment: the names the profile passes through, and those
// the backend's own settings name
func (t *Terminal) Forwarded(cfg *config.Config, names ...string) map[string]string {
	for name := range t.Passthrough(cfg) {
		names = append(names, name)
	}
	out := map[string]string{}
	for _, name := range names {
		if value, ok := cfg.LookupEnv(name); ok {
			out[name] = value
		}
	}
	return out
}

// Variables of this machine every profile shares, never unset in a sandbox
var machineWide = map[string]bool{"ICEBATH_HOME": true, "ICEBATH_PROFILE": true, "PATH": true, "HOME": true, "USER": true, "LANG": true, "LC_ALL": true,
	"TZ": true, "PWD": true, "SHELL": true, "TMPDIR": true, "VIRTUAL_ENV": true, "PYTHONPATH": true, "SSL_CERT_FILE": true}

// Variables a remote backend hands each command of its sandbox: the values here of the names given and of those the profile passes
// through, extra over them, and, sorted, the names among them with no value here, unset in the sandbox so a value set there before, by
// an earlier command or another profile's, never stands in; variables every profile shares, such as PATH, are never unset
func (t *Terminal) Forward(cfg *config.Config, extra map[string]string, names ...string) (map[string]string, []string) {
	values := t.Forwarded(cfg, names...)
	maps.Copy(values, extra)
	for name := range t.Passthrough(cfg) {
		names = append(names, name)
	}
	var unset []string
	for _, name := range names {
		if _, ok := values[name]; ok || !envName.MatchString(name) || machineWide[name] || strings.HasPrefix(name, "TERMINAL_") || slices.Contains(unset, name) {
			continue
		}
		unset = append(unset, name)
	}
	slices.Sort(unset)
	return values, unset
}

// Script that first unsets the variables, quietly
func Unset(script string, names []string) string {
	if len(names) == 0 {
		return script
	}
	return "unset " + Join(names) + " 2>/dev/null || true\n" + script
}

// Key of PATH in the environment, kept as Windows spells it, false when a Windows environment has none
func PathKey(env map[string]string) (string, bool) {
	if runtime.GOOS != "windows" {
		return "PATH", true
	}
	for name := range env {
		if strings.EqualFold(name, "PATH") {
			return name, true
		}
	}
	return "", false
}

// Standard entries for environments started with a minimal PATH
const sanePath = "/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// PATH with the standard folders, the profile's bin folder and ~/.local/bin appended when missing, and the folder of icebath itself first
func completePath(cfg *config.Config, path string) string {
	if runtime.GOOS != "windows" {
		var entries []string
		add := func(entry string) {
			if entry != "" && !slices.Contains(entries, entry) {
				entries = append(entries, entry)
			}
		}
		for _, entry := range strings.Split(path, ":") {
			add(entry)
		}
		for _, entry := range strings.Split(sanePath, ":") {
			add(entry)
		}
		for _, dir := range []string{filepath.Join(cfg.ProfileDir(cfg.Profile()), "bin"), userLocalBin()} {
			if st, err := os.Stat(dir); dir != "" && err == nil && st.IsDir() {
				add(dir)
			}
		}
		path = strings.Join(entries, ":")
	}
	return PrependMissing(path, ownBinDir())
}

func userLocalBin() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "bin")
}

// Folder icebath runs from, empty when it can't be found
func ownBinDir() string {
	candidate := ""
	if found, err := exec.LookPath("icebath"); err == nil {
		candidate = filepath.Dir(found)
	} else if exe, err := os.Executable(); err == nil {
		if base := strings.ToLower(filepath.Base(exe)); base == "icebath" || strings.HasPrefix(base, "icebath.") {
			candidate = filepath.Dir(exe)
		}
	}
	if st, err := os.Stat(candidate); candidate != "" && err == nil && st.IsDir() {
		return candidate
	}
	return ""
}

// PATH with the folders it lacks put first, those it has left where they are
func PrependMissing(path string, dirs ...string) string {
	separator := string(os.PathListSeparator)
	var entries []string
	for _, entry := range strings.Split(path, separator) {
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	var missing []string
	for _, dir := range dirs {
		if dir != "" && !slices.Contains(entries, dir) {
			missing = append(missing, dir)
		}
	}
	if len(missing) == 0 {
		return path
	}
	return strings.Join(append(missing, entries...), separator)
}

// Home folder inside the profile folder, when it was made
func profileHome(cfg *config.Config) string {
	home := filepath.Join(cfg.ProfileDir(cfg.Profile()), "home")
	if st, err := os.Stat(home); err == nil && st.IsDir() {
		return home
	}
	return ""
}

// Comparable form of a home path
func normHome(path string) string {
	if path = strings.TrimSpace(path); path == "" {
		return ""
	}
	if abs, err := filepath.Abs(ExpandUser(path)); err == nil {
		path = abs
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.ToLower(path)
	}
	return path
}

// The user's own home, never the profile's home folder
func RealHome(cfg *config.Config, env map[string]string) string {
	get := func(name string) string {
		if value := strings.TrimSpace(env[name]); value != "" {
			return value
		}
		return strings.TrimSpace(os.Getenv(name))
	}
	candidates := []string{get("ICEBATH_REAL_HOME"), get("HOME")}
	if runtime.GOOS != "windows" {
		if u, err := user.Current(); err == nil {
			candidates = append(candidates, strings.TrimSpace(u.HomeDir))
		}
	}
	candidates = append(candidates, get("USERPROFILE"))
	if drive, path := get("HOMEDRIVE"), get("HOMEPATH"); drive != "" && path != "" {
		candidates = append(candidates, drive+path)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, home)
	}
	profile := normHome(profileHome(cfg))
	for _, candidate := range candidates {
		if key := normHome(candidate); key != "" && key != profile {
			return candidate
		}
	}
	return os.TempDir()
}

// HOME a command gets under the home mode, empty when it keeps its own
func SubprocessHome(cfg *config.Config, env map[string]string) string {
	profile := profileHome(cfg)
	mode := strings.ToLower(strings.TrimSpace(cfg.GetString(terminalconfig.HomeMode)))
	switch mode {
	case "isolated", "profile_home", "profile-home":
		mode = terminalconfig.HomeProfile
	case "host", "user", "real_home", "real-home":
		mode = terminalconfig.HomeReal
	}
	if mode == terminalconfig.HomeProfile {
		return profile
	}
	real := RealHome(cfg, env)
	current := strings.TrimSpace(env["HOME"])
	repaired := ""
	if normHome(real) != normHome(current) {
		repaired = real
	}
	switch {
	case mode == terminalconfig.HomeReal:
		return repaired
	case profile != "" && InContainer():
		return profile
	case current == "" || normHome(current) == normHome(profile):
		return repaired
	}
	return ""
}

// Whether icebath itself runs in a container: a Docker or Podman marker file, a Kubernetes pod, a container runtime in the first process's
// cgroup or under the root mount
func InContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return true
	}
	if data, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		for _, marker := range []string{"docker", "podman", "/lxc/", "kubepods", "containerd", "crio"} {
			if strings.Contains(string(data), marker) {
				return true
			}
		}
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) >= 5 && fields[4] == "/" {
			for _, marker := range []string{"kubepods", "containerd", "crio"} {
				if strings.Contains(line, marker) {
					return true
				}
			}
		}
	}
	return false
}

// Variable that marks the temp variables as pointed at the scratch folder by icebath, not by the user
const scratchMarker = "ICEBATH_SCRATCH_DIR"

var tempVars = []string{"TMPDIR", "TMP", "TEMP"}

// Points the temp variables at the profile's scratch folder unless the user pointed them elsewhere
func scratchTemp(dir string, env map[string]string) {
	ours := env[scratchMarker]
	for _, name := range tempVars {
		if value := strings.TrimSpace(env[name]); value != "" && value != ours {
			return
		}
	}
	scratch := Scratch(dir)
	if scratch == "" {
		return
	}
	for _, name := range tempVars {
		env[name] = scratch
	}
	env[scratchMarker] = scratch
}

// Expands a leading ~ to the user's home
func ExpandUser(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return home + path[1:]
		}
	}
	return path
}

// Expands environment references and a leading ~
func ExpandPath(path string) string {
	return ExpandUser(os.ExpandEnv(path))
}
