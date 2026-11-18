package gateway

import "github.com/nickheyer/icebath/pkg/tuihost"

func init() {
	tuihost.GatewayDocker.EnvActive = dockerEnvActive
	tuihost.GatewayDocker.VolumeMounts = parseDockerVolumeMounts
}
