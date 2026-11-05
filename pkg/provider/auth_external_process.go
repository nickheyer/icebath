package provider

import "github.com/nickheyer/icebath/pkg/config"

// Installs the launch env names as the config env writer denylist hook
func init() { config.EnvWriterLaunchDenylist = EnvWriterLaunchDenylist }

// EnvWriterLaunchDenylist is every external process launch env name plus every left core row's launch env
func EnvWriterLaunchDenylist() map[string]bool {
	out := ExternalProcessLaunchEnvNames()
	for name := range leftCoreLaunchEnvNames() {
		out[name] = true
	}
	return out
}
