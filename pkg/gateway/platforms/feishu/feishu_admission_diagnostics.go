package feishu

import "fmt"

// Warning for a group drop caused by the untouched empty allowlist default, false for an operator configured deny
func emptyAllowlistDropWarning(chatID string, groupRules map[string]any, defaultGroupPolicy string, allowedGroupUsers []string) (string, bool) {
	if _, ok := groupRules[chatID]; ok || defaultGroupPolicy != "allowlist" || len(allowedGroupUsers) > 0 {
		return "", false
	}
	return fmt.Sprintf("[Feishu] Dropped a group message in chat %s: group policy is 'allowlist' and "+
		"FEISHU_ALLOWED_USERS is empty for this profile, so every human group message is rejected. "+
		"Set FEISHU_GROUP_POLICY=open or FEISHU_ALLOWED_USERS=<open_id,...> in this profile's .env "+
		"(under multiplex each profile reads only its own .env; the default profile's value is not "+
		"inherited), or platforms.feishu.extra.group_rules in its config.yaml. "+
		"Further drops are logged at DEBUG.", chatID), true
}
