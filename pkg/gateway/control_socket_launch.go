package gateway

import "github.com/nickheyer/icebath/pkg/internal/pyjson"

// What launched the gateway behind an identify answer, empty when it only says none or nothing usable
func LaunchSupervisor(identity *pyjson.Object) string {
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
