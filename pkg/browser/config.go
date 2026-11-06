// Package browser drives a Chromium, Lightpanda, cloud, or attached browser over the Chrome DevTools Protocol
package browser

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Toolset every browser tool registers under
const Toolset = "browser"

// Env.Values key the agent runtime sets to a bool saying screenshots may ride inside tool results
const NativeVisionValueKey = "vision.native"

// Default browser section as the config schema carries it
func ConfigDefaults() map[string]any {
	return map[string]any{
		"inactivity_timeout":          120,
		"command_timeout":             30,
		"headed":                      false,
		"allow_private_urls":          false,
		"engine":                      "auto",
		"executable_path":             "",
		"user_data_dir":               "",
		"chromium_args":               []any{},
		"auto_local_for_private_urls": true,
		"cdp_url":                     "",
		"use_real_profile":            false,
		"real_profile_autoclose":      false,
		"real_profile_pin":            "",
		"dialog_policy":               "must_respond",
		"dialog_timeout_s":            300,
		"camofox":                     map[string]any{},
	}
}

const (
	defaultInactivityTimeout = 120
	defaultCommandTimeout    = 30
)

// Valid local engines, chrome reads as auto
var validEngines = map[string]bool{"auto": true, "lightpanda": true}

// Managed Nous gateway selection name served by the Browser Use provider
const nousManagedProvider = "nous"

// Dialog policies the supervisor accepts
const (
	DialogPolicyMustRespond = "must_respond"
	DialogPolicyAutoDismiss = "auto_dismiss"
	DialogPolicyAutoAccept  = "auto_accept"
	DefaultDialogPolicy     = DialogPolicyMustRespond
	DefaultDialogTimeoutS   = 300.0
)

var validDialogPolicies = map[string]bool{DialogPolicyMustRespond: true, DialogPolicyAutoDismiss: true, DialogPolicyAutoAccept: true}

// settings reads the browser section of one session's config
type settings struct {
	env *tools.Env
}

func cfgOf(env *tools.Env) settings { return settings{env: env} }

func (s settings) section() config.Tree {
	if s.env == nil || s.env.Config == nil {
		return config.Tree{}
	}
	return s.env.Config.Sub("browser")
}

func (s settings) logger() *slog.Logger {
	if s.env != nil && s.env.Log != nil {
		return s.env.Log
	}
	return slog.Default()
}

// Secret from the home's .env, then the process environment
func (s settings) secret(name string) string {
	if s.env != nil && s.env.Secrets != nil {
		return strings.TrimSpace(s.env.Secrets.String(name))
	}
	return strings.TrimSpace(os.Getenv(name))
}

func (s settings) homeDir() string {
	if s.env != nil && s.env.Home != "" {
		return s.env.Home
	}
	return home.Process()
}

func (s settings) homeKey() string { return home.Key(s.homeDir()) }

// Chromium launch timeout floor in seconds, never under five
func (s settings) commandTimeout() int {
	v := s.section().Int("command_timeout", defaultCommandTimeout)
	if v < 5 {
		return 5
	}
	return v
}

// Idle seconds before a session is reaped, never under thirty, BROWSER_INACTIVITY_TIMEOUT as the legacy fallback
func (s settings) inactivityTimeout() int {
	fallback := defaultInactivityTimeout
	if raw := s.secret("BROWSER_INACTIVITY_TIMEOUT"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			fallback = n
		}
	}
	v := s.section().Int("inactivity_timeout", fallback)
	if v < 30 {
		return 30
	}
	return v
}

