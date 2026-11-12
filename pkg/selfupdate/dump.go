package selfupdate

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
	"github.com/nickheyer/icebath/pkg/skills"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Seams of the dump the hermes tests patch, the project root, version identity, gateway snapshot and git readers
var (
	dumpProjectRoot     = config.GetProjectRoot
	dumpVersionInfo     = updaterecord.GetVersionInfo
	dumpCodeIdentity    = updaterecord.GetCodeIdentity
	dumpGatewaySnapshot = func(ctx context.Context) (gatewaycli.GatewayRuntimeSnapshot, error) {
		return gatewaycli.GetGatewayRuntimeSnapshot(ctx, false)
	}
	dumpGitCommitFn     = dumpGetGitCommit
	dumpGitCommitDateFn = dumpGetGitCommitDate
)

// Env var names assigned a non empty value in the home .env, the set the managed backends load
func dotenvKeyNames(h string) map[string]bool {
	names := map[string]bool{}
	raw, err := os.ReadFile(home.EnvPath(h))
	if err != nil {
		return names
	}
	for _, line := range pystr.Splitlines(pystr.DecodeUTF8SigIgnore(raw), false) {
		line = pystr.Strip(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		if strings.HasPrefix(pystr.Lower(line), "export ") {
			line = strings.TrimLeftFunc(line[len("export "):], unicode.IsSpace)
		}
		name, value, _ := strings.Cut(line, "=")
		if pystr.Strip(name) != "" && strings.Trim(pystr.Strip(value), "'\"") != "" {
			names[pystr.Strip(name)] = true
		}
	}
	return names
}

// Stripped stdout of git args in projectRoot, empty on any failure
func dumpGitOutput(projectRoot string, args ...string) string {
	res, err := pysubprocess.RunWith(context.Background(), append([]string{"git"}, args...), pysubprocess.Options{Dir: projectRoot, Timeout: 5 * time.Second})
	if err != nil || res.Returncode != 0 {
		return ""
	}
	return pystr.Strip(pystr.DecodeUTF8Replace(res.Stdout))
}

// Reports whether root resolves to the running install the version stamp describes
func dumpIsRunningInstall(root string) bool {
	resolved, err := pypath.Realpath(root, pypath.Lax)
	if err != nil {
		resolved = root
	}
	running, err := pypath.Realpath(dumpProjectRoot(), pypath.Lax)
	if err != nil {
		running = dumpProjectRoot()
	}
	return resolved == running
}

// Short git commit of projectRoot, the install stamp only for the running install, else (unknown)
func dumpGetGitCommit(projectRoot string) string {
	if value := dumpGitOutput(projectRoot, "rev-parse", "--short=8", "HEAD"); value != "" {
		return value
	}
	if !dumpIsRunningInstall(projectRoot) {
		return "(unknown)"
	}
	identity, err := dumpCodeIdentity(false)
	if err != nil || identity == nil || !pyrepr.Truthy(identity.Vals["short_sha"]) {
		return "(unknown)"
	}
	return pyrepr.Str(identity.Vals["short_sha"])
}

// Date the HEAD commit of projectRoot was authored as YYYY-MM-DD, empty when unknown
func dumpGetGitCommitDate(projectRoot string) string {
	if value := dumpGitOutput(projectRoot, "log", "-1", "--format=%cd", "--date=short", "HEAD"); value != "" {
		return value
	}
	if !dumpIsRunningInstall(projectRoot) {
		return ""
	}
	info, err := dumpVersionInfo()
	if err != nil || info.CommitDate == nil || *info.CommitDate == 0 {
		return ""
	}
	return time.Unix(*info.CommitDate, 0).UTC().Format("2006-01-02")
}

// Redacts all but the first and last four characters, empty for an empty value
func dumpRedact(value string) string { return tools.MaskDisplaySecret(value, 4, 4, 12) }

// Short gateway status, unknown when the snapshot fails on Linux or macOS and N/A elsewhere
func dumpGatewayStatus(ctx context.Context) string {
	snapshot, err := dumpGatewaySnapshot(ctx)
	if err != nil {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			return "unknown"
		}
		return "N/A"
	}
	if snapshot.Running() {
		mode := snapshot.Manager
		if snapshot.HasProcessServiceMismatch() {
			mode = "manual"
		}
		pid := ""
		if len(snapshot.GatewayPIDs) > 0 {
			pid = fmt.Sprintf(", pid %d", snapshot.GatewayPIDs[0])
		}
		return fmt.Sprintf("running (%s%s)", mode, pid)
	}
	return fmt.Sprintf("stopped (%s)", snapshot.Manager)
}

