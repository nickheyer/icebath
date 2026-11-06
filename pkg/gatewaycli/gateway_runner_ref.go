package gatewaycli

import (
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
)

// Running gateway runner of this process as cron tools drive it, nil outside a gateway
func cronGatewayRunner() cron.GatewayRunner {
	runner, _ := gateway.CurrentGatewayRunner().(cron.GatewayRunner)
	return runner
}

func init() { cron.SetGatewayRunnerRef(cronGatewayRunner) }
