package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

// doctorToolset is one unavailable toolset row of the tool availability report
type doctorToolset struct {
	Name        string
	MissingVars []string
	EnvVars     []string
	Tools       []string
}

// doctorWebRow is one web capability readiness row, status ok or warn
type doctorWebRow struct {
	Status, Label, Detail string
}

var (
	doctorWhich                 = func(cmd string) string { p, _ := pyshutil.Which(cmd, nil); return p }
	doctorIsTermux              = hostinfo.IsTermux
	doctorIsContainer           = hostinfo.IsContainer
	doctorSubprocessRun         = pysubprocess.RunWith
	doctorFindDocker            = func() string { return tools.FindDocker(nil) }
	doctorDescribeVercelAuth    = func() tools.VercelAuthStatus { return tools.DescribeVercelAuth(nil) }
	doctorActiveSearchProvider  = tools.ActiveSearchProvider
	doctorActiveExtractProvider = tools.ActiveExtractProvider
	doctorUsingLightpandaEngine = browser.UsingLightpandaEngine
	doctorLightpandaStatus      = browser.LightpandaEngineStatus
	doctorFindLightpandaBinary  = browser.FindLightpandaBinary
	doctorChromiumExecutable    = func(env *tools.Env) string { return browser.ChromiumExecutable(env, true) }
	doctorPMEnsure              = func(ctx context.Context, name string) error {
		_, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: true})
		return err
	}
)

// PATH lookup of a command that answers empty when the lookup itself fails
func safeWhich(cmd string) (found string) {
	defer func() {
		if recover() != nil {
			found = ""
		}
	}()
	return doctorWhich(cmd)
}

// Binary of the package PM currently selects, empty when nothing usable is installed
var doctorPMToolPath = func(ctx context.Context, name string) string {
	installed, err := pm.Installed(ctx, name, false)
	if err != nil || installed == nil {
		return ""
	}
	return installed.Binary
}

// PM package whose binary basename is the command, empty when no package provisions it
func doctorPMPackageForCommand(command string) string {
	target, err := pm.CurrentTarget()
	if err != nil {
		return ""
	}
	for _, name := range pm.AllPackages() {
		p, err := pm.GetPackage(name)
		if err != nil {
			return ""
		}
		b, ok := p.(interface{ Rel(string) string })
		if !ok {
			continue
		}
		rel := b.Rel(target)
		if rel == "" {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(filepath.FromSlash(rel)), ".exe"), ".cmd")
		if base == command {
			return name
		}
	}
	return ""
}

// Path of the tool Icebath would run, the PM store first then PATH, with the ok row detail
func doctorTool(ctx context.Context, name string) (string, string) {
	for _, packageName := range []string{name, doctorPMPackageForCommand(name)} {
		if packageName == "" {
			continue
		}
		if staged := doctorPMToolPath(ctx, packageName); staged != "" {
			return staged, "(pm store)"
		}
	}
	return safeWhich(name), ""
}

// True when the command exits zero within the timeout, a timeout counting as failure
var doctorRunOK = func(ctx context.Context, cmd []string, timeout time.Duration) (bool, error) {
	done, err := doctorSubprocessRun(ctx, cmd, pysubprocess.Options{Timeout: timeout})
	if err != nil {
		var expired *pysubprocess.TimeoutExpired
		if errors.As(err, &expired) {
			return false, nil
		}
		return false, err
	}
	return done.Returncode == 0, nil
}

var doctorTermuxInstallAllFallbackNotes = []string{
	"Termux uses the Icebath APT package: pkg install icebath-agent.",
	"Matrix E2EE extra is excluded on Termux (python-olm currently fails to build).",
	"Local faster-whisper extra is excluded on Termux (ctranslate2/av build path unavailable).",
	"STT fallback: use Groq Whisper (set GROQ_API_KEY) or OpenAI Whisper (set VOICE_TOOLS_OPENAI_KEY).",
}

// True when Kanban is unavailable only because this process is not a dispatcher spawned worker
func doctorIsKanbanWorkerEnvGate(item doctorToolset) bool {
	if item.Name != "kanban" || os.Getenv("ICEBATH_KANBAN_TASK") != "" || len(item.Tools) == 0 {
		return false
	}
	for _, tool := range item.Tools {
		if !strings.HasPrefix(tool, "kanban_") {
			return false
		}
	}
	return true
}

