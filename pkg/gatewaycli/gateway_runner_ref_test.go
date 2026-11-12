package gatewaycli

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
)

type cronRunnerDouble struct{}

var _ cron.GatewayRunner = cronRunnerDouble{}

func (cronRunnerDouble) AdaptersForProfile(*string) *pydict.Dict[gateway.Platform, gateway.PlatformAdapter] {
	return nil
}
func (cronRunnerDouble) IsSharedBotSatellite(context.Context, string, gateway.Platform) bool {
	return false
}
func (cronRunnerDouble) GatewayLoop() gateway.EventLoop { return nil }

func TestCronSeesTheGatewayRunnerOfThisProcess(t *testing.T) {
	if cronGatewayRunner() != nil {
		t.Fatal("runner outside a gateway")
	}
	gateway.SetGatewayRunnerRef(cronRunnerDouble{})
	defer gateway.SetGatewayRunnerRef(nil)
	if _, ok := cronGatewayRunner().(cronRunnerDouble); !ok {
		t.Fatal("cron did not see the runner")
	}
}
