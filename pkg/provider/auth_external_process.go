package provider

// EnvWriterLaunchDenylist is every external process launch env name plus every left core row's launch env
func EnvWriterLaunchDenylist() map[string]bool {
	out := ExternalProcessLaunchEnvNames()
	for name := range leftCoreLaunchEnvNames() {
		out[name] = true
	}
	return out
}
