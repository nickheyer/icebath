package provider

import "github.com/nickheyer/icebath/pkg/tools"

// Reports a gateway, cron, single query or unattended session where no terminal sign in can run
func pluginOAuthUnattended() bool {
	return tools.NoUserCanAnswer(nil) || tools.IsGatewayApprovalContext(nil)
}
