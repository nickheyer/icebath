package gateway_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/buzz"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/dingtalk"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/discord"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/feishu"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/googlechat"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/irc"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/line"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/matrix"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/mattermost"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/raft"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/simplex"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/teams"
	"github.com/nickheyer/icebath/pkg/gateway/platforms/wecom"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Every platform wizard and the primary env var its shared reconfigure gate keys on
var gatedWizards = []struct {
	id, envVar string
	setup      func(context.Context) error
}{
	{"buzz", "BUZZ_RELAY_URL", buzz.InteractiveSetup},
	{"dingtalk", "DINGTALK_CLIENT_ID", dingtalk.InteractiveSetup},
	{"discord", "DISCORD_BOT_TOKEN", discord.InteractiveSetup},
	{"feishu", "FEISHU_APP_ID", feishu.InteractiveSetup},
	{"google_chat", "GOOGLE_CHAT_SUBSCRIPTION_NAME", googlechat.InteractiveSetup},
	{"irc", "IRC_SERVER", irc.InteractiveSetup},
	{"line", "LINE_CHANNEL_ACCESS_TOKEN", line.InteractiveSetup},
	{"matrix", "MATRIX_PASSWORD", matrix.InteractiveSetup},
	{"mattermost", "MATTERMOST_TOKEN", mattermost.InteractiveSetup},
	{"raft", "RAFT_PROFILE", raft.InteractiveSetup},
	{"simplex", "SIMPLEX_WS_URL", simplex.InteractiveSetup},
	{"slack", "SLACK_BOT_TOKEN", slack.InteractiveSetup},
	{"teams", "TEAMS_CLIENT_ID", teams.InteractiveSetup},
	{"wecom", "WECOM_BOT_ID", wecom.InteractiveSetup},
}

func TestDecliningReconfigureGoesThroughSharedGate(t *testing.T) {
	for _, w := range gatedWizards {
		t.Run(w.id, func(t *testing.T) {
			t.Setenv("ICEBATH_HOME", t.TempDir())
			t.Setenv(w.envVar, "already-set")
			var out bytes.Buffer
			t.Cleanup(provider.WithSetupOutput(&out))
			savedSetup, savedCLI, savedConfig, savedPlatform := gateway.WizardSetup, gateway.WizardCLI, gateway.WizardConfig, gateway.PlatformSetup
			t.Cleanup(func() {
				gateway.WizardSetup, gateway.WizardCLI, gateway.WizardConfig, gateway.PlatformSetup = savedSetup, savedCLI, savedConfig, savedPlatform
			})
			var gated [][]string
			realGate := gateway.PlatformSetup.DeclinesReconfigure
			gateway.PlatformSetup.DeclinesReconfigure = func(ctx context.Context, label, question string, envVars ...string) (bool, error) {
				gated = append(gated, append([]string(nil), envVars...))
				return realGate(ctx, label, question, envVars...)
			}
			noSave := func(context.Context, string, string) error {
				t.Fatal("wizard persisted env after the user declined to reconfigure")
				return nil
			}
			noPrompt := func(string, string, bool) string {
				t.Fatal("wizard fell through to its own prompts after the user declined")
				return ""
			}
			gateway.WizardSetup.PromptYesNo = func(string, bool) bool { return false }
			gateway.WizardCLI.PromptYesNo = func(context.Context, string, bool) bool { return false }
			gateway.WizardSetup.Prompt, gateway.WizardCLI.Prompt = noPrompt, noPrompt
			gateway.WizardSetup.SaveEnvValue, gateway.WizardConfig.SaveEnvValue = noSave, noSave
			if err := w.setup(context.Background()); err != nil {
				t.Fatal(err)
			}
			found := false
			if len(gated) > 0 {
				for _, v := range gated[0] {
					found = found || v == w.envVar
				}
			}
			if !found {
				t.Fatalf("%s: gate not routed through declines_reconfigure: %v", w.id, gated)
			}
		})
	}
}