// Explanatory suffix for a toolset whose doctor status needs context, empty for the rest
func doctorToolAvailabilityDetail(toolset string) string {
	if toolset == "kanban" && os.Getenv("ICEBATH_KANBAN_TASK") == "" {
		return "(runtime-gated; loaded only for dispatcher-spawned workers)"
	}
	return ""
}

// Tool environment of a doctor run over the home config, a failed config read leaving the config empty
func doctorToolsEnv(ctx context.Context, d *Doctor) *tools.Env {
	cfg, err := config.LoadReadonly(ctx, d.Home, nil)
	if err != nil {
		return &tools.Env{Home: d.Home}
	}
	return &tools.Env{Home: d.Home, Config: cfg}
}

// Active provider of one web capability, nil when the resolver fails
func doctorWebProvider(getter func(*tools.Env) tools.WebProvider, env *tools.Env) (p tools.WebProvider) {
	defer func() {
		if recover() != nil {
			p = nil
		}
	}()
	return getter(env)
}

// Readiness rows for web search and web extract, empty when the web plugins cannot be loaded
var doctorWebCapabilityRows = func(ctx context.Context, d *Doctor) (rows []doctorWebRow) {
	env := doctorToolsEnv(ctx, d)
	loaded := func() (ok bool) {
		defer func() { ok = recover() == nil }()
		tools.EnsureWebPluginsLoaded(ctx, env)
		return true
	}()
	if !loaded {
		return rows
	}
	for _, capability := range []struct {
		label  string
		getter func(*tools.Env) tools.WebProvider
	}{{"web search", doctorActiveSearchProvider}, {"web extract", doctorActiveExtractProvider}} {
		provider := doctorWebProvider(capability.getter, env)
		if provider == nil {
			rows = append(rows, doctorWebRow{"warn", capability.label, "(no provider selected or registered)"})
			continue
		}
		name := provider.Name()
		if name == "" {
			t := reflect.TypeOf(provider)
			for t.Kind() == reflect.Pointer {
				t = t.Elem()
			}
			name = t.Name()
		}
		if tools.WebProviderIsReady(env, provider) {
			rows = append(rows, doctorWebRow{"ok", capability.label, "(" + name + ")"})
		} else {
			rows = append(rows, doctorWebRow{"warn", capability.label, "(" + name + " selected; provider not configured)"})
		}
	}
	return rows
}

// Moves a Kanban row gated only by the worker environment from unavailable to available
var doctorApplyToolAvailabilityOverrides = func(available []string, unavailable []doctorToolset) ([]string, []doctorToolset) {
	updatedAvailable, updatedUnavailable := append([]string{}, available...), []doctorToolset{}
	for _, item := range unavailable {
		if !doctorIsKanbanWorkerEnvGate(item) {
			updatedUnavailable = append(updatedUnavailable, item)
		} else if !containsString(updatedAvailable, "kanban") {
			updatedAvailable = append(updatedAvailable, "kanban")
		}
	}
	return updatedAvailable, updatedUnavailable
}

// Reports whether a string list holds a value
func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// Toolsets enabled for the CLI, the second result false when config resolution fails
var doctorEnabledCLIToolsets = func(ctx context.Context, d *Doctor) (enabled map[string]bool, ok bool) {
	defer func() {
		if recover() != nil {
			enabled, ok = nil, false
		}
	}()
	cfg, err := config.Load(d.Home, nil)
	if err != nil {
		return nil, false
	}
	return tools.Default.PlatformTools(ctx, tools.ScopeKey(d.Home), cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true}), true
}

var doctorToolsetSetupHints = map[string]string{
	"image_gen": "(image generation unavailable — check the provider selection and its key or SDK with 'icebath tools')",
}

// True when a toolset is unavailable for want of a key or a named setup step
func doctorSetupGated(item doctorToolset) bool {
	_, hinted := doctorToolsetSetupHints[item.Name]
	return len(item.MissingVars) > 0 || len(item.EnvVars) > 0 || hinted
}

// Unavailable setup gated toolsets narrowed to those enabled for the CLI
func doctorMissingAPIKeyToolsetsForSummary(ctx context.Context, d *Doctor, unavailable []doctorToolset) []doctorToolset {
	gated := []doctorToolset{}
	for _, item := range unavailable {
		if doctorSetupGated(item) {
			gated = append(gated, item)
		}
	}
	enabled, ok := doctorEnabledCLIToolsets(ctx, d)
	if !ok {
		return gated
	}
	out := []doctorToolset{}
	for _, item := range gated {
		if enabled[item.Name] {
			out = append(out, item)
		}
	}
	return out
}

