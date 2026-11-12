package whatsapp

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestPluginRegistersTheWhatsAppPlatform(t *testing.T) {
	h := hermeticHome(t)
	plugins.InstallServices(&plugins.Services{Platforms: gateway.PlatformRegistryService()})
	p, ok := plugins.BundledPlugin("platforms/whatsapp")
	if !ok {
		t.Fatal("whatsapp platform plugin is not compiled in")
	}
	m := p.Manifest()
	if err := p.Register(plugins.NewContext(&m, plugins.NewManager(h))); err != nil {
		t.Fatal(err)
	}
	var entry *gateway.PlatformEntry
	for _, e := range gateway.Registry.AllEntries(context.Background()) {
		if e.Name == "whatsapp" {
			entry = e
		}
	}
	if entry == nil {
		t.Fatal("whatsapp entry missing from the registry")
	}
	if entry.StandaloneSenderFn == nil || entry.ApplyYAMLConfigFn == nil || entry.SetupFn == nil || entry.IsConnected == nil ||
		entry.MaxMessageLength != 4096 || entry.CronDeliverEnvVar != "WHATSAPP_HOME_CHANNEL" || entry.StandaloneSend == nil || !entry.StandaloneSend.Mentions {
		t.Fatalf("entry = %+v", entry)
	}
	adapter, err := entry.AdapterFactory(context.Background(), &gateway.PlatformConfig{Extra: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := adapter.(*WhatsAppAdapter); !ok {
		t.Fatalf("factory built %T", adapter)
	}
	if parsed := entry.TargetRefSyntaxFn("120363001234567890@g.us"); parsed == nil {
		t.Fatal("group JID not accepted as a target")
	}
}
