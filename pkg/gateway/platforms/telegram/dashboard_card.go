package telegram

import (
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

var (
	botTokenRe = mustFullmatch(`\d+:[A-Za-z0-9_-]{30,}`)
	userIDRe   = mustFullmatch(`\d+`)
)

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

// Write checks that reject Telegram bot tokens and user ids pasted into the wrong dashboard channel field
var envValueRules = map[string]gateway.EnvValueRule{
	"TELEGRAM_BOT_TOKEN": {
		Check:   func(v string) bool { return fullmatch(botTokenRe, v) },
		Message: "Telegram bot token must be the complete token from @BotFather, such as 123456789:ABC…",
	},
	"TELEGRAM_ALLOWED_USERS": {
		Check: func(v string) bool {
			for _, u := range strings.Split(v, ",") {
				if s := pystr.Strip(u); s != "" && !fullmatch(userIDRe, s) {
					return false
				}
			}
			return true
		},
		Message: "Telegram allowed users must be comma-separated numeric user IDs.",
	},
}

// Sets the channels page card description, docs link, env vars and env value rules on the registry entry
func applyDashboardCard(e *gateway.PlatformEntry) {
	e.DashboardDescription = "Run Icebath from Telegram DMs, groups, and topics."
	e.DocsURL = "https://core.telegram.org/bots/features#botfather"
	e.DashboardEnvVars = []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_ALLOWED_USERS", "TELEGRAM_PROXY"}
	e.EnvValueRules = envValueRules
}
