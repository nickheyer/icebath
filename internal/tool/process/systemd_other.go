//go:build !linux

package process

const isLinux = false

func userBusEnv(env map[string]string) map[string]string { return env }
