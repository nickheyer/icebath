package telegram

import (
	"context"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Platform adapter double owning a real base so base auth and source helpers run unmodified
type shellAdapter struct{ *gateway.BasePlatformAdapter }

func (s *shellAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (s *shellAdapter) Disconnect(context.Context) error            { return nil }
func (s *shellAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{Success: true}, nil
}
func (s *shellAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return map[string]any{}, nil
}

// Adapter core over a fresh Telegram base with the token config of the hermes tests
func newTestCore() *adapterCore {
	cfg := gateway.NewPlatformConfig()
	cfg.Enabled, cfg.Token = true, "fake-token"
	shell := &shellAdapter{}
	shell.BasePlatformAdapter = gateway.NewBasePlatformAdapter(cfg, gateway.PlatformTelegram, shell)
	return &adapterCore{base: shell.BasePlatformAdapter}
}