// Reports git and ripgrep from the PM store or PATH with the install hint for a missing ripgrep
func checkGitAndRg(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		git, gitDetail := doctorTool(ctx, "git")
		rg, rgDetail := doctorTool(ctx, "rg")
		doctorCheckBool(d.W, git != "", doctorRow{"git", gitDetail}, doctorRow{"git not found", "(optional)"}, false)
		if !doctorCheckBool(d.W, rg != "", doctorRow{"ripgrep (rg)", pystr.Strip(rgDetail + " (faster file search)")},
			doctorRow{"ripgrep (rg) not found", "(file search uses grep fallback)"}, false) {
			doctorCheckInfo(d.W, "Install for faster search: "+doctorSystemPackageInstallCmd("ripgrep"))
		}
		return nil
	})
}

var doctorBuiltinTerminalBackends = map[string]bool{"local": true, "docker": true, "modal": true, "managed_modal": true, "daytona": true, "vercel_sandbox": true, "ssh": true}

// Prints the ok row when cond holds, else the fail row and records the issue
var doctorRequire = func(w io.Writer, cond bool, ok, bad doctorRow, issue string, issues *[]string) {
	if !doctorCheckBool(w, cond, ok, bad, true) {
		*issues = append(*issues, issue)
	}
}

// Upper cases the first character of a hint
func doctorCapitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[:1])) + string(r[1:])
}

// Reports the Docker or Podman runtime, required and probed when it is the selected backend
func doctorCheckDockerBackend(ctx context.Context, w io.Writer, terminalEnv string, runningInContainer bool, issues *[]string) error {
	dockerExe := doctorFindDocker()
	switch {
	case terminalEnv == "docker":
		if dockerExe == "" {
			doctorFailAndIssue(w, "Docker or Podman not installed", "(needed for the 'docker' terminal backend)",
				"Install Docker or Podman, or run `icebath setup terminal` to switch backend.", issues)
			return nil
		}
		runtimeName := tools.DockerRuntimeName(dockerExe)
		hint := tools.DockerRuntimeStartHint(dockerExe)
		unreachable, okDetail := runtimeName+" not reachable", "(reachable)"
		if runtimeName == "Docker" {
			unreachable, okDetail = runtimeName+" daemon not running", "(daemon running)"
		}
		reachable, err := doctorRunOK(ctx, []string{dockerExe, "version"}, 10*time.Second)
		if err != nil {
			return err
		}
		doctorRequire(w, reachable, doctorRow{runtimeName, okDetail},
			doctorRow{unreachable, "(needed for the 'docker' terminal backend)"},
			doctorCapitalize(hint)+", or run `icebath setup terminal` to switch backend.", issues)
	case dockerExe != "":
		doctorCheckOK(w, tools.DockerRuntimeName(dockerExe), "(optional)")
	case doctorIsTermux():
		doctorCheckInfo(w, "Docker backend is not available inside Termux (expected on Android)")
	case !runningInContainer:
		doctorCheckWarn(w, "Docker/Podman not found", "(optional)")
	}
	return nil
}

// Probes the configured SSH host with a batch mode connection
func doctorCheckSSHBackend(ctx context.Context, d *Doctor, issues *[]string) error {
	sshHost := os.Getenv("TERMINAL_SSH_HOST")
	if sshHost == "" {
		doctorFailAndIssue(d.W, "SSH host not configured", "(needed for the 'ssh' terminal backend)",
			"run `icebath setup terminal` and enter the SSH host and user.", issues)
		return nil
	}
	sshUser, sshPort, sshKey := os.Getenv("TERMINAL_SSH_USER"), os.Getenv("TERMINAL_SSH_PORT"), os.Getenv("TERMINAL_SSH_KEY")
	cmd := []string{"ssh", "-o", "ConnectTimeout=5", "-o", "BatchMode=yes"}
	if sshPort != "" {
		cmd = append(cmd, "-p", sshPort)
	}
	if sshKey != "" {
		cmd = append(cmd, "-i", pypath.Expanduser(sshKey))
	}
	target := sshHost
	if sshUser != "" {
		target = sshUser + "@" + sshHost
	}
	cmd = append(cmd, target, "echo ok")
	connected, err := doctorRunOK(ctx, cmd, 15*time.Second)
	if err != nil {
		return err
	}
	doctorRequire(d.W, connected, doctorRow{Text: "SSH connection to " + sshHost}, doctorRow{Text: "SSH connection to " + sshHost},
		"Check SSH configuration for "+sshHost, issues)
	return nil
}

