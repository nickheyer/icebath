package browser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/vault"
)

// The backend is locked for this session, so the surface must prompt for the master password
type UnlockRequired struct{ Backend LoginBackend }

func (e *UnlockRequired) Error() string { return e.Backend.DisplayName() + " is locked" }

// Login source of the browser credential vault, listing metadata and resolving one secret at fill time
type LoginBackend interface {
	Name() string
	DisplayName() string
	Prefix() string
	NeedsUnlock() bool
	Owns(handle string) bool
	IsUnlocked() bool
	// Profile exclusive section entered after the origin pre check, the returned function leaving it
	FillContext() (func() error, error)
	ListItems() ([]vault.VaultItemMeta, error)
	GetMeta(handle string) (*vault.VaultItemMeta, error)
	ResolvePassword(handle string) (string, error)
	ResolveOTP(handle string) (string, bool, error)
	ResolveSecret(handle string) (map[string]string, error)
}

// Contract defaults a concrete backend embeds with Self bound to itself
type LoginBackendDefaults struct{ Self LoginBackend }

// Outer backend the defaults dispatch to, panicking when the embedding backend never bound itself
func (d LoginBackendDefaults) self() LoginBackend {
	if d.Self == nil {
		panic("browser.LoginBackendDefaults.Self is not bound to its backend")
	}
	return d.Self
}

// No master password prompt unless overridden
func (LoginBackendDefaults) NeedsUnlock() bool { return false }

// A handle belongs to the backend whose prefix it carries
func (d LoginBackendDefaults) Owns(handle string) bool {
	return strings.HasPrefix(handle, d.self().Prefix())
}

// Always unlocked unless overridden
func (LoginBackendDefaults) IsUnlocked() bool { return true }

// No profile exclusive section around a fill
func (LoginBackendDefaults) FillContext() (func() error, error) {
	return func() error { return nil }, nil
}

// No one time code, the user is asked
func (LoginBackendDefaults) ResolveOTP(string) (string, bool, error) { return "", false, nil }

// Password only payload, the shape external managers resolve
func (d LoginBackendDefaults) ResolveSecret(handle string) (map[string]string, error) {
	password, err := d.self().ResolvePassword(handle)
	if err != nil {
		return nil, err
	}
	return map[string]string{"password": password}, nil
}

// Python str of a value, empty for None and every other falsy value
func vaultText(v any) string {
	if !config.PyTruthy(v) {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return pyrepr.Str(v)
}

// First n code points of a text
func vaultHead(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// Sorted KEY=VALUE pairs of a child environment
func vaultEnvPairs(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// Captured utf-8 result of a manager CLI, a spawn failure or timeout raised as the labelled error
func runVaultCLI(cmd *exec.Cmd, ctx context.Context, timeout time.Duration, label string) (provider.CLIResult, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return provider.CLIResult{}, fmt.Errorf("%s unlock timed out after %.0fs", label, timeout.Seconds())
	}
	res := provider.CLIResult{Stdout: strings.ToValidUTF8(stdout.String(), "�"), Stderr: strings.ToValidUTF8(stderr.String(), "�")}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ReturnCode = exitErr.ExitCode()
			return res, nil
		}
		return provider.CLIResult{}, fmt.Errorf("failed to invoke %s: %v", label, err)
	}
	return res, nil
}

// Runs a manager CLI feeding the secret on stdin, never argv and never env
func RunWithStdinSecret(argv []string, env map[string]string, secret string, timeout time.Duration, label string) (provider.CLIResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = vaultEnvPairs(env)
	cmd.Stdin = strings.NewReader(secret + "\n")
	return runVaultCLI(cmd, ctx, timeout, label)
}

// Runs a manager CLI that reads the secret from a named variable set on the child environment only
func RunWithSecretEnv(argv []string, env map[string]string, secretEnv, secret string, timeout time.Duration, label string) (provider.CLIResult, error) {
	childEnv := make(map[string]string, len(env)+1)
	for k, v := range env {
		childEnv[k] = v
	}
	childEnv[secretEnv] = secret
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = vaultEnvPairs(childEnv)
	return runVaultCLI(cmd, ctx, timeout, label)
}

