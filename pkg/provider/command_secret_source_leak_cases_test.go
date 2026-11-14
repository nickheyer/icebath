//go:build !windows

package provider

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Runs fn with stdout and stderr swapped for pipes and returns everything written to both
func captureStdStreams(t *testing.T, fn func()) string {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prevOut, prevErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	collected := make(chan string, 2)
	for _, r := range []*os.File{outR, errR} {
		go func(r *os.File) {
			data, _ := io.ReadAll(r)
			collected <- string(data)
		}(r)
	}
	fn()
	os.Stdout, os.Stderr = prevOut, prevErr
	_ = outW.Close()
	_ = errW.Close()
	return <-collected + <-collected
}

func TestFailureLoggingNeverLeaksCommandOrSecret(t *testing.T) {
	secretValue := "sk-super-secret-value-do-not-log"
	helper := writeHelper(t, "echo '"+secretValue+"' >&2\nexit 7", "my-distinctive-helper-name.sh")
	var value string
	var ok bool
	combined := captureStdStreams(t, func() {
		value, ok = runSecretHelper(helper, "CMDTEST_API_KEY", 3*time.Second, 1024, ProcessSourceEnvironment())
	})
	if ok || value != "" {
		t.Fatalf("helper failure resolved %q %v", value, ok)
	}
	if strings.Contains(combined, "my-distinctive-helper-name") || strings.Contains(combined, secretValue) {
		t.Fatalf("failure log leaked the command or the secret %q", combined)
	}
	if !strings.Contains(combined, "code=7") {
		t.Fatalf("structured exit code missing from %q", combined)
	}
}