// Reports whether the client of a built in terminal backend is linked into this binary
func doctorBackendClientLinked(ctx context.Context, name string) bool {
	return tools.GetTerminalBackendScoped(ctx, name, "") != nil
}

// Reports the Daytona API key and the linked Daytona client
func doctorCheckDaytonaBackend(ctx context.Context, d *Doctor, issues *[]string) error {
	doctorRequire(d.W, os.Getenv("DAYTONA_API_KEY") != "", doctorRow{"Daytona API key", "(configured)"},
		doctorRow{"Daytona API key missing", "(needed for the 'daytona' terminal backend)"},
		"run `icebath setup terminal` (Daytona) to enter it.", issues)
	if doctorBackendClientLinked(ctx, "daytona") {
		doctorCheckOK(d.W, "daytona SDK", "(installed)")
	} else {
		doctorFailAndIssue(d.W, "daytona SDK not installed", "(run icebath setup terminal)",
			"Run icebath setup terminal and select Daytona, then restart Icebath", issues)
	}
	return nil
}

// Environment value with a default for an unset name
func doctorGetenvDefault(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Reports the Vercel runtime, disk setting, linked client, auth mode and persistence
func doctorCheckVercelBackend(ctx context.Context, d *Doctor, issues *[]string) error {
	runtimeName := pystr.Strip(doctorGetenvDefault("TERMINAL_VERCEL_RUNTIME", "node24"))
	if runtimeName == "" {
		runtimeName = "node24"
	}
	supported := strings.Join(tools.SupportedVercelRuntimes, ", ")
	doctorRequire(d.W, containsString(tools.SupportedVercelRuntimes, runtimeName), doctorRow{"Vercel runtime", "(" + runtimeName + ")"},
		doctorRow{"Vercel runtime unsupported", "(" + runtimeName + "; use " + supported + ")"},
		"Set TERMINAL_VERCEL_RUNTIME to one of: "+supported, issues)
	disk := pystr.Strip(doctorGetenvDefault("TERMINAL_CONTAINER_DISK", "51200"))
	doctorRequire(d.W, disk == "" || disk == "0" || disk == "51200",
		doctorRow{"Vercel disk setting", "(uses platform default)"},
		doctorRow{"Vercel custom disk unsupported", "(reset terminal.container_disk to 51200)"},
		"Vercel Sandbox does not support custom container_disk; use the shared default 51200", issues)
	doctorRequire(d.W, doctorBackendClientLinked(ctx, "vercel_sandbox"), doctorRow{"vercel SDK", "(installed)"},
		doctorRow{"vercel SDK not installed", "(run icebath setup terminal)"},
		"Run icebath setup terminal and select Vercel Sandbox, then restart Icebath", issues)
	auth := doctorDescribeVercelAuth()
	switch {
	case auth.OK:
		doctorCheckOK(d.W, "Vercel auth", "("+auth.Label+")")
	case strings.HasPrefix(auth.Label, "partial"):
		doctorFailAndIssue(d.W, "Vercel auth incomplete", "("+auth.Label+")", "Set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID together", issues)
	default:
		doctorFailAndIssue(d.W, "Vercel auth not configured", "("+auth.Label+")",
			"Configure Vercel Sandbox auth with VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID", issues)
	}
	for _, line := range auth.DetailLines {
		doctorCheckInfo(d.W, "Vercel auth "+line)
	}
	switch strings.ToLower(doctorGetenvDefault("TERMINAL_CONTAINER_PERSISTENT", "true")) {
	case "1", "true", "yes", "on":
		doctorCheckInfo(d.W, "Vercel persistence: snapshot filesystem only; live processes do not survive sandbox recreation")
	default:
		doctorCheckInfo(d.W, "Vercel persistence: ephemeral filesystem")
	}
	return nil
}

// Plugin terminal backend of a name after plugin discovery, nil when discovery or the lookup fails
var doctorPluginTerminalBackend = func(ctx context.Context, d *Doctor, name string) (p tools.BackendProvider) {
	defer func() {
		if recover() != nil {
			p = nil
		}
	}()
	plugins.DiscoverPlugins(ctx, false)
	return tools.PluginTerminalBackend(ctx, name, tools.ScopeKey(d.Home))
}

// Runs the doctor rows a plugin terminal backend declares, failing an unknown backend name
func doctorCheckPluginBackend(ctx context.Context, d *Doctor, terminalEnv string, issues *[]string) {
	provider := doctorPluginTerminalBackend(ctx, d, terminalEnv)
	if provider == nil {
		doctorFailAndIssue(d.W, "Unknown terminal backend '"+terminalEnv+"'", "(no built-in or plugin backend by that name)",
			"Fix terminal.backend in config.yaml, or install/enable the plugin that provides it", issues)
		return
	}
	for _, c := range provider.DoctorChecks() {
		doctorRequire(d.W, c.OK, doctorRow{c.Label, c.Detail}, doctorRow{c.Label, c.Detail}, strings.Trim(c.Detail, "()"), issues)
	}
}

var doctorBackendChecks = map[string]func(context.Context, *Doctor, *[]string) error{
	"ssh": doctorCheckSSHBackend, "daytona": doctorCheckDaytonaBackend, "vercel_sandbox": doctorCheckVercelBackend,
}

// Reports the Docker, SSH, Daytona, Vercel or plugin terminal backend TERMINAL_ENV selects
func checkTerminalBackend(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		terminalEnv := doctorGetenvDefault("TERMINAL_ENV", "local")
		runningInContainer := func() (in bool) {
			defer func() {
				if recover() != nil {
					in = false
				}
			}()
			return doctorIsContainer()
		}()
		if runningInContainer && terminalEnv != "docker" {
			doctorCheckInfo(d.W, "Running inside a container — using local terminal backend (docker-in-docker is not configured by default)")
			terminalEnv = "local"
		}
		if err := doctorCheckDockerBackend(ctx, d.W, terminalEnv, runningInContainer, &f.Issues); err != nil {
			return err
		}
		if check, ok := doctorBackendChecks[terminalEnv]; ok {
			return check(ctx, d, &f.Issues)
		}
		if !doctorBuiltinTerminalBackends[terminalEnv] {
			doctorCheckPluginBackend(ctx, d, terminalEnv, &f.Issues)
		}
		return nil
	})
}

