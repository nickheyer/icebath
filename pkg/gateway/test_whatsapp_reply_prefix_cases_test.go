package gateway_test

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// WhatsApp platform config loaded from a home holding the config.yaml text with WhatsApp enabled
func loadWhatsappFromYaml(t *testing.T, yaml string) *gateway.PlatformConfig {
	t.Helper()
	gateway.UseCompiledTreeForTest(t)
	whatsappConfigHome(t, yaml)
	t.Setenv("WHATSAPP_ENABLED", "true")
	config, err := gateway.LoadGatewayConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wa := config.Platforms[gateway.PlatformWhatsApp]
	if wa == nil {
		t.Fatal("whatsapp platform missing")
	}
	return wa
}

func TestConfigYamlBridgingReplyPrefixBridgedFromYaml(t *testing.T) {
	wa := loadWhatsappFromYaml(t, "whatsapp:\n  reply_prefix: \"Custom Bot\"\n")
	if got, ok := wa.Extra["reply_prefix"]; !ok || got != "Custom Bot" {
		t.Fatalf("reply_prefix %#v", got)
	}
}

func TestConfigYamlBridgingEmptyReplyPrefixBridged(t *testing.T) {
	wa := loadWhatsappFromYaml(t, "whatsapp:\n  reply_prefix: \"\"\n")
	if got, ok := wa.Extra["reply_prefix"]; !ok || got != "" {
		t.Fatalf("reply_prefix %#v present %v", got, ok)
	}
}
