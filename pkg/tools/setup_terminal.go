package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// SetupConsole is the prompt and print surface of the setup wizard that the setup command provides
type SetupConsole interface {
	Prompt(question, def string, password bool) string
	PromptYesNo(question string, def bool) bool
	PromptChoice(question string, choices []string, def int) int
	PrintHeader(title string)
	PrintInfo(text string)
	PrintSuccess(text string)
	PrintWarning(text string)
	Println()
}

// TerminalBackendPostSetup is the optional interactive hook a plugin backend runs after the wizard selects it
type TerminalBackendPostSetup interface {
	PostSetup(ctx context.Context, env *Env) error
}

// SetupTerminalConsole is the setup wizard console, installed by the setup command
var SetupTerminalConsole SetupConsole

// NousSubscriptionAuthPresent reports Nous auth in the subscription features of a config, installed by pkg/provider
var NousSubscriptionAuthPresent func(env *Env, cfg map[string]any) bool

const setupDocsBase = "https://hermes-agent.nousresearch.com/docs"

var (
	errSetupConsoleUnwired = errors.New("setup wizard console is not installed")
	errNousFeaturesUnwired = errors.New("nous subscription features are not installed")
)

// Seams over the docker probe, managed gateway checks, SDK import and install, home and ssh run
var (
	setupFindDocker              = FindDocker
	setupManagedNousToolsEnabled = func(env *Env) bool { return managedNousToolsEnabled(env, false) }
	setupManagedGatewayReady     = func(env *Env, vendor string) bool { return IsManagedToolGatewayReady(env, vendor, nil, nil) }
	setupHomeDir                 = os.UserHomeDir
	setupRunSSH                  = runSetupSSH
)

// Wizard state for one run, the console and the home every env and config write targets
type terminalSetup struct {
	ctx context.Context
	env *Env
	ui  SetupConsole
	h   string
}

// Prints each line as info, a nil entry printing a bare blank line
func (s *terminalSetup) info(lines ...any) {
	for _, line := range lines {
		if line == nil {
			s.ui.Println()
		} else {
			s.ui.PrintInfo(line.(string))
		}
	}
}

func (s *terminalSetup) getEnv(key string) (string, error) {
	v, _, err := config.GetEnvValue(s.ctx, s.h, key)
	return v, err
}

func (s *terminalSetup) saveEnv(key, value string) error {
	return config.SaveEnvValueContext(s.ctx, s.h, key, value)
}

// Terminal section of a config, the KeyError or type failure of indexing it directly
func terminalSection(cfg map[string]any) (map[string]any, error) {
	raw, ok := cfg["terminal"]
	if !ok {
		return nil, errors.New("KeyError: 'terminal'")
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'setdefault'", pyrepr.TypeName(raw))
	}
	return section, nil
}

// Stores a value under key unless one is already present
func setDefault(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
}