// Text of a config value the way str renders it, empty for a falsy value
func doctorConfigText(v any) string {
	if !config.PyTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Why the profile config needs no local Chromium, empty when it does
func doctorChromiumNotNeededReason(ctx context.Context, d *Doctor) (string, error) {
	cfg, err := config.LoadReadonly(ctx, d.Home, nil)
	if err != nil {
		return "", err
	}
	if os.Getenv("BROWSER_CDP_URL") != "" || config.PyTruthy(cfg.Get("browser.cdp_url")) {
		return "a CDP override is set", nil
	}
	if strings.ToLower(pystr.Strip(doctorConfigText(cfg.Get("browser.engine")))) == "lightpanda" {
		return "browser.engine is lightpanda", nil
	}
	provider := strings.ToLower(pystr.Strip(doctorConfigText(cfg.Get("browser.cloud_provider"))))
	if provider != "" && provider != "local" {
		return "cloud provider " + provider + " is selected", nil
	}
	return "", nil
}

// Reports the Chromium a local browser session launches, acquiring it through PM only with fix
func doctorCheckChromium(ctx context.Context, d *Doctor, shouldFix bool) error {
	env := doctorToolsEnv(ctx, d)
	found := doctorChromiumExecutable(env)
	if found == "" && shouldFix && !doctorIsTermux() {
		if err := doctorPMEnsure(ctx, "chromium"); err != nil {
			if !doctorPMErrorReported(err) {
				return err
			}
			doctorCheckWarn(d.W, "Chromium install failed", "("+err.Error()+")")
		}
		found = doctorChromiumExecutable(env)
	}
	if found != "" {
		doctorCheckOK(d.W, "Chromium", "("+found+")")
		return nil
	}
	reason, err := doctorChromiumNotNeededReason(ctx, d)
	if err != nil {
		return err
	}
	if reason != "" {
		doctorCheckInfo(d.W, "Chromium not installed (not needed: "+reason+")")
		return nil
	}
	doctorCheckWarn(d.W, "Chromium not installed", "(browser_exec cannot start a local browser)")
	doctorCheckInfo(d.W, browser.ChromiumMissingHint(env))
	return nil
}

// Reports a shadowed Lightpanda engine selection or a selected engine without its binary
func doctorCheckLightpanda(ctx context.Context, d *Doctor) {
	env := doctorToolsEnv(ctx, d)
	if !doctorUsingLightpandaEngine(env) {
		return
	}
	used, reason := func() (used bool, reason string) {
		defer func() {
			if r := recover(); r != nil {
				used, reason = false, fmt.Sprintf("status check failed: %v", r)
			}
		}()
		return doctorLightpandaStatus(env)
	}()
	if !used {
		doctorCheckWarn(d.W, "browser.engine=lightpanda is shadowed", "("+reason+")")
		doctorCheckInfo(d.W, "Fix: pick Lightpanda in `icebath tools` → Browser Automation, or set browser.engine: auto")
		return
	}
	if !doctorCheckBool(d.W, doctorFindLightpandaBinary(env) != "", doctorRow{"Lightpanda", "(" + reason + ")"},
		doctorRow{"Lightpanda selected but binary not found", "(browser tools will fail until it is installed)"}, false) {
		doctorCheckInfo(d.W, browser.LightpandaInstallHint)
	}
}

// Reports Node.js, the Chromium a local browser session launches and the Lightpanda engine
func checkNodeAndBrowser(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		switch {
		case doctorPMToolPath(ctx, "node") != "" || (doctorIsTermux() && safeWhich("node") != ""):
			doctorCheckOK(d.W, "Node.js", "")
		case doctorIsTermux():
			doctorCheckInfo(d.W, "Node.js not found (optional; install on Termux with: pkg install nodejs)")
		default:
			doctorCheckWarn(d.W, "Node.js not found", "(optional; run: icebath pm install node)")
		}
		if err := doctorCheckChromium(ctx, d, shouldFix); err != nil {
			return err
		}
		doctorCheckLightpanda(ctx, d)
		return nil
	})
}

