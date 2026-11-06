package provider

import (
	"os/signal"
	"syscall"
	"testing"
)

func TestInstallSafeStdioIgnoresBrokenPipeSignals(t *testing.T) {
	InstallSafeStdio()
	if !signal.Ignored(syscall.SIGPIPE) {
		t.Fatal("SIGPIPE is still delivered after InstallSafeStdio")
	}
}
