//go:build !unix

package portaudio

// Runs a call as is: outside Unix the host APIs print only from debug builds, which export the print hook hookLogs routes into the log
func quietly(call func()) { call() }