// Singular or plural of vulnerability for a count
func doctorPlural(n any) string {
	if v, ok := n.(int64); ok && v == 1 {
		return "vulnerability"
	}
	if v, ok := n.(float64); ok && v == 1 {
		return "vulnerability"
	}
	return "vulnerabilities"
}

// One npm audit count as a Python number, zero for a missing key and false for a non number
func doctorAuditCount(counts map[string]any, key string) (any, bool) {
	v, present := counts[key]
	if !present {
		return int64(0), true
	}
	switch x := v.(type) {
	case bool:
		if x {
			return int64(1), true
		}
		return int64(0), true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n, true
		}
		if f, err := x.Float64(); err == nil {
			return f, true
		}
	}
	return nil, false
}

// Sum of two Python numbers, an integer when both are integers
func doctorNumberAdd(a, b any) any {
	ai, aInt := a.(int64)
	bi, bInt := b.(int64)
	if aInt && bInt {
		return ai + bi
	}
	return doctorNumberFloat(a) + doctorNumberFloat(b)
}

// Float value of a Python number
func doctorNumberFloat(n any) float64 {
	if i, ok := n.(int64); ok {
		return float64(i)
	}
	return n.(float64)
}

// Environment map as the KEY=VALUE list a child process takes, sorted by name
func doctorEnvList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// Runs one npm audit and reports its counts, any failure leaving the row unprinted
func doctorAuditOne(ctx context.Context, w io.Writer, npmBin, npmDir, label string, auditExtra []string, manualIssues *[]string) {
	defer func() { _ = recover() }()
	argv := append([]string{npmBin, "audit", "--json"}, auditExtra...)
	done, err := doctorSubprocessRun(ctx, argv, pysubprocess.Options{Dir: npmDir, Env: doctorEnvList(pm.WithIcebathNodePath(nil)), Timeout: 30 * time.Second})
	if err != nil {
		return
	}
	var auditData any = map[string]any{}
	if stdout := strings.ToValidUTF8(string(done.Stdout), "�"); pystr.Strip(stdout) != "" {
		dec := json.NewDecoder(bytes.NewReader([]byte(stdout)))
		dec.UseNumber()
		if err := dec.Decode(&auditData); err != nil || dec.More() {
			return
		}
	}
	root, ok := auditData.(map[string]any)
	if !ok {
		return
	}
	var metadata any = map[string]any{}
	if v, present := root["metadata"]; present {
		metadata = v
	}
	meta, ok := metadata.(map[string]any)
	if !ok {
		return
	}
	var vulnerabilities any = map[string]any{}
	if v, present := meta["vulnerabilities"]; present {
		vulnerabilities = v
	}
	counts, ok := vulnerabilities.(map[string]any)
	if !ok {
		return
	}
	critical, okC := doctorAuditCount(counts, "critical")
	high, okH := doctorAuditCount(counts, "high")
	moderate, okM := doctorAuditCount(counts, "moderate")
	if !okC || !okH || !okM {
		return
	}
	total := doctorNumberAdd(doctorNumberAdd(critical, high), moderate)
	workspaceScoped := len(auditExtra) > 0 && auditExtra[0] == "--workspace"
	switch {
	case doctorNumberFloat(total) == 0:
		doctorCheckOK(w, label+" deps", "(no known vulnerabilities)")
	case doctorNumberFloat(critical) > 0 || doctorNumberFloat(high) > 0:
		remedy := "fix is an upstream lockfile bump — a local manual fix does not persist" +
			" (the next `icebath update` reinstalls from the committed lockfile)"
		doctorCheckWarn(w, label+" deps", fmt.Sprintf("(%s critical, %s high, %s moderate — %s)", pyrepr.Str(critical), pyrepr.Str(high), pyrepr.Str(moderate), remedy))
		if workspaceScoped {
			doctorCheckInfo(w, "  ^ workspace audit includes runtime and build dependencies; if manual npm remediation "+
				"errors with an arborist crash it's a known npm bug — clears via a lockfile bump")
		} else {
			doctorCheckInfo(w, "  ^ runtime dependency tree; report/pin the fix in package-lock.json — see #116774")
		}
		*manualIssues = append(*manualIssues, fmt.Sprintf("%s has %s npm %s; wait for an upstream lockfile fix, then update "+
			"your installation (Docker: pull a rebuilt image; recreating the same image restores the same dependencies)",
			label, pyrepr.Str(total), doctorPlural(total)))
	default:
		doctorCheckOK(w, label+" deps", fmt.Sprintf("(%s moderate %s)", pyrepr.Str(moderate), doctorPlural(moderate)))
	}
}

