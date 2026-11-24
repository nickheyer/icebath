// Package browser drives a Chromium, Lightpanda, cloud, or attached browser over the Chrome DevTools Protocol
package browser

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/settings"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Toolset every browser tool registers under
const Toolset = "browser"

// Env.Values key the agent runtime sets to a bool saying screenshots may ride inside tool results
const NativeVisionValueKey = "vision.native"

// Floors the browser applies to its configured waits, in seconds
const (
	minCommandTimeout    = 5
	minInactivityTimeout = 30
)

// Dialog policies the supervisor accepts
const (
	DialogPolicyMustRespond = "must_respond"
	DialogPolicyAutoDismiss = "auto_dismiss"
	DialogPolicyAutoAccept  = "auto_accept"
)

var validDialogPolicies = map[string]bool{DialogPolicyMustRespond: true, DialogPolicyAutoDismiss: true, DialogPolicyAutoAccept: true}

// Reader of the browser settings of the profile one tool env is bound to
type browserConf struct {
	env *tools.Env
}

func cfgOf(env *tools.Env) browserConf { return browserConf{env: env} }

// Context the settings of the env's profile are read under
func (s browserConf) ctx() context.Context { return tools.WithEnvHome(context.Background(), s.env) }

// Logger of the tool env or the process default, named after this package
func (s browserConf) logger() *slog.Logger {
	if s.env != nil && s.env.Log != nil {
		return s.env.Log.With("logger", logName)
	}
	return pkgLog()
}

// Credential from the env's secret resolver, else the profile's secret scope, .env and environment, whitespace trimmed
func (s browserConf) secret(name string) string { return strings.TrimSpace(s.rawSecret(name)) }

// Credential as the profile holds it, whitespace kept
func (s browserConf) rawSecret(name string) string {
	if s.env != nil && s.env.Secrets != nil {
		return s.env.Secrets.String(name)
	}
	v, _ := config.ProfileEnvLookup(s.ctx(), s.homeDir())(name)
	return v
}

func (s browserConf) homeDir() string {
	if s.env != nil && s.env.Home != "" {
		return s.env.Home
	}
	return home.Process()
}

func (s browserConf) homeKey() string { return home.Key(s.homeDir()) }

// Whole seconds of a duration
func wholeSeconds(d time.Duration) int { return int(d / time.Second) }

// Chromium launch timeout in seconds, never under five
func (s browserConf) commandTimeout() int {
	return max(wholeSeconds(settings.BrowserCommandTimeout.Get(s.ctx())), minCommandTimeout)
}

// Idle seconds before a session is reaped, never under thirty
func (s browserConf) inactivityTimeout() int {
	return max(wholeSeconds(settings.BrowserInactivityTimeout.Get(s.ctx())), minInactivityTimeout)
}

// Extra Chromium flags appended to every packaged or shared profile launch, one per item or per line of an item
func (s browserConf) chromiumArgs() []string {
	var out []string
	for _, item := range settings.BrowserChromiumArgs.Strings(s.ctx()) {
		for _, line := range strings.Split(item, "\n") {
			if t := strings.TrimSpace(line); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

// Pinned Chromium binary, empty for the packaged one
func (s browserConf) executablePath() string {
	return strings.TrimSpace(settings.BrowserExecutablePath.Get(s.ctx()))
}

// Persistent profile dir shared by every session, tilde expanded and relative paths anchored at the home
func (s browserConf) userDataDir() string {
	raw := strings.TrimSpace(settings.BrowserUserDataDir.Get(s.ctx()))
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

// Local engine, auto for Chromium or lightpanda, chrome naming Chromium as auto does
func (s browserConf) engine() string {
	if engine := settings.BrowserEngine.Get(s.ctx()); engine == "lightpanda" {
		return engine
	}
	return "auto"
}

func (s browserConf) usingLightpanda() bool { return s.engine() == "lightpanda" }

func (s browserConf) headed() bool { return settings.BrowserHeaded.Get(s.ctx()) }

func (s browserConf) autoLocalForPrivateURLs() bool {
	return settings.BrowserAutoLocalForPrivateUrls.Get(s.ctx())
}

// Real profile consent, read on every call
func (s browserConf) useRealProfile() bool { return settings.BrowserUseRealProfile.Get(s.ctx()) }

// Private address access granted by browser.allow_private_urls
func (s browserConf) allowPrivateURLs() bool { return settings.BrowserAllowPrivateUrls.Get(s.ctx()) }

// Cloud provider the profile chose, lower case, and whether it chose one at all; unchosen auto detects from credentials
func (s browserConf) cloudProviderSelection() (string, bool) {
	ctx := s.ctx()
	chosen, err := settings.Resolve(ctx, settings.BrowserCloudProvider.Key)
	if err != nil || chosen.Source == settings.SourceDefault {
		return "", false
	}
	return strings.ToLower(strings.TrimSpace(settings.BrowserCloudProvider.Get(ctx))), true
}

// Dialog policy and timeout in seconds, a timeout of zero or less taking the setting's default
func (s browserConf) dialogPolicyConfig() (string, float64) {
	ctx := s.ctx()
	timeout := settings.BrowserDialogTimeoutS.Get(ctx)
	if timeout <= 0 {
		timeout = settings.BrowserDialogTimeoutS.Default
	}
	return settings.BrowserDialogPolicy.Get(ctx), timeout.Seconds()
}

// Configured CDP override without network traffic, a live connect over browser.cdp_url and its BROWSER_CDP_URL override
func (s browserConf) cdpOverrideRaw() string {
	if live := liveCDPOverride(s.env); live != "" {
		return live
	}
	return strings.TrimSpace(settings.BrowserCdpUrl.Get(s.ctx()))
}

// Resolved CDP override websocket URL, discovering it over HTTP when needed
func (s browserConf) cdpOverride(ctx context.Context) string {
	raw := s.cdpOverrideRaw()
	if raw == "" {
		return ""
	}
	return ResolveCDPOverride(ctx, raw)
}
