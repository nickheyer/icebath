package tools

import "errors"

// Error a backend requirements check returns when the provider contract answered False
var ErrBackendRequirementsNotMet = errors.New("backend requirements not met")

// Classification and picker defaults of the terminal environment provider contract, dispatching through Self
type TerminalEnvironmentProviderDefaults struct{ Self BackendProvider }

// Outer provider the defaults dispatch to, panicking when the embedding provider never bound itself
func (d TerminalEnvironmentProviderDefaults) self() BackendProvider {
	if d.Self == nil {
		panic("tools.TerminalEnvironmentProviderDefaults.Self is not bound to its provider")
	}
	return d.Self
}

// Label shown in pickers, the provider name
func (d TerminalEnvironmentProviderDefaults) DisplayName() string { return d.self().Name() }

// One line description shown in backend pickers
func (d TerminalEnvironmentProviderDefaults) Description() string {
	return "Run commands in a " + d.self().DisplayName() + " environment."
}

// Prompt builder fallback for where commands run when the live probe fails
func (d TerminalEnvironmentProviderDefaults) EnvDescription() string {
	return "a " + d.self().DisplayName() + " environment (likely Linux)"
}

// Commands run off host unless overridden
func (TerminalEnvironmentProviderDefaults) IsRemote() bool { return true }

// Own filesystem rooted away from the host unless overridden
func (TerminalEnvironmentProviderDefaults) IsContainer() bool { return true }

// Dangerous command approval is skipped exactly when the backend is a container
func (d TerminalEnvironmentProviderDefaults) SkipContainerGuards() bool { return d.self().IsContainer() }

// Host paths remain correct for synced cache files
func (TerminalEnvironmentProviderDefaults) CachePathBase() string { return "" }

// Commands start in the generic root
func (TerminalEnvironmentProviderDefaults) DefaultCwd() string { return "" }

// No vendor credential env vars to strip
func (TerminalEnvironmentProviderDefaults) StripEnvKeys() []string { return nil }

// Ephemeral runs share one sandbox identity
func (TerminalEnvironmentProviderDefaults) SessionIsolatedWhenNonpersistent() bool { return false }

// No exec_prefix stream support
func (TerminalEnvironmentProviderDefaults) SupportsStreams() bool { return false }

// Full requirements check reading as the availability probe
func (d TerminalEnvironmentProviderDefaults) CheckRequirements(*Env, map[string]any) error {
	if d.self().IsAvailable() {
		return nil
	}
	return ErrBackendRequirementsNotMet
}

// Dashboard picker health, ready when available and needs_setup otherwise
func (d TerminalEnvironmentProviderDefaults) Probe() (string, string) {
	if d.self().IsAvailable() {
		return "ready", ""
	}
	return "needs_setup", d.self().DisplayName() + " is not configured."
}

// No lines after selection
func (TerminalEnvironmentProviderDefaults) SetupInstructions(*Env) []string { return []string{} }

// One doctor row reflecting the availability probe, a panicking probe reading as not configured
func (d TerminalEnvironmentProviderDefaults) DoctorChecks() []DoctorCheck {
	ok := func() (ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		return d.self().IsAvailable()
	}()
	detail := "(not configured — see setup instructions)"
	if ok {
		detail = "(configured)"
	}
	return []DoctorCheck{{OK: ok, Label: d.self().DisplayName() + " backend", Detail: detail}}
}
