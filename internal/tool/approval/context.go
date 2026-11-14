package approval

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	envInteractive = "ICEBATH_INTERACTIVE"
	envGateway     = "ICEBATH_GATEWAY_SESSION"
	envExecAsk     = "ICEBATH_EXEC_ASK"
	envCron        = "ICEBATH_CRON_SESSION"
	envSingleQuery = "ICEBATH_SINGLE_QUERY_SESSION"
)

func truthy(v string) bool {
	switch strings.ToLower(pyStrip(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func PresenceFromEnv(getenv func(string) string) tool.Presence {
	return tool.Presence{
		Interactive: truthy(getenv(envInteractive)),
		Gateway:     truthy(getenv(envGateway)),
		Ask:         truthy(getenv(envExecAsk)),
		Cron:        truthy(getenv(envCron)),
		SingleQuery: truthy(getenv(envSingleQuery)),
	}
}

func PresenceEnv(p tool.Presence) []string {
	var out []string
	for _, kv := range []struct {
		name string
		on   bool
	}{{envInteractive, p.Interactive}, {envGateway, p.Gateway}, {envExecAsk, p.Ask}, {envCron, p.Cron}, {envSingleQuery, p.SingleQuery}} {
		if kv.on {
			out = append(out, kv.name+"=1")
		}
	}
	return out
}

var (
	localInteractiveSources = set("cli", "tui", "gui", "acp")
	unattendedPlatforms     = set("webhook", "msgraph_webhook", "api_server")
	notifierBlindPlatforms  = set("webhook", "msgraph_webhook")
)

func localInteractive(o tool.Origin) bool { return localInteractiveSources[o.Source] }

func unattendedPlatform(p tool.Presence, o tool.Origin) bool {
	return p.Unattended || unattendedPlatforms[o.Platform]
}

func notifierBlind(o tool.Origin) bool { return notifierBlindPlatforms[o.Platform] }

func NoUserCanAnswer(c *tool.Call) bool {
	return c.Presence.SingleQuery || c.Presence.Cron || unattendedPlatform(c.Presence, c.Origin)
}

func gatewayContext(c *tool.Call) bool {
	if c.Presence.Cron || unattendedPlatform(c.Presence, c.Origin) {
		return false
	}
	return c.Presence.Gateway || c.Origin.Platform != ""
}

func gatewaySurface(o tool.Origin) string {
	if o.Platform == "cli" {
		return "cli"
	}
	return "gateway"
}

func fallThroughToCLI(cli, prompt, notify bool) bool { return cli && prompt && !notify }

func sessionKey(c *tool.Call) string {
	if c.SessionKey != "" {
		return c.SessionKey
	}
	return "default"
}

func normalizeMode(raw config.ApprovalMode) config.ApprovalMode {
	mode := config.ApprovalMode(strings.ToLower(pyStrip(string(raw))))
	switch mode {
	case config.ApprovalManual, config.ApprovalSmart, config.ApprovalOff:
		return mode
	case "false":
		return config.ApprovalOff
	case "true":
		return config.ApprovalManual
	}
	if mode != "" {
		slog.Warn("Unknown approvals.mode — defaulting to 'manual'. Valid values: manual, smart, off", "mode", strconv.Quote(string(raw)))
	}
	return config.ApprovalManual
}

type approvalsView struct {
	cfg     config.ApprovalsConfig
	loaded  bool
	auxTask config.AuxTask
}

func readApprovals(ctx context.Context, st *config.Store) approvalsView {
	cfg, err := st.LoadReadonly(ctx)
	if err != nil {
		slog.WarnContext(ctx, "Failed to load approval config", "error", err)
		return approvalsView{}
	}
	return approvalsView{cfg: cfg.Approvals, loaded: true, auxTask: cfg.Auxiliary.Approval}
}

func Mode(ctx context.Context, st *config.Store, o tool.Origin) config.ApprovalMode {
	if o.RoomApprovalMode != "" {
		return config.ApprovalMode(o.RoomApprovalMode)
	}
	v := readApprovals(ctx, st)
	if !v.loaded {
		return config.ApprovalManual
	}
	return normalizeMode(v.cfg.Mode)
}

func approvalTimeout(ctx context.Context, st *config.Store) int {
	v := readApprovals(ctx, st)
	if !v.loaded {
		return 300
	}
	raw := v.cfg.Timeout
	safe := int(config.MaxSafeTimeout.Seconds())
	if raw > safe {
		slog.WarnContext(ctx, "approvals.timeout exceeds the platform-safe maximum; clamping", "timeout", raw, "max", safe)
		return safe
	}
	return raw
}

func binaryMode(v approvalsView, pick func(config.ApprovalsConfig) config.UnattendedChoice) bool {
	if !v.loaded {
		return false
	}
	switch strings.ToLower(pyStrip(string(pick(v.cfg)))) {
	case "approve", "off", "allow", "yes":
		return true
	}
	return false
}

func cronApproves(ctx context.Context, st *config.Store) bool {
	return binaryMode(readApprovals(ctx, st), func(a config.ApprovalsConfig) config.UnattendedChoice { return a.CronMode })
}

func singleQueryApproves(ctx context.Context, st *config.Store) bool {
	return binaryMode(readApprovals(ctx, st), func(a config.ApprovalsConfig) config.UnattendedChoice { return a.SingleQueryMode })
}

func unattendedApproves(ctx context.Context, st *config.Store) bool {
	return binaryMode(readApprovals(ctx, st), func(a config.ApprovalsConfig) config.UnattendedChoice { return a.UnattendedMode })
}

func transportSelection(ctx context.Context, st *config.Store) (string, string) {
	cfg, err := st.LoadReadonly(ctx)
	if err != nil {
		return "config-error", ""
	}
	selected := strings.ToLower(pyStrip(cfg.Security.Approval.Transport))
	if selected == "" {
		selected = "builtin"
	}
	if strings.ToLower(pyStrip(cfg.Security.Approval.TransportFallback)) == "builtin" {
		return selected, "builtin"
	}
	return selected, ""
}

func FormatWindow(seconds int, translate func(key string, args map[string]string) string) (string, error) {
	if translate == nil {
		return "", &tool.UnfilledError{Name: "approval message catalog"}
	}
	if seconds < 0 {
		seconds = 0
	}
	count, unit := seconds, "second"
	switch {
	case seconds != 0 && seconds%3600 == 0:
		count, unit = seconds/3600, "hour"
	case seconds != 0 && seconds%60 == 0:
		count, unit = seconds/60, "minute"
	}
	form := "_other"
	if count == 1 {
		form = "_one"
	}
	return translate("approval.window."+unit+form, map[string]string{"count": strconv.Itoa(count)}), nil
}
