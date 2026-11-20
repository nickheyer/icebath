package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// SetupConsole is the prompt and print surface the terminal backend setup flow runs on
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

// SetupTerminalConsole is the terminal setup console, installed by pkg/provider
var SetupTerminalConsole SetupConsole

// Example config file and key documenting every terminal backend
const terminalBackendGuide = "cli-config.yaml.example, key terminal.backend"

var errSetupConsoleUnwired = errors.New("setup wizard console is not installed")

// Seams over the docker probe, home and ssh run
var (
	setupFindDocker = FindDocker
	setupHomeDir    = os.UserHomeDir
	setupRunSSH     = runSetupSSH
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

// Terminal section of a config, an error when it is missing or not a mapping
func terminalSection(cfg map[string]any) (map[string]any, error) {
	raw, ok := cfg["terminal"]
	if !ok {
		return nil, errors.New("the config has no terminal section")
	}
	section, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("terminal in config is %s, expected a mapping", errchain.Kind(raw))
	}
	return section, nil
}

// Stores a value under key unless one is already present
func setDefault(m map[string]any, key string, value any) {
	if _, ok := m[key]; !ok {
		m[key] = value
	}
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

var setupHandledPyClasses = []string{"OSError", "IOError", "EnvironmentError", "FileNotFoundError", "FileExistsError", "PermissionError",
	"IsADirectoryError", "NotADirectoryError", "TimeoutError", "InterruptedError", "BlockingIOError", "ChildProcessError",
	"ProcessLookupError", "ConnectionError", "BrokenPipeError", "ConnectionAbortedError", "ConnectionRefusedError",
	"ConnectionResetError", "ValueError", "UnicodeError", "UnicodeDecodeError", "UnicodeEncodeError", "JSONDecodeError"}

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
	if err := runPluginBackendSetup(s.ctx, s.env, backend, s.ui.PrintSuccess, s.ui.PrintInfo); err != nil {
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
	"local": (*terminalSetup).backendLocal,
}

// Env var, config key and default mirrored into the home .env after a backend is configured
var backendEnvMirror = map[string][3]any{
	"vercel_sandbox": {"TERMINAL_VERCEL_IMAGE", "vercel_image", config.DefaultVercelImage},
}

// Menu rows for plugin backends, none when discovery fails like the fail soft wizard
func (s *terminalSetup) pluginBackendRows() (rows [][2]string, names []string) {
	for _, row := range SetupTerminalBackendRows(s.ctx, s.env) {
		rows = append(rows, [2]string{row.Name, row.DisplayName + " - " + row.Description})
		names = append(names, row.Name)
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
		"   Guide: "+terminalBackendGuide, nil)
	current, ok := config.CfgGet(cfg, "terminal", "backend")
	if !ok {
		current = "local"
	}
	currentBackend := pyrepr.Str(current)
	backends := slices.DeleteFunc(slices.Clone(builtinTerminalBackends), func(row [2]string) bool { return builtinBackend(row[0]) == nil })
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
		return errors.New("no terminal backend was selected")
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
			return fmt.Errorf("terminal.%v is %s, expected text", mirror[1], errchain.Kind(value))
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
