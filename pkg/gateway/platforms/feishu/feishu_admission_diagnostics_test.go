package feishu

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestEmptyAllowlistDropWarningOnlyForUntouchedDefault(t *testing.T) {
	msg, ok := emptyAllowlistDropWarning("oc_1", map[string]any{}, "allowlist", nil)
	if !ok || !strings.HasPrefix(msg, "[Feishu] Dropped a group message in chat oc_1: group policy is 'allowlist'") || !strings.HasSuffix(msg, "Further drops are logged at DEBUG.") {
		t.Fatal(msg)
	}
	for _, tc := range []struct {
		rules  map[string]any
		policy string
		users  []string
	}{{map[string]any{"oc_1": map[string]any{}}, "allowlist", nil}, {nil, "open", nil}, {nil, "allowlist", []string{"ou_1"}}} {
		if _, ok := emptyAllowlistDropWarning("oc_1", tc.rules, tc.policy, tc.users); ok {
			t.Fatalf("%+v warned", tc)
		}
	}
	e := gateway.PlatformEntry{}
	applyDashboardCard(&e)
	if e.DashboardDescription != "Use Icebath inside Feishu / Lark." || len(e.DashboardEnvVars) != 4 || !strings.Contains(platformHint, "Feishu (Lark)") {
		t.Fatalf("%+v", e)
	}
}
