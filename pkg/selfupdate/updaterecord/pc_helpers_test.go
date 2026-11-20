package updaterecord

import (
	"bufio"
	"fmt"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const pcModeEnv = "ICEBATH_PAUSE_TEST_CHILD"

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	if mode := os.Getenv(pcModeEnv); mode != "" {
		os.Exit(pcChild(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func pcPrint(text string) { fmt.Fprintln(os.Stdout, text) }

func pcWaitFile(path string) {
	for !pathExists(path) {
		time.Sleep(10 * time.Millisecond)
	}
}

func pcCommand(mode string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), pcModeEnv+"="+mode)
	return cmd
}

type pcProc struct {
	cmd *exec.Cmd
	out *bufio.Reader
}

func pcStart(t *testing.T, mode string, args ...string) *pcProc {
	t.Helper()
	cmd := pcCommand(mode, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return &pcProc{cmd: cmd, out: bufio.NewReader(stdout)}
}

func (p *pcProc) line(t *testing.T) string {
	t.Helper()
	line, _ := p.out.ReadString('\n')
	return strings.TrimSpace(line)
}

func (p *pcProc) rest(t *testing.T) (string, int) {
	t.Helper()
	all, _ := io.ReadAll(p.out)
	_ = p.cmd.Wait()
	return string(all), p.cmd.ProcessState.ExitCode()
}

func (p *pcProc) kill(t *testing.T) {
	t.Helper()
	_ = p.cmd.Process.Signal(syscall.SIGKILL)
	_ = p.cmd.Wait()
}
