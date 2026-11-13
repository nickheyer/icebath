package bundled

// Links every bundled platform package so its init registers the platform plugin and adapter
import (
	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/a2a"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/bluebubbles"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/buzz"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/dingtalk"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/discord"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/email"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/feishu"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/googlechat"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/irc"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/line"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/matrix"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/mattermost"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/ntfy"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/photon"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/qqbot"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/raft"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/signal"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/simplex"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/slack"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/sms"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/teams"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/telegram"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/wecom"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/weixin"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/whatsappcloud"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/yuanbao"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Gives the bundled platform plugins the gateway registry to load into until the process wiring installs every service
func init() {
	if plugins.InstalledServices() == nil {
		plugins.InstallServices(&plugins.Services{Platforms: gateway.PlatformRegistryService()})
	}
}