// The vault section of the read only config, empty when absent or not a mapping
func vaultCfg(ctx context.Context) (map[string]any, error) {
	cfg, err := config.LoadReadonly(ctx, home.Dir(ctx), nil)
	if err != nil {
		return nil, err
	}
	section, _ := cfg.Lookup("vault")
	switch m := section.(type) {
	case map[string]any:
		return m, nil
	case config.Tree:
		return map[string]any(m), nil
	}
	return map[string]any{}, nil
}

// Config section reader of the vault, the module attribute hermes tests patch
var vaultCfgFn = vaultCfg

// Identity and constructor of one external password manager backend
type externalLoginBackend struct {
	name, prefix, displayName string
	new                       func(ctx context.Context, cfg map[string]any) LoginBackend
}

// The external manager backends in their hermes order, 1Password then Bitwarden
func externalBackendClasses() []externalLoginBackend {
	return []externalLoginBackend{
		{name: "onepassword", prefix: "op:", displayName: "1Password", new: func(ctx context.Context, cfg map[string]any) LoginBackend {
			return NewOnePasswordLoginBackend(ctx, cfg)
		}},
		{name: "bitwarden", prefix: "bw:", displayName: "Bitwarden", new: func(ctx context.Context, cfg map[string]any) LoginBackend { return NewBitwardenLoginBackend(ctx, cfg) }},
	}
}

// Whether the manager CLI is reachable, a configured binary_path honoured over PATH
func IsLoginBackendInstalled(ctx context.Context, name string) (bool, error) {
	cfg, err := vaultCfgFn(ctx)
	if err != nil {
		return false, err
	}
	section, _ := cfg[name].(map[string]any)
	explicit := ""
	if section != nil {
		explicit = vaultText(section["binary_path"])
	}
	if explicit != "" {
		info, err := os.Stat(explicit)
		return err == nil && info.Mode().IsRegular(), nil
	}
	if name == "onepassword" {
		return provider.FindOp("") != "", nil
	}
	_, err = exec.LookPath("bw")
	return err == nil, nil
}

// An installed manager is a login source unless the profile set vault.<name>.enabled to false
func IsLoginBackendEnabled(ctx context.Context, name string) (bool, error) {
	cfg, err := vaultCfgFn(ctx)
	if err != nil {
		return false, err
	}
	if section, ok := cfg[name].(map[string]any); ok {
		if enabled, isBool := section["enabled"].(bool); isBool && !enabled {
			return false, nil
		}
	}
	return IsLoginBackendInstalled(ctx, name)
}

// Enablement reader of an external manager, the module attribute hermes tests patch
var isLoginBackendEnabledFn = IsLoginBackendEnabled

// Local first, then every detected external manager the user has not turned off, then the enabled plugin backends
func EnabledLoginBackends(ctx context.Context) ([]LoginBackend, error) {
	cfg, err := vaultCfgFn(ctx)
	if err != nil {
		return nil, err
	}
	out := []LoginBackend{NewLocalLoginBackend(ctx)}
	for _, cls := range externalBackendClasses() {
		enabled, err := isLoginBackendEnabledFn(ctx, cls.name)
		if err != nil {
			return nil, err
		}
		if !enabled {
			continue
		}
		section, _ := cfg[cls.name].(map[string]any)
		if section == nil {
			section = map[string]any{}
		}
		out = append(out, cls.new(ctx, section))
	}
	return append(out, EnabledPluginLoginBackends(ctx, cfg)...), nil
}

// Enabled backend lister, the module attribute hermes tests patch
var enabledLoginBackendsFn = EnabledLoginBackends

// The enabled backend whose namespace a handle carries, nil when none owns it
func LoginBackendForHandle(ctx context.Context, handle string) (LoginBackend, error) {
	backends, err := enabledLoginBackendsFn(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range backends {
		if b.Owns(handle) {
			return b, nil
		}
	}
	return nil, nil
}
