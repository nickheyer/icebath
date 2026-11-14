package env

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

const builtinBackendList = "local, docker, modal, daytona, vercel_sandbox, ssh"

var supportedVercelRuntimes = []string{"node24", "node22", "python3.13"}

type BackendConfigError struct {
	Msg string
}

func (e *BackendConfigError) Error() string { return e.Msg }

type buildRequest struct {
	config  EnvConfig
	kind    string
	image   string
	cwd     string
	timeout int
	key     string
	hostCwd string
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

func (m *Manager) builtinBuilder(kind string) (func(context.Context, buildRequest) (*Shell, error), bool) {
	switch kind {
	case "local":
		return m.buildLocal, true
	}
	return nil, false
}

func (m *Manager) create(ctx context.Context, req buildRequest) (*Shell, error) {
	build, ok := m.builtinBuilder(req.kind)
	if !ok {
		build = m.buildPlugin
	}
	sh, err := build(ctx, req)
	if err != nil {
		return nil, err
	}
	sh.setEnvType(req.kind)
	return sh, nil
}

func (m *Manager) limits(ctx context.Context) (tool.OutputLimits, error) {
	cfg, err := m.cfg.LoadReadonly(ctx)
	if err != nil {
		return tool.OutputLimits{}, err
	}
	return tool.Limits(cfg), nil
}

func (m *Manager) buildLocal(ctx context.Context, req buildRequest) (*Shell, error) {
	limits, err := m.limits(ctx)
	if err != nil {
		return nil, err
	}
	return m.host.OpenLocal(ctx, LocalOptions{Cwd: req.cwd, Timeout: seconds(req.timeout), Persistent: req.config.LocalPersistent,
		Prepare: m.prepare, Foreground: m.fg, Limits: limits})
}

func (m *Manager) wrapBackend(ctx context.Context, b Backend, cwd string, timeoutSeconds int) (*Shell, error) {
	limits, err := m.limits(ctx)
	if err != nil {
		return nil, err
	}
	homeDir, err := m.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	timeout := seconds(timeoutSeconds)
	if timeout <= 0 {
		timeout = defaultLocalTimeout
	}
	sh, err := NewShell(b, ShellOptions{Cwd: cwd, Timeout: timeout, Prepare: m.prepare, Foreground: m.fg,
		Passthrough: m.host.passthrough, Secrets: m.secrets, Homes: m.homes, Home: homeDir, Limits: limits})
	if err != nil {
		return nil, err
	}
	sh.InitSession(ctx)
	return sh, nil
}

func (m *Manager) buildPlugin(ctx context.Context, req buildRequest) (*Shell, error) {
	p, ok, err := m.providers.Get(ctx, req.kind, "")
	if err != nil {
		return nil, err
	}
	if !ok {
		names, err := m.providers.Names(ctx, "")
		if err != nil {
			return nil, err
		}
		quoted := make([]string, 0, 6+len(names))
		for _, n := range append(strings.Split(builtinBackendList, ", "), names...) {
			quoted = append(quoted, "'"+n+"'")
		}
		return nil, &BackendConfigError{Msg: fmt.Sprintf("Unknown environment type: %s. Use %s%s", req.kind, strings.Join(quoted, ", "),
			m.hints.TerminalBackendInstallHint(ctx, req.kind))}
	}
	if hp, isHosted := p.(HostedProvider); isHosted && hp.Hosted() {
		return nil, &BackendConfigError{Msg: fmt.Sprintf("terminal.backend is %s, a plugin that runs in the plugin host (plugins.isolation: host), "+
			"and the host cannot hand its environment to icebath, so commands cannot run in it. Set "+
			"plugins.isolation: in_process to use this backend.", req.kind)}
	}
	var cc *ContainerConfig
	if m.settings.IsContainerBackend(ctx, req.kind) {
		cc = &ContainerConfig{CPU: req.config.ContainerCPU, Memory: req.config.ContainerMemory, Disk: req.config.ContainerDisk,
			Persistent: req.config.ContainerPersistent}
	}
	b, err := p.New(ctx, ProviderOptions{Cwd: req.cwd, Timeout: seconds(req.timeout), TaskID: req.key, Image: req.image, Container: cc})
	if err != nil {
		return nil, err
	}
	sh, err := m.wrapBackend(ctx, b, req.cwd, req.timeout)
	if err != nil {
		return nil, err
	}
	declared := p.Flags().DefaultCwd
	sandboxHome := declared
	if declared == "~" {
		sandboxHome = ""
		if req.cwd == "~" {
			sandboxHome = sh.Cwd()
		}
	}
	if strings.HasPrefix(sandboxHome, "/") && sandboxHome != "/" {
		sh.setSandboxHome(sandboxHome)
	}
	return sh, nil
}

func (m *Manager) CheckRequirements(ctx context.Context, cfg EnvConfig) bool {
	m.RecordUnavailableReason("", false)
	switch cfg.Kind {
	case "local":
		return true
	case "ssh":
		if cfg.SSHHost != "" && cfg.SSHUser != "" {
			return true
		}
		return m.reject("the SSH host and user are not configured (TERMINAL_SSH_HOST / TERMINAL_SSH_USER); " +
			"run `icebath setup terminal` to enter them or pick the 'local' backend")
	case "modal":
		state, err := m.modalBackendState(ctx, cfg.ModalMode)
		if err != nil {
			return m.requirementsFailed(err)
		}
		if state.Selected == "managed" || state.Selected == "direct" {
			return true
		}
		logMsg, _ := m.modalUnavailableReason(ctx, state)
		return m.reject(logMsg)
	case "vercel_sandbox":
		return m.checkVercel(ctx, cfg)
	case "daytona":
		_, ok, err := m.secrets.Lookup(ctx, "DAYTONA_API_KEY")
		if err != nil {
			return m.requirementsFailed(err)
		}
		if !ok {
			return m.reject("the 'daytona' backend is missing its API key or SDK")
		}
		return true
	}
	return m.checkPluginRequirements(ctx, cfg)
}

func (m *Manager) requirementsFailed(err error) bool {
	slog.Error(fmt.Sprintf("Terminal requirements check failed: %v", err))
	m.RecordUnavailableReason(fmt.Sprintf("the requirements check failed: %v", err), true)
	return false
}

func (m *Manager) checkVercel(ctx context.Context, cfg EnvConfig) bool {
	runtime := strings.TrimSpace(cfg.VercelRuntime)
	if runtime != "" && !slices.Contains(supportedVercelRuntimes, runtime) {
		return m.reject(fmt.Sprintf("Vercel Sandbox runtime '%s' is not supported. Set TERMINAL_VERCEL_RUNTIME to one of: %s.",
			runtime, strings.Join(supportedVercelRuntimes, ", ")))
	}
	if cfg.ContainerDisk != 0 && cfg.ContainerDisk != 51200 {
		return m.reject(fmt.Sprintf("Vercel Sandbox does not support custom TERMINAL_CONTAINER_DISK=%d. Use the default shared setting (51200 MB).", cfg.ContainerDisk))
	}
	oidc, err := m.secrets.Get(ctx, "VERCEL_OIDC_TOKEN", "")
	if err != nil {
		return m.requirementsFailed(err)
	}
	if oidc != "" {
		return true
	}
	present := 0
	for _, k := range []string{"VERCEL_TOKEN", "VERCEL_PROJECT_ID", "VERCEL_TEAM_ID"} {
		v, err := m.secrets.Get(ctx, k, "")
		if err != nil {
			return m.requirementsFailed(err)
		}
		if v != "" {
			present++
		}
	}
	if present == 3 {
		return true
	}
	head := "selected but no supported auth configuration was found. Set VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID for normal use."
	if present > 0 {
		head = "selected with token auth, but VERCEL_TOKEN, VERCEL_PROJECT_ID, and VERCEL_TEAM_ID must all be set together."
	}
	return m.reject("Vercel Sandbox backend " + head + " VERCEL_OIDC_TOKEN is supported for one-off local development only.")
}

func (m *Manager) checkPluginRequirements(ctx context.Context, cfg EnvConfig) bool {
	m.RecordUnavailableReason("", false)
	p, ok, err := m.providers.Get(ctx, cfg.Kind, "")
	if err != nil {
		return m.requirementsFailed(err)
	}
	if ok {
		loaded, err := m.cfg.LoadReadonly(ctx)
		if err != nil {
			return m.requirementsFailed(err)
		}
		if p.CheckRequirements(ctx, loaded.Terminal) {
			return true
		}
		return m.reject(fmt.Sprintf("the '%s' plugin backend reported its requirements are not met", cfg.Kind))
	}
	return m.reject(fmt.Sprintf("Unknown TERMINAL_ENV '%s'. Use one of: %s, or a plugin-registered backend%s",
		cfg.Kind, builtinBackendList, m.hints.TerminalBackendInstallHint(ctx, cfg.Kind)))
}

func (m *Manager) hasDirectModalCredentials(ctx context.Context) (bool, error) {
	id, err := m.secrets.Get(ctx, "MODAL_TOKEN_ID", "")
	if err != nil {
		return false, err
	}
	sec, err := m.secrets.Get(ctx, "MODAL_TOKEN_SECRET", "")
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(id) != "" && strings.TrimSpace(sec) != "" {
		return true, nil
	}
	userHome, err := home.UserHome()
	if err != nil || userHome == "" {
		return false, nil
	}
	_, statErr := os.Stat(filepath.Join(userHome, ".modal.toml"))
	return statErr == nil, nil
}

func (m *Manager) modalBackendState(ctx context.Context, mode string) (modalState, error) {
	hasDirect, err := m.hasDirectModalCredentials(ctx)
	if err != nil {
		return modalState{}, err
	}
	return resolveModalState(mode, hasDirect, m.gateway.Ready(ctx, "modal"), m.gateway.Enabled(ctx, false)), nil
}

func (m *Manager) modalUnavailableReason(ctx context.Context, state modalState) (string, string) {
	gateway := m.gateway.UnavailableMessage(ctx, "managed Modal execution", false)
	if state.ManagedModeBlocked || state.RequestedMode == "managed" {
		tail := "the managed tool gateway is unavailable. " + gateway
		if state.ManagedModeBlocked {
			tail = "Icebath Portal Tool Gateway access is not currently available and no direct Modal credentials/config " +
				"were found. " + gateway + " Choose TERMINAL_MODAL_MODE=direct/auto to use direct Modal credentials."
		}
		return "Modal backend selected with TERMINAL_MODAL_MODE=managed, but " + tail,
			"Modal backend is configured for managed mode, but " + tail
	}
	managed := m.gateway.Enabled(ctx, false)
	if state.RequestedMode == "direct" {
		choice := "auto"
		if managed {
			choice = "managed/auto"
		}
		return "Modal backend selected with TERMINAL_MODAL_MODE=direct, but no direct Modal credentials/config " +
				"were found. Configure Modal or choose TERMINAL_MODAL_MODE=" + choice + ".",
			"Modal backend is configured for direct mode, but no direct Modal credentials/config were found."
	}
	found, fix := "was found", " or"
	if managed {
		found, fix = "or managed tool gateway was found", ", set up the managed gateway, or"
	}
	return "Modal backend selected but no direct Modal credentials/config " + found + ". Configure Modal" + fix + " choose a different TERMINAL_ENV.",
		"Modal backend selected but no direct Modal credentials/config " + found + "."
}

func (m *Manager) modalUnavailableError(ctx context.Context, state modalState) error {
	_, msg := m.modalUnavailableReason(ctx, state)
	return &BackendConfigError{Msg: msg}
}
