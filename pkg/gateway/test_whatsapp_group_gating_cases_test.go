package gateway_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	_ "github.com/nickheyer/icebath/pkg/gateway/platforms/whatsapp"
)

// Unsets each variable for the case, restoring its value after
func unsetEnvForCase(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// Fresh home holding the config.yaml text, selected through ICEBATH_HOME
func whatsappConfigHome(t *testing.T, yaml string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func TestConfigBridgesWhatsappDmAndGroupPolicy(t *testing.T) {
	gateway.UseCompiledTreeForTest(t)
	whatsappConfigHome(t, "whatsapp:\n"+
		"  dm_policy: disabled\n"+
		"  group_policy: allowlist\n"+
		"  group_allow_from:\n"+
		"    - \"120363001234567890@g.us\"\n")
	unsetEnvForCase(t, "WHATSAPP_DM_POLICY", "WHATSAPP_GROUP_POLICY", "WHATSAPP_GROUP_ALLOWED_USERS")

	config, err := gateway.LoadGatewayConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if config == nil {
		t.Fatal("no config")
	}
	wa := config.Platforms[gateway.PlatformWhatsApp]
	if wa == nil {
		t.Fatal("whatsapp platform missing")
	}
	if wa.Extra["dm_policy"] != "disabled" {
		t.Fatalf("dm_policy %v", wa.Extra["dm_policy"])
	}
	if wa.Extra["group_policy"] != "allowlist" {
		t.Fatalf("group_policy %v", wa.Extra["group_policy"])
	}
	if !reflect.DeepEqual(wa.Extra["group_allow_from"], []any{"120363001234567890@g.us"}) {
		t.Fatalf("group_allow_from %#v", wa.Extra["group_allow_from"])
	}
	if got := os.Getenv("WHATSAPP_DM_POLICY"); got != "disabled" {
		t.Fatalf("WHATSAPP_DM_POLICY %q", got)
	}
	if got := os.Getenv("WHATSAPP_GROUP_POLICY"); got != "allowlist" {
		t.Fatalf("WHATSAPP_GROUP_POLICY %q", got)
	}
	if got := os.Getenv("WHATSAPP_GROUP_ALLOWED_USERS"); got != "120363001234567890@g.us" {
		t.Fatalf("WHATSAPP_GROUP_ALLOWED_USERS %q", got)
	}
}
