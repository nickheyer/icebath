package selfupdate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/proc"
)

// Plays a build child when this test binary is started with a child mode in its environment
func init() {
	switch os.Getenv("ICEBATH_UC_CHILD") {
	case "loud":
		fmt.Println("LOUD BUILD OUTPUT")
		os.Exit(0)
	case "progress":
		os.WriteFile(os.Getenv("ICEBATH_UC_READY"), nil, 0o644)
		os.Stdout.Write(append([]byte("progress: "), 0xe2, 0x82))
		time.Sleep(100 * time.Millisecond)
		os.Stderr.Write([]byte{0xac, 0xff})
		for {
			if _, err := os.Stat(os.Getenv("ICEBATH_UC_RELEASE")); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		fmt.Println(" done")
		os.Exit(7)
	case "cancel":
		os.WriteFile(os.Getenv("ICEBATH_UC_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		fmt.Println("started")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}

// Argv and environment that start this test binary as a build child of the given mode
func ucChild(t *testing.T, mode string, extra ...string) ([]string, []string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return []string{self}, append(append(os.Environ(), "ICEBATH_UC_CHILD="+mode), extra...)
}

// Terminal stand-in safe to read while a worker writes it
type ucTerminal struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *ucTerminal) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *ucTerminal) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLogOnlyWritePlainStdoutKeepsBuildOutputOffScreen(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var plain bytes.Buffer
	if err := logOnlyWrite(&plain, "something"); err != nil {
		t.Fatal(err)
	}
	if plain.Len() != 0 {
		t.Fatalf("plain stdout received %q", plain.String())
	}
}

func TestRunLoggedSubprocessCapturesOutputToLogOnly(t *testing.T) {
	var terminal, log bytes.Buffer
	argv, env := ucChild(t, "loud")
	result, err := runLoggedSubprocess(context.Background(), gateway.NewUpdateOutputStream(&terminal, &log), argv, "", env)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReturnCode != 0 || !strings.Contains(result.Stdout, "LOUD BUILD OUTPUT") {
		t.Fatalf("result = %+v", result)
	}
	if terminal.Len() != 0 {
		t.Fatalf("terminal received %q", terminal.String())
	}
	if !strings.Contains(log.String(), "LOUD BUILD OUTPUT") {
		t.Fatalf("log = %q", log.String())
	}
}

func TestChildProgressReachesLogBeforeExit(t *testing.T) {
	for _, mode := range []string{"False", "True", "None"} {
		t.Run(mode, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("ICEBATH_HOME", tmp)
			terminal := &ucTerminal{}
			var w io.Writer = terminal
			var state *gateway.UpdateOutputState
			if mode != "None" {
				state = gateway.InstallHangupProtection(terminal, terminal, mode == "True")
				w = state.Stdout
			}
			log := filepath.Join(tmp, "logs", "update.log")
			release, ready := filepath.Join(tmp, "release"), filepath.Join(tmp, "ready")
			argv, env := ucChild(t, "progress", "ICEBATH_UC_READY="+ready, "ICEBATH_UC_RELEASE="+release)
			type outcome struct {
				result *CompletedProcess
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := runLoggedSubprocess(context.Background(), w, argv, "", env)
				done <- outcome{result, err}
			}()
			readLog := func() string {
				raw, _ := os.ReadFile(log)
				return string(raw)
			}
			var finished *outcome
			func() {
				defer func() {
					os.WriteFile(release, nil, 0o644)
					select {
					case got := <-done:
						finished = &got
					case <-time.After(10 * time.Second):
					}
					gateway.FinalizeUpdateOutput(state)
				}()
				text := ""
				for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
					if text = readLog(); strings.Contains(text, "progress: €�") {
						break
					}
				}
				if _, err := os.Stat(ready); err != nil {
					t.Fatal("child fixture never started")
				}
				if !strings.Contains(text, "progress: €�") {
					t.Fatalf("output withheld while child waits for release: %q", text)
				}
				select {
				case got := <-done:
					t.Fatalf("the worker finished before release: %+v", got)
				default:
				}
				info, err := os.Stat(log)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(300 * time.Millisecond)
				if again, err := os.Stat(log); err != nil || again.Size() != info.Size() {
					t.Fatal("silence must not manufacture progress")
				}
				if got := terminal.String(); got != "" {
					t.Fatalf("terminal received %q", got)
				}
				if state != nil {
					if !state.Installed {
						t.Fatal("the output mirror was not installed")
					}
					fmt.Fprintln(state.Stdout, "update stage")
					if !strings.Contains(readLog(), "update stage") {
						t.Fatal("a stage line did not reach the log")
					}
				}
			}()
			if finished == nil {
				t.Fatal("the worker is still running after release")
			}
			if finished.err != nil {
				t.Fatal(finished.err)
			}
			if finished.result.ReturnCode != 7 || finished.result.Stdout != "progress: €� done\n" || finished.result.Stderr != "" {
				t.Fatalf("result = %+v", finished.result)
			}
		})
	}
}

// Log whose every write is the operator's interrupt
type ucCancelLog struct{}

func (ucCancelLog) Write([]byte) (int, error) { return 0, cliui.ErrInterrupt }

func TestCancelledOutputReaderReapsChild(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pid")
	argv, env := ucChild(t, "cancel", "ICEBATH_UC_PIDFILE="+pidfile)
	started := time.Now()
	_, err := runLoggedSubprocess(context.Background(), gateway.NewUpdateOutputStream(&bytes.Buffer{}, ucCancelLog{}), argv, "", env)
	if !errors.Is(err, cliui.ErrInterrupt) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) >= 10*time.Second {
		t.Fatal("the cancelled reader did not return promptly")
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if proc.Exists(pid) {
		t.Fatalf("child %d survived the cancelled reader", pid)
	}
}
