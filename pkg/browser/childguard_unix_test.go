//go:build !windows

package browser

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	rootPipeEnv    = "ICEBATH_BROWSER_TEST_ROOT_PIPE"
	childLedgerEnv = "ICEBATH_BROWSER_TEST_LEDGER"
)

// Write end of the root fifo, open for the life of the root test binary
var rootPipeWriter *os.File

// Per test ledgers of the children a test started, keyed by test
var (
	ledgerMu sync.Mutex
	ledgers  = map[*testing.T]string{}
)

// Creates the fifo every child watches and returns the removal of its directory
func openRootPipe() func() {
	dir, err := os.MkdirTemp("", "browser-test-root-")
	if err != nil {
		panic(err)
	}
	path := filepath.Join(dir, "root")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		panic(err)
	}
	if rootPipeWriter, err = os.OpenFile(path, os.O_RDWR, 0); err != nil {
		panic(err)
	}
	os.Setenv(rootPipeEnv, path)
	return func() { os.RemoveAll(dir) }
}

// Notes this child in its test ledger and exits it once the root fifo loses its last writer
func exitWithRoot() error {
	path := os.Getenv(rootPipeEnv)
	if path == "" {
		return errors.New("child mode started without the root pipe")
	}
	pipe, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		os.Exit(0)
	}
	if ledger := os.Getenv(childLedgerEnv); ledger != "" {
		start, _ := tools.ProcessStartTime(os.Getpid())
		appendLine(ledger, fmt.Sprintf("%d %d", os.Getpid(), start))
	}
	go func() {
		buf := make([]byte, 64)
		for {
			if _, err := pipe.Read(buf); err != nil {
				os.Exit(0)
			}
		}
	}()
	return nil
}

// Ledger path of this test, its first use registering the kill of every child noted in it
func childLedger(t *testing.T) string {
	ledgerMu.Lock()
	defer ledgerMu.Unlock()
	if path, ok := ledgers[t]; ok {
		return path
	}
	dir, err := os.MkdirTemp("", "browser-test-ledger-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "children")
	ledgers[t] = path
	t.Cleanup(func() {
		killLedger(t, path)
		os.RemoveAll(dir)
		ledgerMu.Lock()
		delete(ledgers, t)
		ledgerMu.Unlock()
	})
	return path
}

// Live children of a ledger, each still the process its line names by start time
func ledgerLive(path string) []int {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var live []int
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) != 2 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		start, _ := strconv.ParseInt(fields[1], 10, 64)
		if now, ok := tools.ProcessStartTime(pid); ok && now == start && syscall.Kill(pid, 0) == nil {
			live = append(live, pid)
		}
	}
	return live
}

// Kills every noted child and the process group it leads, failing the test if one outlives the kill
func killLedger(t *testing.T, path string) {
	own := syscall.Getpgrp()
	for _, pid := range ledgerLive(path) {
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid && pgid != own {
			syscall.Kill(-pgid, syscall.SIGKILL)
		}
		syscall.Kill(pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(15 * time.Second)
	for len(ledgerLive(path)) > 0 {
		if time.Now().After(deadline) {
			t.Errorf("children %v outlived the kill", ledgerLive(path))
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Command running this test binary in a child mode, in its own process group and noted in the test ledger
func childCommand(t *testing.T, mode string, env ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(append(os.Environ(), childModeEnv+"="+mode, childLedgerEnv+"="+childLedger(t)), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}
