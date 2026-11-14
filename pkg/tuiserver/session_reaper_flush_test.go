//go:build !windows

package tuiserver

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pysignal"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Puts the signal slots and the install state back the way the case found them
func restoreSignalState(t *testing.T, prev map[os.Signal]*pysignal.Handler) {
	t.Helper()
	for sig, handler := range prev {
		pysignal.Signal(sig, handler)
	}
	exitFlushMu.Lock()
	clear(exitFlushPrevHandlers)
	exitFlushHandlersInstalled = false
	exitFlushMu.Unlock()
	tools.ResetForegroundExitFenceForTests()
}

func TestSigtermFlushesPopulatedSessionIntoStateDb(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	db := reaperTestDB(t)
	defer db.Close()
	sid := "sess-sigterm-flush"
	if err := db.CreateSession(state.NewSession{ID: sid, Source: "tui"}); err != nil {
		t.Fatal(err)
	}
	a := newFlushAgent("survive the kill")
	a.persist = func(messages []*state.Message) {
		for _, msg := range messages {
			if msg.Extra["_db_persisted"] == true {
				continue
			}
			if _, err := db.AppendMessage(context.Background(), sid, map[string]any{"role": msg.Role, "content": msg.Content}, state.AppendGuards{}); err != nil {
				t.Error(err)
			}
			if msg.Extra == nil {
				msg.Extra = map[string]any{}
			}
			msg.Extra["_db_persisted"] = true
		}
	}
	registerFlushSession(t, sid, a, nil)

	chained := make(chan struct{}, 4)
	prev := map[os.Signal]*pysignal.Handler{
		syscall.SIGTERM: pysignal.Signal(syscall.SIGTERM, pysignal.Func(func(os.Signal) error { chained <- struct{}{}; return nil })),
		os.Interrupt:    pysignal.Getsignal(os.Interrupt),
	}
	called := false
	func() {
		defer restoreSignalState(t, prev)
		if !InstallExitFlushSignalHandlers() {
			t.Fatal("the flush handlers must install")
		}
		if err := pysignal.KillSelf(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case <-chained:
			called = true
		case <-time.After(5 * time.Second):
		}
	}()
	if !called {
		t.Fatal("previous SIGTERM handler must still be chained")
	}
	if a.calls() == 0 {
		t.Fatal("SIGTERM must flush in-memory sessions")
	}
	rows, err := db.GetMessages(context.Background(), sid, state.GetMessagesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if content, _ := row.Get("content"); strings.Contains(strOr(content), "survive the kill") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the transcript did not reach state.db: %v", rows)
	}
}

func TestIgnoredSigintLeavesTerminalCommandsRunnable(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	prev := map[os.Signal]*pysignal.Handler{
		syscall.SIGTERM: pysignal.Getsignal(syscall.SIGTERM),
		os.Interrupt:    pysignal.Signal(os.Interrupt, pysignal.Ignore),
	}
	func() {
		defer func() {
			for sig, handler := range prev {
				pysignal.Signal(sig, handler)
			}
			exitFlushMu.Lock()
			clear(exitFlushPrevHandlers)
			exitFlushHandlersInstalled = false
			exitFlushMu.Unlock()
		}()
		if !InstallExitFlushSignalHandlers() {
			t.Fatal("the flush handlers must install")
		}
		if err := pysignal.KillSelf(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	env := tools.NewLocalEnvironment(context.Background(), &tools.Env{Home: h}, cwd, 30, nil, false)
	defer env.Cleanup(false)
	result, err := env.Execute(context.Background(), "echo still-alive", tools.ExecOptions{Timeout: 30})
	if err != nil {
		t.Fatal(err)
	}
	if result.ReturnCode != 0 {
		t.Fatalf("an ignored SIGINT raised the exit fence: rc %d %q", result.ReturnCode, result.Output)
	}
}

func TestExitFlushIsBounded(t *testing.T) {
	a := newFlushAgent("unflushed turn")
	a.persist = func([]*state.Message) { time.Sleep(5 * time.Second) }
	registerFlushSession(t, "sess-hang", a, nil)
	budget := 0.3
	start := time.Now()
	flushSessionsBeforeExit(&budget)
	if elapsed := time.Since(start); elapsed >= 2*time.Second {
		t.Fatalf("exit flush blocked %.1fs past its budget", elapsed.Seconds())
	}
}

func TestExitFlushNeverWaitsOnAnExternalSecretSource(t *testing.T) {
	launch, served := t.TempDir(), t.TempDir()
	t.Setenv("ICEBATH_HOME", launch)
	a := newFlushAgent("hi")
	registerFlushSession(t, "sid", a, map[string]any{"profile_home": served})
	budget := 1.0
	start := time.Now()
	if got := flushSessionsBeforeExit(&budget); got != 1 {
		t.Fatalf("flushed %d transcripts, want 1", got)
	}
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("the flush waited %.1fs on something other than the transcript", elapsed.Seconds())
	}
}