// Count of installed skills, every SKILL.md under the skills dir outside excluded paths
func dumpCountSkills(h string) int {
	skillsDir := filepath.Join(h, "skills")
	if info, err := os.Stat(skillsDir); err != nil || !info.IsDir() {
		return 0
	}
	count := 0
	filepath.WalkDir(skillsDir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Name() == "SKILL.md" && !skills.IsExcludedSkillPath(path, "") {
			count++
		}
		return nil
	})
	return count
}

// Cron jobs summary, active over total, 0 without a jobs file and (error reading) on a bad one
func dumpCronSummary(h string) string {
	jobsFile := filepath.Join(h, "cron", "jobs.json")
	if _, err := os.Stat(jobsFile); err != nil {
		return "0"
	}
	raw, err := os.ReadFile(jobsFile)
	if err != nil {
		return "(error reading)"
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return "(error reading)"
	}
	doc, err := pyjson.Loads(text)
	if err != nil {
		return "(error reading)"
	}
	obc, ok := pyjson.AsObject(doc)
	if !ok {
		return "(error reading)"
	}
	jobs := []any{}
	if raw, has := obc.Vals["jobs"]; has {
		if jobs, ok = raw.([]any); !ok {
			return "(error reading)"
		}
	}
	active := 0
	for _, job := range jobs {
		entry, ok := pyjson.AsObject(job)
		if !ok {
			return "(error reading)"
		}
		if enabled, has := entry.Vals["enabled"]; !has || pyrepr.Truthy(enabled) {
			active++
		}
	}
	return fmt.Sprintf("%d active / %d total", active, len(jobs))
}

// Platforms and the env var that marks each one configured, in display order
var dumpPlatformEnvVars = [][2]string{
	{"telegram", "TELEGRAM_BOT_TOKEN"}, {"discord", "DISCORD_BOT_TOKEN"}, {"slack", "SLACK_BOT_TOKEN"},
	{"whatsapp", "WHATSAPP_ENABLED"}, {"signal", "SIGNAL_HTTP_URL"}, {"email", "EMAIL_ADDRESS"},
	{"sms", "TWILIO_ACCOUNT_SID"}, {"matrix", "MATRIX_HOMESERVER_URL"}, {"mattermost", "MATTERMOST_URL"},
	{"dingtalk", "DINGTALK_CLIENT_ID"}, {"feishu", "FEISHU_APP_ID"},
	{"wecom", "WECOM_BOT_ID"}, {"wecom_callback", "WECOM_CALLBACK_CORP_ID"}, {"weixin", "WEIXIN_ACCOUNT_ID"},
	{"qqbot", "QQ_APP_ID"},
}

// First truthy value rendered as text, else the fallback
func dumpFirstTruthy(fallback string, values ...any) string {
	for _, v := range values {
		if pyrepr.Truthy(v) {
			return pyrepr.Str(v)
		}
	}
	return fallback
}

// Model and provider of the config, (not set) and (auto) when absent
func dumpModelAndProvider(cfg config.Tree) (string, string) {
	switch model := cfg["model"].(type) {
	case map[string]any:
		return dumpFirstTruthy("(not set)", model["default"], model["model"], model["name"]), dumpFirstTruthy("(auto)", model["provider"])
	case string:
		if model != "" {
			return model, "(auto)"
		}
	}
	return "(not set)", "(auto)"
}

// Sections and keys with interesting user facing overrides
var dumpInterestingPaths = [][2]string{
	{"agent", "max_turns"}, {"agent", "gateway_timeout"}, {"agent", "session_stall_timeout"},
	{"agent", "sanitizer_heal_escalation_threshold"}, {"agent", "tool_use_enforcement"},
	{"agent", "execution_guidance"}, {"terminal", "backend"}, {"terminal", "docker_image"},
	{"terminal", "persistent_shell"}, {"browser", "allow_private_urls"}, {"compression", "enabled"},
	{"compression", "threshold"}, {"compression", "in_place"}, {"display", "streaming"},
	{"display", "skin"}, {"display", "show_reasoning"}, {"privacy", "redact_pii"}, {"tts", "provider"},
}

// Mapping section of a tree, an empty mapping when absent and false for another shape
func dumpSection(tree map[string]any, name string) (map[string]any, bool) {
	raw, has := tree[name]
	if !has {
		return map[string]any{}, true
	}
	section, ok := raw.(map[string]any)
	return section, ok
}