// Prompts for the Vercel Sandbox image, persistence, resources and access token auth
func (s *terminalSetup) promptVercelSandboxSettings(cfg map[string]any) error {
	setDefault(cfg, "terminal", map[string]any{})
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	s.info(nil, "Vercel Sandbox settings:", "  Filesystem persistence uses Vercel snapshots.",
		"  Snapshots restore files only; live processes do not continue after sandbox recreation.")
	currentImage := config.DefaultVercelImage
	if v := terminal["vercel_image"]; pyrepr.Truthy(v) {
		currentImage = pyrepr.Str(v)
	}
	image := pystr.Strip(s.ui.Prompt("  Image (Vercel managed image or VCR repository[:tag])", currentImage, false))
	if image == "" {
		image = currentImage
	}
	terminal["vercel_image"] = image
	if err := s.saveEnv("TERMINAL_VERCEL_IMAGE", image); err != nil {
		return err
	}
	if runtimeValue := terminal["vercel_runtime"]; pyrepr.Truthy(runtimeValue) {
		s.ui.PrintWarning(fmt.Sprintf("terminal.vercel_runtime=%s is deprecated by Vercel and overrides the image; unset it to use the image above.", pyrepr.Repr(runtimeValue)))
	}
	persistLabel := "no"
	if v, ok := terminal["container_persistent"]; !ok || pyrepr.Truthy(v) {
		persistLabel = "yes"
	}
	persist := pystr.Lower(s.ui.Prompt("  Persist filesystem with snapshots? (yes/no)", persistLabel, false))
	terminal["container_persistent"] = slices.Contains([]string{"yes", "true", "y", "1"}, persist)
	resources := []struct {
		key, label string
		def        any
		parse      func(string) (any, bool)
	}{
		{"container_cpu", "  CPU cores", 1, func(text string) (any, bool) {
			f, err := pyint.ParseFloat(text)
			return f, err == nil
		}},
		{"container_memory", "  Memory in MB (5120 = 5GB)", 5120, func(text string) (any, bool) {
			n, ok := pyint.Parse(text)
			if !ok {
				return nil, false
			}
			return pyint.Native(n), true
		}},
	}
	for _, r := range resources {
		current, ok := terminal[r.key]
		if !ok {
			current = r.def
		}
		if parsed, ok := r.parse(s.ui.Prompt(r.label, pyrepr.Str(current), false)); ok {
			terminal[r.key] = parsed
		}
	}
	disk, ok := terminal["container_disk"]
	if !ok {
		disk = 51200
	}
	if !pyrepr.Equal(disk, 0) && !pyrepr.Equal(disk, 51200) {
		s.ui.PrintWarning("Vercel Sandbox does not support custom disk sizing; resetting container_disk to 51200.")
	}
	terminal["container_disk"] = 51200
	s.info(nil, "Vercel authentication:", "  Use a long-lived Vercel access token plus project/team IDs.")
	linked, err := readNearestVercelProject("")
	if err != nil {
		return err
	}
	if len(linked) > 0 {
		s.ui.PrintInfo("  Found defaults in nearest .vercel/project.json.")
	}
	if _, err := config.RemoveHomeEnvValue(s.ctx, s.h, "VERCEL_OIDC_TOKEN"); err != nil {
		return err
	}
	fields := []struct {
		label, envVar, linkedKey string
		secret                   bool
	}{
		{"    Vercel access token", "VERCEL_TOKEN", "", true},
		{"    Vercel project ID", "VERCEL_PROJECT_ID", "projectId", false},
		{"    Vercel team ID", "VERCEL_TEAM_ID", "orgId", false},
	}
	for _, f := range fields {
		def, err := s.getEnv(f.envVar)
		if err != nil {
			return err
		}
		if def == "" && f.linkedKey != "" {
			def = linked[f.linkedKey]
		}
		if value := s.ui.Prompt(f.label, def, f.secret); value != "" {
			if err := s.saveEnv(f.envVar, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Project and team ids of the nearest .vercel/project.json at or above start, the cwd when start is empty
func readNearestVercelProject(start string) (map[string]string, error) {
	if start == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		start = cwd
	}
	current, err := pypath.Resolve(start)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(current); err == nil && info.Mode().IsRegular() {
		current = filepath.Dir(current)
	}
	for directory := current; ; directory = filepath.Dir(directory) {
		projectFile := filepath.Join(directory, ".vercel", "project.json")
		if _, err := os.Stat(projectFile); err == nil {
			return readVercelProjectFile(projectFile)
		}
		if filepath.Dir(directory) == directory {
			return map[string]string{}, nil
		}
	}
}

// Non blank string projectId and orgId of a link file, empty on read failure, malformed JSON or a non object
func readVercelProjectFile(projectFile string) (map[string]string, error) {
	out := map[string]string{}
	raw, err := os.ReadFile(projectFile)
	if err != nil {
		return out, nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil, err
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return out, nil
	}
	get := func(key string) (any, bool) { return nil, false }
	switch obj := data.(type) {
	case *pyjson.Object:
		get = obj.Get
	case map[string]any:
		get = func(key string) (any, bool) { v, ok := obj[key]; return v, ok }
	default:
		return out, nil
	}
	for _, key := range []string{"projectId", "orgId"} {
		if v, ok := get(key); ok {
			if str, isStr := v.(string); isStr && pystr.Strip(str) != "" {
				out[key] = str
			}
		}
	}
	return out, nil
}

// Prompts for a secret and saves it to the home .env when non empty, confirming when asked to
func (s *terminalSetup) promptSecretEnv(label, envVar, confirmMsg string) error {
	value := s.ui.Prompt(label, "", true)
	if value == "" {
		return nil
	}
	if err := s.saveEnv(envVar, value); err != nil {
		return err
	}
	if confirmMsg != "" {
		s.ui.PrintSuccess(confirmMsg)
	}
	return nil
}

// True when the variable is already set and the user declines to update it
func (s *terminalSetup) existingSecretKeeps(envVar, label, question string) (bool, error) {
	v, err := s.getEnv(envVar)
	if err != nil || v == "" {
		return false, err
	}
	s.ui.PrintInfo(fmt.Sprintf("  %s: already configured", label))
	return !s.ui.PromptYesNo(question, false), nil
}

// Reports whether an SDK sync failure is the install, OS or value error class the wizard reports
func setupSyncFailureHandled(err error) bool {
	var installErr *pm.InstallError
	if errors.As(err, &installErr) || pypath.IsOSError(err) {
		return true
	}
	var pyErr *pm.PyError
	return errors.As(err, &pyErr) && slices.Contains(setupHandledPyClasses, pyErr.Class)
}

// Exported setupSyncFailureHandled for the text to speech engine installs in pkg/media
func SetupSyncFailureHandled(err error) bool { return setupSyncFailureHandled(err) }

var setupHandledPyClasses = []string{"OSError", "IOError", "EnvironmentError", "FileNotFoundError", "FileExistsError", "PermissionError",
	"IsADirectoryError", "NotADirectoryError", "TimeoutError", "InterruptedError", "BlockingIOError", "ChildProcessError",
	"ProcessLookupError", "ConnectionError", "BrokenPipeError", "ConnectionAbortedError", "ConnectionRefusedError",
	"ConnectionResetError", "ValueError", "UnicodeError", "UnicodeDecodeError", "UnicodeEncodeError", "JSONDecodeError"}

// Prints where a binary was found or warns with the install hint
func (s *terminalSetup) reportBinary(found, missing, installHint, foundPrefix string) {
	if found != "" {
		s.ui.PrintInfo(foundPrefix + found)
		return
	}
	s.ui.PrintWarning(missing)
	s.ui.PrintInfo(installHint)
}

// Selects the local backend defaulting the working directory to the home directory
func (s *terminalSetup) backendLocal(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Local")
	s.ui.PrintInfo("Commands run directly on this machine.")
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	if _, ok := terminal["cwd"]; !ok {
		homeDir, err := setupHomeDir()
		if err != nil {
			return err
		}
		terminal["cwd"] = homeDir
	}
	return nil
}

// Reports the container runtime, defaults the image and offers the egress firewall
func (s *terminalSetup) backendDocker(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Docker / Podman")
	dockerExe := setupFindDocker(s.env)
	prefix := ""
	if dockerExe != "" {
		prefix = DockerRuntimeName(dockerExe) + " found: "
	}
	s.reportBinary(dockerExe, "Docker or Podman not found in PATH!",
		"Install Docker: https://docs.docker.com/get-docker/ or Podman: https://podman.io/docs/installation", prefix)
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	setDefault(terminal, "docker_image", config.DefaultSandboxImage)
	s.info(nil, "Docker sandboxes can be protected with the egress credential firewall.",
		"It routes sandbox traffic through iron-proxy so containers receive proxy tokens instead of real API keys.",
		"   Docker only for now; Modal, SSH, Daytona and plugin backends are not wired yet.")
	if !s.ui.PromptYesNo("  Enable egress firewall for Docker sandboxes?", false) {
		s.ui.PrintInfo("Skipping egress firewall. You can enable it later with `icebath egress setup`.")
		return nil
	}
	setDefault(cfg, "proxy", map[string]any{})
	proxy, ok := cfg["proxy"].(map[string]any)
	if !ok {
		return fmt.Errorf("TypeError: '%s' object does not support item assignment", pyrepr.TypeName(cfg["proxy"]))
	}
	proxy["enabled"] = true
	setDefault(proxy, "enforce_on_docker", true)
	s.ui.PrintSuccess("Egress firewall enabled in config")
	s.ui.PrintInfo("Run `icebath egress setup` then `icebath egress start` to mint tokens and launch the proxy.")
	return nil
}

// Chooses managed or direct Modal billing and collects direct Modal credentials
func (s *terminalSetup) backendModal(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Modal")
	s.ui.PrintInfo("Serverless cloud sandboxes. Each session gets its own container.")
	managedAvailable := false
	if setupManagedNousToolsEnabled(s.env) {
		if NousSubscriptionAuthPresent == nil {
			return errNousFeaturesUnwired
		}
		managedAvailable = NousSubscriptionAuthPresent(s.env, cfg) && setupManagedGatewayReady(s.env, "modal")
	}
	modeValue, _ := config.CfgGet(cfg, "terminal", "modal_mode")
	modalMode := NormalizeModalMode(pyOrStr(modeValue))
	terminal, err := terminalSection(cfg)
	useManaged := false
	if managedAvailable {
		tokenID, err := s.getEnv("MODAL_TOKEN_ID")
		if err != nil {
			return err
		}
		defaultIdx := 0
		switch {
		case modalMode == "direct" || (modalMode != "managed" && tokenID != ""):
			defaultIdx = 1
		}
		useManaged = s.ui.PromptChoice("Select how Modal execution should be billed:",
			[]string{"Use my Nous subscription", "Use my own Modal account"}, defaultIdx) == 0
	}
	if err != nil {
		return err
	}
	if useManaged {
		terminal["modal_mode"] = "managed"
		s.ui.PrintInfo("Modal execution will use the managed Nous gateway and bill to your subscription.")
		for _, key := range []string{"MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"} {
			v, err := s.getEnv(key)
			if err != nil {
				return err
			}
			if v != "" {
				s.ui.PrintInfo("Direct Modal credentials are still configured, but this backend is pinned to managed mode.")
				break
			}
		}
		return nil
	}
	terminal["modal_mode"] = "direct"
	s.ui.PrintInfo("Requires a Modal account: https://modal.com")
	s.info(nil, "Modal authentication:", "  Get your token at: https://modal.com/settings")
	if keeps, err := s.existingSecretKeeps("MODAL_TOKEN_ID", "Modal token", "  Update Modal credentials?"); err != nil || keeps {
		return err
	}
	if err := s.promptSecretEnv("    Modal Token ID", "MODAL_TOKEN_ID", ""); err != nil {
		return err
	}
	return s.promptSecretEnv("    Modal Token Secret", "MODAL_TOKEN_SECRET", "")
}

// Installs the Daytona SDK, collects the API key and defaults the sandbox image
func (s *terminalSetup) backendDaytona(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Daytona")
	s.info("Persistent cloud development environments.",
		"Each session gets a dedicated sandbox with filesystem persistence.",
		"Sign up at: https://daytona.io")
	s.ui.Println()
	existing, err := s.getEnv("DAYTONA_API_KEY")
	if err != nil {
		return err
	}
	keeps, err := s.existingSecretKeeps("DAYTONA_API_KEY", "Daytona API key", "  Update API key?")
	if err != nil {
		return err
	}
	if !keeps {
		confirm := "    Configured"
		if existing != "" {
			confirm = "    Updated"
		}
		if err := s.promptSecretEnv("    Daytona API key", "DAYTONA_API_KEY", confirm); err != nil {
			return err
		}
	}
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	setDefault(terminal, "daytona_image", config.DefaultSandboxImage)
	return nil
}

// Announces the Vercel backend and prompts for the sandbox settings
func (s *terminalSetup) backendVercel(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: Vercel Sandbox")
	s.info("Cloud microVM sandboxes with snapshot-backed filesystem persistence.")
	return s.promptVercelSandboxSettings(cfg)
}

// Collects SSH host, user, port and key, saving the port only when it is not 22, and tests the connection
func (s *terminalSetup) backendSSH(cfg map[string]any) error {
	s.ui.PrintSuccess("Terminal backend: SSH")
	s.ui.PrintInfo("Run commands on a remote machine via SSH.")
	homeDir, err := setupHomeDir()
	if err != nil {
		return err
	}
	fields := []struct{ label, envVar, def string }{
		{"  SSH host (hostname or IP)", "TERMINAL_SSH_HOST", ""},
		{"  SSH user", "TERMINAL_SSH_USER", os.Getenv("USER")},
		{"  SSH port", "TERMINAL_SSH_PORT", "22"},
		{"  SSH private key path", "TERMINAL_SSH_KEY", filepath.Join(homeDir, ".ssh", "id_rsa")},
	}
	values := make([]string, 0, len(fields))
	for _, f := range fields {
		def, err := s.getEnv(f.envVar)
		if err != nil {
			return err
		}
		if def == "" {
			def = f.def
		}
		value := s.ui.Prompt(f.label, def, false)
		values = append(values, value)
		switch {
		case value != "" && (f.envVar != "TERMINAL_SSH_PORT" || value != "22"):
			if err := s.saveEnv(f.envVar, value); err != nil {
				return err
			}
		case f.envVar == "TERMINAL_SSH_PORT" && value == "22":
			if _, err := config.RemoveHomeEnvValue(s.ctx, s.h, f.envVar); err != nil {
				return err
			}
		}
	}
	host, user, port, sshKey := values[0], values[1], values[2], values[3]
	if host == "" || !s.ui.PromptYesNo("  Test SSH connection?", true) {
		return nil
	}
	s.ui.PrintInfo("  Testing connection...")
	argv := []string{"ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5"}
	if sshKey != "" {
		argv = append(argv, "-i", sshKey)
	}
	if port != "" && port != "22" {
		argv = append(argv, "-p", port)
	}
	target := host
	if user != "" {
		target = user + "@" + host
	}
	argv = append(argv, target, "echo ok")
	code, stderr, err := setupRunSSH(s.ctx, argv)
	if err != nil {
		return err
	}
	if code == 0 {
		s.ui.PrintSuccess("  SSH connection successful!")
		return nil
	}
	s.ui.PrintWarning(fmt.Sprintf("  SSH connection failed: %s", pystr.Strip(stderr)))
	s.ui.PrintInfo("  Check your SSH key and host settings.")
	return nil
}

// Runs the connection probe with a ten second limit, the timeout and a missing binary returned as errors
func runSetupSSH(ctx context.Context, argv []string) (int, string, error) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return 0, "", fmt.Errorf("Command %s timed out after 10 seconds", pyrepr.Repr(argv))
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), pystr.DecodeUTF8Replace([]byte(stderr.String())), nil
	}
	if err != nil {
		return 0, "", err
	}
	return 0, pystr.DecodeUTF8Replace([]byte(stderr.String())), nil
}

