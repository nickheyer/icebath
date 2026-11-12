package feishu

import "github.com/nickheyer/icebath/pkg/gateway"

// Sets the channels page card description, docs link and env vars on the registry entry
func applyDashboardCard(e *gateway.PlatformEntry) {
	e.DashboardDescription = "Use Icebath inside Feishu / Lark."
	e.DocsURL = "https://open.feishu.cn/document/uAjLw4CM/ukTMukTMukTM/reference/im-v1/intro"
	e.DashboardEnvVars = []string{"FEISHU_APP_ID", "FEISHU_APP_SECRET", "FEISHU_ENCRYPT_KEY", "FEISHU_VERIFICATION_TOKEN"}
}

// Registers the card of the bundled feishu directory for dashboards whose registry holds no feishu entry
func init() { gateway.RegisterBundledDashboardCard("feishu", applyDashboardCard) }
