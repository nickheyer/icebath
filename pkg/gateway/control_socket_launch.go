package gateway

import (
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// What launched the gateway behind an identify answer, empty when it only says none or nothing usable
func LaunchSupervisor(identity *jsonutil.Object) string {
	if identity == nil {
		return ""
	}
	for _, key := range []string{"launch_supervisor", "supervisor"} {
		if value, _ := identity.Get(key); value != nil {
			if text, isStr := value.(string); isStr && text != "" && text != "none" {
				return text
			}
		}
	}
	return ""
}