// Runs a plugin backend's setup lines and post setup hook, any failure printed as a warning
func (s *terminalSetup) backendPlugin(backend string) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		provider := GetTerminalBackend(backend)
		if provider == nil {
			return errors.New("'NoneType' object has no attribute 'display_name'")
		}
		s.ui.PrintSuccess("Terminal backend: " + provider.DisplayName())
		for _, line := range provider.SetupInstructions(s.env) {
			s.ui.PrintInfo(line)
		}
		if hook, ok := provider.(TerminalBackendPostSetup); ok {
			return hook.PostSetup(s.ctx, s.env)
		}
		return nil
	}()
	if err != nil {
		s.ui.PrintWarning(fmt.Sprintf("Backend plugin setup hook failed: %s", err.Error()))
	}
}

// Built in backend names with their menu labels in menu order
var builtinTerminalBackends = [][2]string{
	{"local", "Local - run directly on this machine (default)"},
	{"docker", "Docker/Podman - isolated container with configurable resources"},
	{"modal", "Modal - serverless cloud sandbox"}, {"ssh", "SSH - run on a remote machine"},
	{"daytona", "Daytona - persistent cloud development environment"},
	{"vercel_sandbox", "Vercel Sandbox - cloud microVM with snapshot filesystem persistence"},
}

