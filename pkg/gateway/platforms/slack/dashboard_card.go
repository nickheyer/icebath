package slack

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var memberIDRe = mustFullmatch(`[UW][A-Z0-9]{2,}`)

func mustFullmatch(pattern string) *regexp2.Regexp {
	re, err := pyre.CompileFullmatch(pattern)
	if err != nil {
		panic(err)
	}
	return re
}

func fullmatch(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// Write checks that reject Slack tokens and member ids pasted into the wrong dashboard field
var envValueRules = map[string]gateway.EnvValueRule{
	"SLACK_BOT_TOKEN": {
		Check:   func(v string) bool { return strings.HasPrefix(v, "xoxb-") },
		Message: "Slack Bot Token must start with xoxb-. Paste the bot token from OAuth & Permissions.",
	},
	"SLACK_APP_TOKEN": {
		Check:   func(v string) bool { return strings.HasPrefix(v, "xapp-") },
		Message: "Slack App Token must start with xapp-. Paste the app-level token from Basic Information > App-Level Tokens.",
	},
	"SLACK_ALLOWED_USERS": {
		Check: func(v string) bool {
			for _, p := range strings.Split(v, ",") {
				if u := pystr.Strip(p); u != "" && u != "*" && !fullmatch(memberIDRe, u) {
					return false
				}
			}
			return true
		},
		Message: "Slack allowed user IDs must be comma-separated member IDs like U01ABC2DEF3.",
	},
}

// Sets the channels page card description, docs link, env vars and env value rules on the registry entry
func applyDashboardCard(e *gateway.PlatformEntry) {
	e.DashboardDescription = "Use Icebath from Slack via Socket Mode. Add allowed Slack member IDs so connected bots can respond."
	e.DocsURL = "https://api.slack.com/apps"
	e.DashboardEnvVars = []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_ALLOWED_USERS"}
	e.EnvValueRules = envValueRules
}