// Session directory, store presence and pairing of the in process WhatsApp adapter for the configured session path
var doctorWhatsAppSession = func(ctx context.Context) (string, bool, bool, error) {
	var extra map[string]any
	if cfg, err := gateway.LoadGatewayConfig(ctx); err == nil {
		if wa := cfg.Platforms[gateway.PlatformWhatsApp]; wa != nil {
			extra = wa.Extra
		}
	} else {
		return "", false, false, err
	}
	return whatsapp.SessionStatus(ctx, extra)
}

// Reports the WhatsApp session store once one exists, paired or awaiting a QR pairing
func doctorCheckWhatsAppSession(ctx context.Context, d *Doctor, f *Finding) {
	dir, present, paired, err := doctorWhatsAppSession(ctx)
	switch {
	case err != nil:
		doctorCheckWarn(d.W, "WhatsApp session store unreadable", "("+err.Error()+")")
	case !present:
	case paired:
		doctorCheckOK(d.W, "WhatsApp session", "(paired, "+dir+")")
	default:
		doctorCheckWarn(d.W, "WhatsApp session not paired", "("+dir+")")
		f.ManualIssues = append(f.ManualIssues, "Pair WhatsApp by scanning the QR code: icebath whatsapp")
	}
}

// Runs npm audit for the root package and the web and ui-tui workspaces, then reports the WhatsApp session
func checkNpmAudit(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheck(func(f *Finding) error {
		npmBin := doctorPMToolPath(ctx, "npm")
		if npmBin == "" && doctorIsTermux() {
			npmBin = safeWhich("npm")
		}
		if npmBin != "" {
			for _, tree := range []struct {
				dir, label string
				extra      []string
			}{
				{d.ProjectRoot, "Root npm package", []string{"--workspaces=false"}},
				{d.ProjectRoot, "web workspace", []string{"--workspace", "web"}},
				{d.ProjectRoot, "ui-tui workspace", []string{"--workspace", "ui-tui"}},
			} {
				modulesRoot := tree.dir
				if len(tree.extra) > 0 {
					modulesRoot = d.ProjectRoot
				}
				if _, err := os.Stat(filepath.Join(modulesRoot, "node_modules")); err == nil {
					doctorAuditOne(ctx, d.W, npmBin, tree.dir, tree.label, tree.extra, &f.ManualIssues)
				}
			}
		}
		doctorCheckWhatsAppSession(ctx, d, f)
		if doctorIsTermux() {
			doctorCheckInfo(d.W, "Termux compatibility fallbacks:")
			for _, note := range doctorTermuxInstallAllFallbackNotes {
				doctorCheckInfo(d.W, note)
			}
		}
		return nil
	})
}

