package slack

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestSlackDashboardCardRules(t *testing.T) {
	var e gateway.PlatformEntry
	applyDashboardCard(&e)
	if e.DocsURL != "https://api.slack.com/apps" || len(e.DashboardEnvVars) != 3 || len(e.EnvValueRules) != 3 {
		t.Fatalf("card %+v", e)
	}
	for _, c := range []struct {
		env, value string
		ok         bool
	}{
		{"SLACK_BOT_TOKEN", "xoxb-1", true}, {"SLACK_BOT_TOKEN", "xapp-1", false},
		{"SLACK_APP_TOKEN", "xapp-1", true}, {"SLACK_APP_TOKEN", "xoxb-1", false},
		{"SLACK_ALLOWED_USERS", "U01ABC2DEF3, W123 ,", true}, {"SLACK_ALLOWED_USERS", "*", true},
		{"SLACK_ALLOWED_USERS", ",,", true}, {"SLACK_ALLOWED_USERS", "U1", false},
		{"SLACK_ALLOWED_USERS", "u01abc", false}, {"SLACK_ALLOWED_USERS", "U01ABC,alice", false},
	} {
		if got := e.EnvValueRules[c.env].Check(c.value); got != c.ok {
			t.Fatalf("%s=%q -> %v", c.env, c.value, got)
		}
	}
	if e.EnvValueRules["SLACK_ALLOWED_USERS"].Message != "Slack allowed user IDs must be comma-separated member IDs like U01ABC2DEF3." {
		t.Fatal(e.EnvValueRules["SLACK_ALLOWED_USERS"].Message)
	}
}
