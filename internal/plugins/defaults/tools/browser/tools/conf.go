package tools

import (
	"context"
	"strconv"
	"strings"
	"time"

	browserconfig "github.com/nickheyer/icebath/internal/browser/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/fsx"
)

// Floors of the waits the profile sets
const (
	minCommandTimeout    = 5 * time.Second
	minInactivityTimeout = 30 * time.Second
	defaultDialogTimeout = 300.0
)

func seconds(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// Number of seconds a setting holds, the default for one that is no number
func secondsOf(cfg *config.Config, key string, def float64) float64 {
	raw := strings.TrimSpace(cfg.GetString(key))
	if v, err := strconv.ParseFloat(raw, 64); err == nil {
		return v
	}
	return def
}

// How long the browser may take to start and answer, never under five seconds
func commandTimeout(cfg *config.Config) time.Duration {
	return max(seconds(secondsOf(cfg, browserconfig.CommandTimeout, 30)), minCommandTimeout)
}

// How long an unused session is kept, never under thirty seconds
func inactivity(cfg *config.Config) time.Duration {
	return max(seconds(secondsOf(cfg, browserconfig.InactivityTimeout, 120)), minInactivityTimeout)
}

// Extra flags of every Chromium launch, one per item or per line of an item
func chromiumArgs(cfg *config.Config) []string {
	var out []string
	for _, item := range cfg.GetStringSlice(browserconfig.ChromiumArgs) {
		for _, line := range strings.Split(item, "\n") {
			if t := strings.TrimSpace(line); t != "" {
				out = append(out, t)
			}
		}
	}
	return out
}

func lightpanda(cfg *config.Config) bool {
	return strings.ToLower(strings.TrimSpace(cfg.GetString(browserconfig.Engine))) == browserconfig.Lightpanda
}

func headed(cfg *config.Config) bool          { return cfg.GetBool(browserconfig.Headed) }
func localForPrivate(cfg *config.Config) bool { return cfg.GetBool(browserconfig.LocalForPrivate) }
func realProfile(cfg *config.Config) bool     { return cfg.GetBool(browserconfig.UseRealProfile) }
func privateURLs(cfg *config.Config) bool     { return cfg.GetBool(browserconfig.PrivateURLs) }
func tabMarker(cfg *config.Config) bool       { return cfg.GetBool(browserconfig.TabMarker) }

// What a page's dialog gets and how long it waits for the agent, in seconds
func dialogPolicy(cfg *config.Config) (string, float64) {
	timeout := secondsOf(cfg, browserconfig.DialogTimeout, defaultDialogTimeout)
	if timeout <= 0 {
		timeout = defaultDialogTimeout
	}
	policy := strings.TrimSpace(cfg.GetString(browserconfig.DialogPolicy))
	switch policy {
	case browserconfig.AutoDismiss, browserconfig.AutoAccept:
	default:
		policy = browserconfig.MustRespond
	}
	return policy, timeout
}

// Folder of the profile
func profileDir(cfg *config.Config) string { return cfg.ProfileDir(cfg.Profile()) }

// Folder browser code keeps its files in for every task, ~ expanded; empty for one per task
func execWorkspace(cfg *config.Config) string {
	return fsx.ExpandHome(strings.TrimSpace(cfg.GetString(browserconfig.ExecWorkspace)))
}

// Endpoint the profile attaches to without asking the network: a live /browser connect, else browser.cdp_url
func (r *registrant) overrideRaw(cfg *config.Config) string {
	if live := r.live.get(cfg.Profile()); live != "" {
		return live
	}
	return strings.TrimSpace(cfg.GetString(browserconfig.CDPURL))
}

// Endpoint the profile attaches to as a websocket address, found over HTTP when it names a debugging port
func (r *registrant) override(ctx context.Context, cfg *config.Config) string {
	raw := r.overrideRaw(cfg)
	if raw == "" {
		return ""
	}
	return resolveCDP(ctx, raw)
}
