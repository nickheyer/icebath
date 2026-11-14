package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/state"
)

const resumeHintSID = "20260524_000001_abc123"

// Every interactive exit that prints the summary after clearing the screen
var interactiveExits = []struct {
	name string
	exit func(t *testing.T, c *CLI)
}{
	{"print_exit_summary", func(_ *testing.T, c *CLI) { c.printExitSummary(true) }},
	{"run_stdin_unusable", exitThroughUnusableStdin},
	{"tui_shutdown", exitThroughShutdown},
}

// Marks the once per process cleanup done so the exit path skips it the way the cases patch it out
func skipExitCleanup(t *testing.T) {
	t.Helper()
	cleanupMu.Lock()
	prev := cleanupDone
	cleanupDone = true
	cleanupMu.Unlock()
	t.Cleanup(func() {
		cleanupMu.Lock()
		cleanupDone = prev
		cleanupMu.Unlock()
	})
}

// Runs the interactive loop over a closed standard input so it exits before the prompt starts
func exitThroughUnusableStdin(t *testing.T, c *CLI) {
	t.Helper()
	skipExitCleanup(t)
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	stdin.Close()
	stdout, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	prevIn, prevOut := tuiStdin, tuiStdout
	tuiStdin, tuiStdout = func() *os.File { return stdin }, func() *os.File { return stdout }
	t.Cleanup(func() {
		c.releaseActiveSession()
		tuiStdin, tuiStdout = prevIn, prevOut
		stdout.Close()
	})
	c.run()
	c.setShouldExit(false)
	joinCLIWorkers(t, c)
	rec, ok := console().Out.(*printRecorder)
	if !ok || !strings.Contains(strings.Join(rec.plain(), "\n"), c.t("cli.tui.stdin_unavailable", nil)) {
		t.Fatal("the loop did not exit through the unusable stdin check")
	}
}

// Waits until no other goroutine still runs a method of the CLI, the workers the loop started before its exit
func joinCLIWorkers(t *testing.T, c *CLI) {
	t.Helper()
	receiver := fmt.Sprintf("(%p", c)
	deadline := time.Now().Add(30 * time.Second)
	buf := make([]byte, 1<<22)
	for {
		dumps := strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n")
		busy := ""
		for _, g := range dumps[1:] {
			if strings.Contains(g, "app.(*CLI).") && strings.Contains(g, receiver) {
				busy = g
				break
			}
		}
		if busy == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a worker of the loop never stopped\n%s", busy)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Tears the prompt down the way the loop does once it returns
func exitThroughShutdown(t *testing.T, c *CLI) {
	t.Helper()
	skipExitCleanup(t)
	c.tuiShutdown()
}

// Replaces the active profile lookup of the resume hint
func fakeExitProfile(t *testing.T, lookup func(context.Context) string) {
	t.Helper()
	prev := exitSummaryActiveProfileName
	t.Cleanup(func() { exitSummaryActiveProfileName = prev })
	exitSummaryActiveProfileName = lookup
}

// A CLI with one user turn on the case session, a scripted gateway answering every call it makes
func resumeHintCLI(t *testing.T) (*CLI, *printRecorder) {
	t.Helper()
	rec := recordPrints(t)
	resetProcessState(t)
	client := &scriptedClient{}
	client.override = func(method string, params map[string]any) (any, bool, error) {
		if method == "session.create" || method == "session.resume" {
			client.record(method, params)
			return storeSnapshot(resumeHintSID), true, nil
		}
		return nil, false, nil
	}
	installScriptedGateway(t, client)
	c := configuredCLI(t, map[string]any{}, CLIOptions{})
	c.sessionDB = nil
	c.sessionID = resumeHintSID
	c.conversationHistory = []map[string]any{{"role": "user", "content": "hi"}}
	c.sessionStart = time.Now()
	return c, rec
}

// Seeds the case session titled in the home state store and points the CLI at the store reading it
func titledResumeHintStore(t *testing.T, c *CLI, title string) {
	t.Helper()
	ctx := context.Background()
	db, err := state.NewSessionDB(ctx, filepath.Join(home.Dir(ctx), "state.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateSession(state.NewSession{ID: resumeHintSID, Source: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetSessionTitle(ctx, resumeHintSID, title); err != nil {
		t.Fatal(err)
	}
	store := gatewaycli.SessionStore()
	t.Cleanup(func() { _ = store.CloseReader() })
	c.sessionDB = store
}

func TestExitSummaryResumeHintResumeHintNoProfileFlagOnDefault(t *testing.T) {
	for _, path := range interactiveExits {
		t.Run(path.name, func(t *testing.T) {
			c, rec := resumeHintCLI(t)
			fakeExitProfile(t, func(context.Context) string { return "default" })
			path.exit(t, c)
			out := strings.Join(rec.plain(), "\n")
			if !strings.Contains(out, "icebath --resume "+resumeHintSID) || strings.Contains(out, " -p ") {
				t.Fatalf("default profile hint %q", out)
			}
		})
	}
}

func TestExitSummaryResumeHintResumeHintNoProfileFlagOnCustom(t *testing.T) {
	for _, path := range interactiveExits {
		t.Run(path.name, func(t *testing.T) {
			c, rec := resumeHintCLI(t)
			fakeExitProfile(t, func(context.Context) string { return "custom" })
			path.exit(t, c)
			out := strings.Join(rec.plain(), "\n")
			if !strings.Contains(out, "icebath --resume "+resumeHintSID) || strings.Contains(out, " -p ") {
				t.Fatalf("custom profile hint %q", out)
			}
		})
	}
}

func TestExitSummaryResumeHintResumeHintIncludesProfileFlagForNamedProfile(t *testing.T) {
	for _, path := range interactiveExits {
		t.Run(path.name, func(t *testing.T) {
			c, rec := resumeHintCLI(t)
			fakeExitProfile(t, func(context.Context) string { return "dev" })
			path.exit(t, c)
			out := strings.Join(rec.plain(), "\n")
			if !strings.Contains(out, "icebath --resume "+resumeHintSID+" -p dev") {
				t.Fatalf("named profile hint %q", out)
			}
		})
	}
}

func TestExitSummaryResumeHintResumeHintIncludesProfileFlagOnTitleHintToo(t *testing.T) {
	for _, path := range interactiveExits {
		t.Run(path.name, func(t *testing.T) {
			c, rec := resumeHintCLI(t)
			titledResumeHintStore(t, c, "My Cool Session")
			fakeExitProfile(t, func(context.Context) string { return "dev" })
			path.exit(t, c)
			out := strings.Join(rec.plain(), "\n")
			if !strings.Contains(out, `icebath -c "My Cool Session" -p dev`) || !strings.Contains(out, "icebath --resume "+resumeHintSID+" -p dev") {
				t.Fatalf("title hint %q", out)
			}
		})
	}
}

func TestExitSummaryResumeHintResumeHintFallsBackWhenProfileLookupFails(t *testing.T) {
	for _, path := range interactiveExits {
		t.Run(path.name, func(t *testing.T) {
			c, rec := resumeHintCLI(t)
			fakeExitProfile(t, func(context.Context) string { panic("profiles unavailable") })
			path.exit(t, c)
			out := strings.Join(rec.plain(), "\n")
			if !strings.Contains(out, "icebath --resume "+resumeHintSID) || strings.Contains(out, " -p ") {
				t.Fatalf("fallback hint %q", out)
			}
		})
	}
}