// Non default config values worth reporting as section.key and text pairs in display order
func dumpConfigOverrides(cfg config.Tree) [][2]string {
	defaults := config.Defaults()
	var overrides [][2]string
	for _, p := range dumpInterestingPaths {
		defaultSection, ok1 := dumpSection(defaults, p[0])
		userSection, ok2 := dumpSection(cfg, p[0])
		if !ok1 || !ok2 {
			continue
		}
		userVal, has := userSection[p[1]]
		if !has || userVal == nil {
			continue
		}
		if !pyrepr.Equal(userVal, defaultSection[p[1]]) {
			overrides = append(overrides, [2]string{p[0] + "." + p[1], pyrepr.Str(userVal)})
		}
	}
	userToolsets, has := cfg["toolsets"]
	if !has {
		userToolsets = []any{}
	}
	defaultToolsets, has := defaults["toolsets"]
	if !has {
		defaultToolsets = []any{}
	}
	if !pyrepr.Equal(userToolsets, defaultToolsets) {
		overrides = append(overrides, [2]string{"toolsets", pyrepr.Str(userToolsets)})
	}
	if fallbacks := cfg["fallback_providers"]; pyrepr.Truthy(fallbacks) {
		overrides = append(overrides, [2]string{"fallback_providers", RedactDebugSupportText(pyrepr.Str(dumpFallbackDebugProjection(fallbacks)))})
	}
	return overrides
}

// Fallback entry fields useful to support
var dumpFallbackDebugFields = []string{"provider", "model", "base_url", "api_mode", "key_env", "api_key_env", "max_tokens"}

// Route metadata of fallback entries, an inline key shown only as a credential source status
func dumpFallbackDebugProjection(value any) []any {
	var entries []any
	switch v := value.(type) {
	case map[string]any:
		entries = []any{v}
	case []any:
		entries = v
	}
	projected := []any{}
	for _, entry := range entries {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		item := pyjson.NewObject()
		for _, key := range dumpFallbackDebugFields {
			child := m[key]
			if key == "max_tokens" {
				switch child.(type) {
				case int, int64, float64:
					item.Set(key, child)
				}
			} else if s, ok := child.(string); ok {
				item.Set(key, s)
			}
		}
		if pyrepr.Truthy(m["api_key"]) {
			item.Set("credential_source", "inline")
		}
		projected = append(projected, item)
	}
	return projected
}

// Env var and dump label of every API key in display order
var dumpAPIKeys = [][2]string{
	{"OPENROUTER_API_KEY", "openrouter"}, {"OPENAI_API_KEY", "openai"},
	{"ANTHROPIC_API_KEY", "anthropic"}, {"ANTHROPIC_TOKEN", "anthropic_token"},
	{"NOUS_API_KEY", "nous"}, {"GOOGLE_API_KEY", "google/gemini"}, {"GEMINI_API_KEY", "gemini"},
	{"GLM_API_KEY", "glm/zai"}, {"ZAI_API_KEY", "zai"}, {"KIMI_API_KEY", "kimi"},
	{"MINIMAX_API_KEY", "minimax"}, {"DEEPSEEK_API_KEY", "deepseek"},
	{"DASHSCOPE_API_KEY", "dashscope"}, {"HF_TOKEN", "huggingface"}, {"NVIDIA_API_KEY", "nvidia"},
	{"AI_GATEWAY_API_KEY", "ai_gateway"}, {"OPENCODE_ZEN_API_KEY", "opencode_zen"},
	{"OPENCODE_GO_API_KEY", "opencode_go"}, {"COMMANDCODE_API_KEY", "commandcode"},
	{"KILOCODE_API_KEY", "kilocode"}, {"FIRECRAWL_API_KEY", "firecrawl"}, {"TAVILY_API_KEY", "tavily"}, {"PERPLEXITY_API_KEY", "perplexity"},
	{"KEENABLE_API_KEY", "keenable"}, {"BROWSERBASE_API_KEY", "browserbase"}, {"FAL_KEY", "fal"},
	{"ELEVENLABS_API_KEY", "elevenlabs"}, {"GITHUB_TOKEN", "github"},
}

// Version line of the dump, version [commit] (date) from the running code identity
func dumpVersionLine(projectRoot string) string {
	version := "(unknown)"
	if info, err := dumpVersionInfo(); err == nil {
		version = info.DerivedVersion
	}
	line := fmt.Sprintf("%s [%s]", version, dumpGitCommitFn(projectRoot))
	if date := dumpGitCommitDateFn(projectRoot); date != "" {
		return fmt.Sprintf("%s (%s)", line, date)
	}
	return line
}

// Effective terminal backend, a TERMINAL_ENV in the environment overriding terminal.backend
func dumpEffectiveTerminalBackend(cfg config.Tree) string {
	configBackend := any("local")
	if terminal, ok := cfg["terminal"].(map[string]any); ok {
		if v, has := terminal["backend"]; has {
			configBackend = v
		}
	}
	envBackend := pystr.Lower(pystr.Strip(os.Getenv("TERMINAL_ENV")))
	if envBackend != "" && envBackend != pystr.Lower(pystr.Strip(pyrepr.Str(configBackend))) {
		return fmt.Sprintf("%s  (TERMINAL_ENV overrides config.yaml terminal.backend=%s)", envBackend, pyrepr.Str(configBackend))
	}
	return pyrepr.Str(configBackend)
}

