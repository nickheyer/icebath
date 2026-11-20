//go:build unix

package gatewayrun

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSigusr2StackDumpLeavesTheGatewayRunning(t *testing.T) {
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "ICEBATH_FAULTHANDLER_CHILD="+dir)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	defer func() {
		child.Process.Kill()
		<-exited
	}()
	lines := bufio.NewScanner(stdout)
	ready := false
	for lines.Scan() {
		if strings.TrimSpace(lines.Text()) == "ready" {
			ready = true
			break
		}
	}
	go func() { exited <- child.Wait() }()
	if !ready {
		t.Fatal("child never became ready")
	}
	if err := child.Process.Signal(syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-exited:
		exited <- err
		t.Fatalf("gateway died on SIGUSR2 (%v)", err)
	default:
	}
	dump, err := os.ReadFile(filepath.Join(dir, "logs", "gateway_faulthandler.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dump), "goroutine ") {
		t.Fatal(string(dump))
	}
}
