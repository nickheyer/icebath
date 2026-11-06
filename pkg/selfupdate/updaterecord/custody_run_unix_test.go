//go:build !windows

package updaterecord

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCustodyGitArgvInsertsMutatorConfigOnlyForLocalMutators(t *testing.T) {
	git := []string{"git", "-C", "/r"}
	if got := GitArgv(git, []string{"status"}); !reflect.DeepEqual(got, []string{"git", "-C", "/r", "-c", "gc.autoDetach=false", "-c", "maintenance.auto=false", "status"}) {
		t.Fatalf("reader argv %v", got)
	}
	got := GitArgv(git, []string{"checkout", "-q", "main"})
	want := []string{"git", "-C", "/r", "-c", "gc.autoDetach=false", "-c", "maintenance.auto=false",
		"-c", "core.fsmonitor=false", "-c", "credential.helper=", "-c", "core.hooksPath=" + os.DevNull, "checkout", "-q", "main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mutator argv %v", got)
	}
	if sub := GitSubcommand([]string{"-c", "x=y", "-C", "d", "--no-pager", "diff"}); sub != "diff" {
		t.Fatalf("subcommand %q", sub)
	}
	if _, err := RunGitQuery(context.Background(), git, []string{"reset", "--hard"}, time.Second); err == nil {
		t.Fatal("a mutator ran through the query runner")
	}
}

func TestCustodyRunGitQueryCapturesTextAndEndsTimedOutOrCancelledChildren(t *testing.T) {
	ctx := context.Background()
	shell := func(script string) []string { return []string{"/bin/sh", "-c", script, "sh"} }
	result, err := RunGitQuery(ctx, shell(`printf 'a\r\nb\377'; printf oops >&2; exit 3`), nil, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "a\nb�" || result.Stderr != "oops" || result.ReturnCode != 3 {
		t.Fatalf("result %+v", result)
	}
	start := time.Now()
	_, err = RunGitQuery(ctx, shell("sleep 30"), nil, 200*time.Millisecond)
	var timeout *GitTimeout
	if !errors.As(err, &timeout) || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout %v after %v", err, time.Since(start))
	}
	if !strings.Contains(err.Error(), "timed out after 0.2 seconds") {
		t.Fatalf("timeout text %q", err.Error())
	}
	cancelled, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	if _, err = RunGitQuery(cancelled, shell("sleep 30"), nil, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	if result, err = RunGitQuery(ctx, shell("kill -9 $$"), nil, 10*time.Second); err != nil || result.ReturnCode != -9 {
		t.Fatalf("signal death %+v %v", result, err)
	}
}

func TestCustodyARefusedChildIsTheRunsFirstRefusalNotice(t *testing.T) {
	custodyRun.Lock()
	saved := custodyRun.refused
	ran, unchanged := custodyRun.ran, custodyRun.unchanged
	custodyRun.refused, custodyRun.ran = nil, false
	custodyRun.Unlock()
	t.Cleanup(func() {
		custodyRun.Lock()
		custodyRun.refused, custodyRun.ran, custodyRun.unchanged = saved, ran, unchanged
		custodyRun.Unlock()
	})
	if RefusalNotice("icebath update") != nil {
		t.Fatal("a notice without a refusal")
	}
	exc := refuseChild(context.Background(), []string{"/usr/bin/git", "-C", "/r"}, errors.New("Access is denied."))
	if !strings.HasPrefix(exc.Reason, "Windows would not put `git` in this update's process job (Access is denied.)") {
		t.Fatalf("reason %q", exc.Reason)
	}
	_ = refuseChild(context.Background(), []string{"/usr/bin/later"}, errors.New("second"))
	notice := RefusalNotice("icebath update")
	if notice == nil || !strings.HasPrefix(*notice, "✗ `icebath update` stopped: "+exc.Reason+"\n  Nothing was changed: no update step had run yet.\n  Run `icebath update` again") {
		t.Fatalf("notice %v", notice)
	}
}
