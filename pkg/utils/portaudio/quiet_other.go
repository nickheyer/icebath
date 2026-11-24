//go:build !unix

package portaudioutil

// Runs a PortAudio call as is: outside Unix the PortAudio host APIs print diagnostics only from debug builds, which also export the debug print hook installLogHooks routes into the log
func quietly(call func()) { call() }