// Extra Chromium flags appended to every packaged or shared profile launch
func (s settings) chromiumArgs() []string {
	raw := s.section().Get("chromium_args")
	var parts []string
	switch v := raw.(type) {
	case string:
		parts = strings.Split(strings.ReplaceAll(v, "\n", ","), ",")
	default:
		parts = config.AsStrings(v)
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Pinned Chromium binary, empty for the packaged one
func (s settings) executablePath() string {
	return strings.TrimSpace(s.section().String("executable_path", ""))
}

// Persistent profile dir shared by every session, tilde expanded and relative paths anchored at the home
func (s settings) userDataDir() string {
	raw := strings.TrimSpace(s.section().String("user_data_dir", ""))
	if raw == "" {
		return ""
	}
	raw = expandTilde(raw)
	if filepath.IsAbs(raw) {
		return raw
	}
	return filepath.Join(s.homeDir(), raw)
}

// Expands a leading tilde to the user home
func expandTilde(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if u, err := os.UserHomeDir(); err == nil {
			return u + path[1:]
		}
	}
	return path
}

// Local engine, auto for Chromium or lightpanda, with chrome and unknown values read as auto
func (s settings) engine() string {
	engine := strings.ToLower(strings.TrimSpace(s.section().String("engine", "auto")))
	if engine == "" || engine == "chrome" {
		return "auto"
	}
	if !validEngines[engine] {
		s.logger().Warn("browser: unknown engine, using auto", "engine", engine)
		return "auto"
	}
	return engine
}

func (s settings) usingLightpanda() bool { return s.engine() == "lightpanda" }

func (s settings) headed() bool { return truthy(s.section().Get("headed")) }

func (s settings) autoLocalForPrivateURLs() bool {
	return s.section().Bool("auto_local_for_private_urls", true)
}

// Real profile consent, read on every call
func (s settings) useRealProfile() bool { return pyrepr.Truthy(s.section().Get("use_real_profile")) }

func (s settings) realProfileAutoclose() bool {
	return truthy(s.section().Get("real_profile_autoclose"))
}

func (s settings) realProfilePin() string {
	return strings.TrimSpace(s.section().String("real_profile_pin", ""))
}

// Private address access granted by browser.allow_private_urls
func (s settings) allowPrivateURLs() bool { return truthy(s.section().Get("allow_private_urls")) }

// Configured cdp_url without any network resolution
func (s settings) configuredCDPURL() string {
	return strings.TrimSpace(s.section().String("cdp_url", ""))
}

// Stored cloud provider selection, present reports whether the key was written at all
func (s settings) cloudProviderSelection() (string, bool) {
	sec := s.section()
	if !sec.Has("cloud_provider") {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(sec.String("cloud_provider", ""))), true
}

// Reports whether the Nous gateway flag or picker row selects the managed browser
func (s settings) useGateway() bool {
	if truthy(s.section().Get("use_gateway")) {
		return true
	}
	sel, _ := s.cloudProviderSelection()
	return sel == nousManagedProvider
}

// Dialog policy and timeout with the supervisor defaults for absent or invalid values
func (s settings) dialogPolicyConfig() (string, float64) {
	policy := strings.TrimSpace(s.section().String("dialog_policy", DefaultDialogPolicy))
	if policy == "" {
		policy = DefaultDialogPolicy
	}
	if !validDialogPolicies[policy] {
		s.logger().Debug("browser: invalid dialog_policy, using default", "policy", policy)
		policy = DefaultDialogPolicy
	}
	timeout := s.section().Float("dialog_timeout_s", DefaultDialogTimeoutS)
	if timeout <= 0 {
		timeout = DefaultDialogTimeoutS
	}
	return policy, timeout
}

// Agent image input mode, auto, native or text
func (s settings) imageInputMode() string {
	if s.env == nil || s.env.Config == nil {
		return "auto"
	}
	mode := strings.ToLower(strings.TrimSpace(s.env.Config.String("agent.image_input_mode", "auto")))
	switch mode {
	case "native", "text":
		return mode
	}
	return "auto"
}

// Reports whether screenshots ride inside this tool's results for the session's main model
func (s settings) nativeVision() bool {
	switch s.imageInputMode() {
	case "native":
		return true
	case "text":
		return false
	}
	if s.env != nil && s.env.Values != nil {
		if v, ok := s.env.Values.Load(NativeVisionValueKey); ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	if s.env != nil && s.env.Config != nil {
		vision := s.env.Config.Sub("auxiliary").Sub("vision")
		provider := strings.ToLower(strings.TrimSpace(vision.String("provider", "auto")))
		if (provider != "" && provider != "auto") || strings.TrimSpace(vision.String("model", "")) != "" {
			return false
		}
	}
	return s.env != nil && s.env.Aux == nil
}

// Embed byte budget for a screenshot baked into history, clamped to 64 KiB through 4 MiB
func (s settings) embedTargetBytes() int {
	const def, min, max = 256 * 1024, 64 * 1024, 4 * 1024 * 1024
	if s.env == nil || s.env.Config == nil {
		return def
	}
	raw := s.env.Config.Get("vision.embed_target_bytes")
	if _, isBool := raw.(bool); isBool {
		return def
	}
	n, ok := config.AsInt(raw)
	if !ok {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

// Shared truthy reading of config scalars and strings
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "1", "true", "yes", "on", "y", "t":
			return true
		}
		return false
	}
	return config.Truthy(v)
}

// Configured CDP override without network traffic, live connect over BROWSER_CDP_URL over browser.cdp_url
func (s settings) cdpOverrideRaw() string {
	if live := LiveCDPOverride(s.homeKey()); live != "" {
		return live
	}
	if envURL := s.secret("BROWSER_CDP_URL"); envURL != "" {
		return envURL
	}
	return s.configuredCDPURL()
}

// Resolved CDP override websocket URL, discovering it over HTTP when needed
func (s settings) cdpOverride(ctx context.Context) string {
	raw := s.cdpOverrideRaw()
	if raw == "" {
		return ""
	}
	return ResolveCDPOverride(ctx, raw)
}
