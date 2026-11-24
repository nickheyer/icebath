package feishu

import "fmt"

// Warning for a group drop caused by the untouched empty allowlist default, false for an operator configured deny
func emptyAllowlistDropWarning(chatID string, groupRules map[string]any, groupPolicy string, allowedGroupUsers []string) (string, bool) {
	if _, ok := groupRules[chatID]; ok || groupPolicy != "allowlist" || len(allowedGroupUsers) > 0 {
		return "", false
	}
	return fmt.Sprintf("[Feishu] Dropped a group message in chat %s: group policy is 'allowlist' and "+
		"feishu.allowed_users is empty for this profile, so every human group message is rejected. "+
		"Set feishu.group_policy to open, list open IDs under feishu.allowed_users, or give the chat a rule "+
		"under feishu.group_rules, in this profile's config.yaml (FEISHU_GROUP_POLICY and FEISHU_ALLOWED_USERS "+
		"in its .env work too; each profile reads only its own settings and .env). "+
		"Further drops are logged at DEBUG.", chatID), true
}