// Available toolset names, unavailable toolset rows and the requirement table of the tool registry
var doctorModelTools = func(ctx context.Context, d *Doctor) ([]string, []doctorToolset, map[string]map[string]any, error) {
	env := doctorToolsEnv(ctx, d)
	available, missing := tools.Default.CheckToolAvailability(ctx, env)
	unavailable := make([]doctorToolset, 0, len(missing))
	for _, item := range missing {
		unavailable = append(unavailable, doctorToolset{Name: item.Name, EnvVars: item.EnvVars, Tools: item.Tools})
	}
	return available, unavailable, tools.Default.ToolsetRequirements(ctx, tools.ScopeKey(d.Home)), nil
}

// Reports each toolset as available or missing its setup and records the setup issue for enabled ones
func checkToolAvailability(ctx context.Context, d *Doctor, shouldFix bool) (*Finding, error) {
	return doctorCheckGuarded(d.W, "Could not check tool availability", "({e})", func(f *Finding) error {
		rawAvailable, rawUnavailable, requirements, err := doctorModelTools(ctx, d)
		if err != nil {
			return err
		}
		available, unavailable := doctorApplyToolAvailabilityOverrides(rawAvailable, rawUnavailable)
		var webRows []doctorWebRow
		webListed := containsString(available, "web")
		for _, item := range unavailable {
			webListed = webListed || item.Name == "web"
		}
		if webListed {
			webRows = doctorWebCapabilityRows(ctx, d)
			if len(webRows) > 0 {
				keptAvailable, keptUnavailable := []string{}, []doctorToolset{}
				for _, tid := range available {
					if tid != "web" {
						keptAvailable = append(keptAvailable, tid)
					}
				}
				for _, item := range unavailable {
					if item.Name != "web" {
						keptUnavailable = append(keptUnavailable, item)
					}
				}
				available, unavailable = keptAvailable, keptUnavailable
			}
		}
		for _, tid := range available {
			name := tid
			if v, ok := requirements[tid]["name"]; ok {
				name = pyrepr.Str(v)
			}
			doctorCheckOK(d.W, name, doctorToolAvailabilityDetail(tid))
		}
		webNotOK := false
		for _, row := range webRows {
			if row.Status == "ok" {
				doctorCheckOK(d.W, row.Label, row.Detail)
			} else {
				webNotOK = true
				doctorCheckWarn(d.W, row.Label, row.Detail)
			}
		}
		for _, item := range unavailable {
			envVars := item.MissingVars
			if len(envVars) == 0 {
				envVars = item.EnvVars
			}
			detail := "(missing " + strings.Join(envVars, ", ") + ")"
			if len(envVars) == 0 {
				hint, hinted := doctorToolsetSetupHints[item.Name]
				if !hinted {
					hint = "(system dependency not met)"
				}
				detail = hint
			}
			doctorCheckWarn(d.W, item.Name, detail)
		}
		if len(doctorMissingAPIKeyToolsetsForSummary(ctx, d, unavailable)) > 0 || webNotOK {
			f.Issues = append(f.Issues, "Run 'icebath setup' to configure missing API keys for full tool access")
		}
		return doctorWarnPartiallyStrippedToolsets(ctx, d, f)
	})
}

// Warns for each enabled CLI toolset that agent.disabled_toolsets cuts down and names the removed tools
func doctorWarnPartiallyStrippedToolsets(ctx context.Context, d *Doctor, f *Finding) error {
	cfg, err := config.Load(d.Home, nil)
	if err != nil {
		return err
	}
	scope := tools.ScopeKey(d.Home)
	enabled := tools.Default.PlatformTools(ctx, scope, cfg, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: true})
	report := tools.Default.ToolsetsPartiallyStripped(ctx, scope, cfg, enabled)
	names := make([]string, 0, len(report))
	for name := range report {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, ts := range names {
		strip := report[ts]
		removed := strings.Join(strip.Stripped, ", ")
		doctorCheckWarn(d.W, ts+" (partly disabled)", "("+removed+" removed by agent.disabled_toolsets: "+strings.Join(strip.Entries, ", ")+")")
		f.Issues = append(f.Issues, ts+": "+doctorCapitalize(strip.Hint)+" "+removed)
	}
	return nil
}
