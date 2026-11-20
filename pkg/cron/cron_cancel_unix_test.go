//go:build !windows

package cron

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

func TestCancellingAScriptJobKillsItAndNamesTheReason(t *testing.T) {
	s := scriptStore(t)
	t.Setenv("ICEBATH_CRON_SCRIPT_TIMEOUT", "60")
	t.Setenv("PYTHONPATH", "")
	os.Unsetenv("PYTHONPATH")
	ready := filepath.Join(s.Home(), "script.pid")
	script := writeScript(t, s, "blocking.py", "import os, time\n"+
		"open("+pyrepr.StrRepr(ready+".tmp")+", 'w').write(str(os.getpid()))\n"+
		"os.replace("+pyrepr.StrRepr(ready+".tmp")+", "+pyrepr.StrRepr(ready)+")\n"+
		"time.sleep(120)\n")
	cancel := s.newCancelRequest("exec-live-script")
	type result struct {
		ok  bool
		out string
		err error
	}
	results := make(chan result, 1)
	go func() {
		ok, out, err := s.runJobScript(context.Background(), script, s.Home(), cancel, nil)
		results <- result{ok, out, err}
	}()
	readPid := func() (int, bool) {
		raw, ok := readUTF8Sig(ready)
		if !ok {
			return 0, false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(raw))
		return pid, err == nil
	}
	pid := 0
	defer func() {
		if p, ok := readPid(); ok && proc.Exists(p) {
			syscall.Kill(p, syscall.SIGKILL)
		}
	}()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if p, ok := readPid(); ok {
			pid = p
			break
		}
	}
	if pid == 0 {
		t.Fatal("script must start before it can be cancelled")
	}
	mustRequestCancel(t, s, "exec-live-script", strPtr("operator asked"))
	var got result
	select {
	case got = <-results:
	case <-time.After(20 * time.Second):
		t.Fatal("the cancelled script run did not return")
	}
	if got.err != nil || got.ok || !strings.Contains(got.out, "operator asked") {
		t.Fatalf("result %+v", got)
	}
	for deadline := time.Now().Add(5 * time.Second); proc.Exists(pid) && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
	}
	if proc.Exists(pid) {
		t.Fatal("the cancelled script must not outlive the cancel")
	}
}