// Wizard step of each built in backend
var terminalBackendSetup = map[string]func(s *terminalSetup, cfg map[string]any) error{
	"local": (*terminalSetup).backendLocal, "docker": (*terminalSetup).backendDocker, "modal": (*terminalSetup).backendModal,
	"daytona": (*terminalSetup).backendDaytona, "vercel_sandbox": (*terminalSetup).backendVercel, "ssh": (*terminalSetup).backendSSH,
}

// Env var, config key and default mirrored into the home .env after a backend is configured
var backendEnvMirror = map[string][3]any{
	"modal":          {"TERMINAL_MODAL_MODE", "modal_mode", "auto"},
	"vercel_sandbox": {"TERMINAL_VERCEL_IMAGE", "vercel_image", config.DefaultVercelImage},
}

// Plugin registered backends sorted by name, the built in reserved names left out
func pluginTerminalBackends() []BackendProvider {
	var providers []BackendProvider
	for _, name := range TerminalBackendNames() {
		if builtinBackends[name] {
			continue
		}
		if p := GetTerminalBackend(name); p != nil {
			providers = append(providers, p)
		}
	}
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].Name() < providers[j].Name() })
	return providers
}

// Menu rows for plugin backends, none when discovery fails like the fail soft wizard
func (s *terminalSetup) pluginBackendRows() (rows [][2]string, names []string) {
	defer func() {
		if r := recover(); r != nil {
			toolsLogger(s.env).Debug(fmt.Sprintf("terminal backend plugin listing failed: %v", r))
		}
	}()
	discoverErr := errPluginDiscoveryUnwired
	if EnsurePluginsDiscovered != nil {
		discoverErr = EnsurePluginsDiscovered(s.ctx)
	}
	if discoverErr != nil {
		return nil, nil
	}
	for _, provider := range pluginTerminalBackends() {
		pname := pystr.Lower(pystr.Strip(provider.Name()))
		rows = append(rows, [2]string{pname, provider.DisplayName() + " - " + provider.Description()})
		names = append(names, pname)
	}
	return rows, names
}