// API key lines, set or not set per provider with the shell only flag and the auth pool note
func dumpAPIKeyLines(h string, showKeys bool) []string {
	dotenv := dotenvKeyNames(h)
	var lines []string
	for _, pair := range dumpAPIKeys {
		envVar, label := pair[0], pair[1]
		val := os.Getenv(envVar)
		display := "not set"
		if val != "" {
			display = "set"
			if showKeys {
				display = dumpRedact(val)
			}
		}
		if val != "" && !dotenv[envVar] {
			display += " (shell only — not in .env; managed/desktop backend may not see it)"
		}
		if val == "" && label == "openrouter" {
			if pool := provider.LoadPool(provider.HomePoolEnv(h), "openrouter"); pool != nil && pool.HasCredentials() {
				display = "set (auth pool)"
			}
		}
		lines = append(lines, fmt.Sprintf("  %-20s %s", label, display))
	}
	return lines
}

// Length of a mapping or list config value, zero for anything else
func dumpLen(v any) int {
	switch x := v.(type) {
	case map[string]any:
		return len(x)
	case []any:
		return len(x)
	}
	return 0
}

// Writes the compact copy pasteable setup summary of the dump command
func RunDump(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	showKeys := pyrepr.Truthy(ns["show_keys"])
	projectRoot := dumpProjectRoot()
	if _, err := provider.LoadIcebathDotenv(ctx, provider.DotenvLoadOptions{Home: h, ProjectEnv: filepath.Join(projectRoot, ".env")}); err != nil {
		return 1, err
	}
	cfg, err := config.LoadReadonly(ctx, h, nil)
	if err != nil {
		cfg = config.Tree{}
	}
	model, providerName := dumpModelAndProvider(cfg)
	profile := profiles.GetActiveProfileName(ctx)
	if profile == "" {
		profile = "(default)"
	}
	toolsets := []any{"icebath-cli"}
	if raw, has := cfg["toolsets"]; has {
		toolsets, _ = raw.([]any)
	}
	toolsetNames := make([]string, len(toolsets))
	for i, ts := range toolsets {
		toolsetNames[i] = pyrepr.Str(ts)
	}
	var platforms []string
	for _, pair := range dumpPlatformEnvVars {
		if os.Getenv(pair[1]) != "" {
			platforms = append(platforms, pair[0])
		}
	}
	mcp, _ := dumpSection(cfg, "mcp")
	memory, _ := dumpSection(cfg, "memory")
	lines := []string{
		"--- icebath dump ---",
		fmt.Sprintf("%-18s%s", "version:", dumpVersionLine(projectRoot)),
		fmt.Sprintf("%-18s%s %s %s", "os:", pyplatform.System(), pyplatform.Release(), pyplatform.Machine()),
		fmt.Sprintf("%-18s%s", "go:", hostinfo.GoVersion()),
		fmt.Sprintf("%-18s%s", "profile:", profile),
		fmt.Sprintf("%-18s%s", "icebath_home:", home.Display(h)),
		fmt.Sprintf("%-18s%s", "model:", model),
		fmt.Sprintf("%-18s%s", "provider:", providerName),
		fmt.Sprintf("%-18s%s", "terminal:", dumpEffectiveTerminalBackend(cfg)),
		"", "api_keys:",
	}
	lines = append(lines, dumpAPIKeyLines(h, showKeys)...)
	lines = append(lines, "", "features:",
		"  toolsets:           "+dumpJoinOr(toolsetNames, "(default)"),
		fmt.Sprintf("  mcp_servers:        %d", dumpLen(mcp["servers"])),
		"  memory_provider:    "+dumpFirstTruthy("built-in", memory["provider"]),
		"  gateway:            "+dumpGatewayStatus(ctx),
		"  platforms:          "+dumpJoinOr(platforms, "none"),
		"  cron_jobs:          "+dumpCronSummary(h),
		fmt.Sprintf("  skills:             %d", dumpCountSkills(h)),
	)
	if overrides := dumpConfigOverrides(cfg); len(overrides) > 0 {
		lines = append(lines, "", "config_overrides:")
		for _, kv := range overrides {
			lines = append(lines, fmt.Sprintf("  %s: %s", kv[0], kv[1]))
		}
	}
	lines = append(lines, "--- end dump ---")
	fmt.Fprintln(w, strings.Join(lines, "\n"))
	return 0, nil
}

// Comma joined items or the fallback when there are none
func dumpJoinOr(items []string, fallback string) string {
	if len(items) == 0 {
		return fallback
	}
	return strings.Join(items, ", ")
}
