package gateway

import "github.com/nickheyer/icebath/pkg/tuigateway"

func init() {
	tuigateway.GatewayDocker.EnvActive = dockerEnvActive
	tuigateway.GatewayDocker.VolumeMounts = parseDockerVolumeMounts
}