// SetupTerminalBackend runs the terminal backend wizard, persisting the choice to config and the home .env
func SetupTerminalBackend(ctx context.Context, env *Env, cfg map[string]any) error {
	if SetupTerminalConsole == nil {
		return errSetupConsoleUnwired
	}
	s := &terminalSetup{ctx: ctx, env: env, ui: SetupTerminalConsole, h: envHome(env)}
	s.ui.PrintHeader("Terminal Backend")
	s.info("Choose where Icebath runs shell commands and code.",
		"This affects tool execution, file access, and isolation.",
		fmt.Sprintf("   Guide: %s/user-guide/configuration#terminal-backend-configuration", setupDocsBase), nil)
	current, ok := config.CfgGet(cfg, "terminal", "backend")
	if !ok {
		current = "local"
	}
	currentBackend := pyrepr.Str(current)
	backends := slices.Clone(builtinTerminalBackends)
	pluginRows, pluginNames := s.pluginBackendRows()
	backends = append(backends, pluginRows...)
	choices := make([]string, 0, len(backends)+1)
	for _, b := range backends {
		choices = append(choices, b[1])
	}
	choices = append(choices, fmt.Sprintf("Keep current (%s)", currentBackend))
	idx := s.ui.PromptChoice("Select terminal backend:", choices, len(backends))
	if idx == len(backends) {
		s.ui.PrintInfo(fmt.Sprintf("Keeping current backend: %s", currentBackend))
		return nil
	}
	setDefault(cfg, "terminal", map[string]any{})
	terminal, err := terminalSection(cfg)
	if err != nil {
		return err
	}
	if idx < 0 || idx > len(backends) {
		terminal["backend"] = nil
		return errors.New("AttributeError: 'NoneType' object has no attribute 'replace'")
	}
	selected := backends[idx][0]
	terminal["backend"] = selected
	handler, builtin := terminalBackendSetup[selected]
	switch {
	case builtin && (selected != "ssh" || !slices.Contains(pluginNames, selected)):
		if err := handler(s, cfg); err != nil {
			return err
		}
	case slices.Contains(pluginNames, selected):
		s.backendPlugin(selected)
	}
	if err := s.saveEnv("TERMINAL_ENV", selected); err != nil {
		return err
	}
	if mirror, ok := backendEnvMirror[selected]; ok {
		terminal, err := terminalSection(cfg)
		if err != nil {
			return err
		}
		value, ok := terminal[mirror[1].(string)]
		if !ok {
			value = mirror[2]
		}
		text, isStr := value.(string)
		if !isStr {
			return fmt.Errorf("AttributeError: '%s' object has no attribute 'replace'", pyrepr.TypeName(value))
		}
		if err := s.saveEnv(mirror[0].(string), text); err != nil {
			return err
		}
	}
	if err := config.SaveConfig(ctx, s.h, cfg, config.SaveOptions{}); err != nil {
		return err
	}
	s.ui.Println()
	s.ui.PrintSuccess(fmt.Sprintf("Terminal backend set to: %s", selected))
	return nil
}
