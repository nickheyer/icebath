package gatewaycli

import (
	"testing"

	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
)

type cronRunnerDouble struct{}

var _ cron.GatewayRunner = cronRunnerDouble{}

func (cronRunnerDouble) AdaptersForProfile(string) any    { return nil }
func (cronRunnerDouble) IsSharedBotSatellite(string) bool { return false }
func (cronRunnerDouble) GatewayLoop() any                 { return nil }

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
