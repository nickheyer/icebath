package gatewayrun

import (
	"context"
	"reflect"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

// Gateway config with one enabled platform whose home channel is the chat id
func homeOrderConfig(platform gateway.Platform, chatID string) *gateway.GatewayConfig {
	cfg := gateway.DefaultGatewayConfig(context.Background())
	pc := gateway.NewPlatformConfig()
	pc.Enabled = true
	pc.Token = "***"
	pc.HomeChannel = &gateway.HomeChannel{Platform: platform, ChatID: chatID, Name: chatID}
	cfg.SetPlatform(platform, pc)
	return cfg
}

// Served profiles are walked in the order they were configured, launch first, never by name
func TestServedHomeChannelsWalkProfilesInConfigOrder(t *testing.T) {
	r, _ := makeRestartRunner(t)
	r.config = homeOrderConfig(gateway.PlatformTelegram, "launch-home")
	r.profileConfigs = pydict.New[string, *gateway.GatewayConfig]()
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		r.profileConfigs.Set(name, homeOrderConfig(gateway.PlatformTelegram, name+"-home"))
		r.profileAdapters.Set(name, adapterDict(gateway.PlatformTelegram, newRestartTestAdapter()))
	}
	want := []string{"launch-home", "zeta-home", "alpha-home", "mid-home"}
	var transports []string
	for _, target := range r.servedHomeChannelTransports(context.Background()) {
		transports = append(transports, target.home.ChatID)
	}
	if !reflect.DeepEqual(transports, want) {
		t.Fatalf("transports walked %v, want %v", transports, want)
	}
	var configs []string
	for _, served := range r.servedHomeChannelConfigs() {
		configs = append(configs, served.cfg.HomeChannel.ChatID)
	}
	if !reflect.DeepEqual(configs, want) {
		t.Fatalf("configs walked %v, want %v", configs, want)
	}
	r.profileConfigs.Delete("alpha")
	r.profileConfigs.Set("alpha", homeOrderConfig(gateway.PlatformTelegram, "alpha-home"))
	transports = nil
	for _, target := range r.servedHomeChannelTransports(context.Background()) {
		transports = append(transports, target.home.ChatID)
	}
	if want := []string{"launch-home", "zeta-home", "mid-home", "alpha-home"}; !reflect.DeepEqual(transports, want) {
		t.Fatalf("after a profile was served again transports walked %v, want %v", transports, want)
	}
}
