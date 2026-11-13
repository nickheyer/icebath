package main

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"sync"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	oneshotExit            = os.Exit
	oneshotLoggingShutdown = logging.Shutdown

	oneshotCleanupMu   sync.Mutex
	oneshotCleanupDone bool
)

// Process global teardown steps a hard exit would skip, run in order with each failure swallowed
var oneshotCleanups = []func(ctx context.Context){
	func(ctx context.Context) { tools.CleanupAllEnvironments(processToolsEnv(ctx)) },
	func(ctx context.Context) { tools.InterruptAllAsyncDelegations("oneshot shutdown") },
	func(ctx context.Context) { browser.EmergencyCleanupAllSessions(ctx) },
	func(ctx context.Context) { mcp.ShutdownMCPServers(ctx, nil, nil, 0) },
	func(ctx context.Context) { provider.ShutdownCachedClients(ctx) },
	func(ctx context.Context) { _ = provider.ShutdownLocalRuntime() },
	func(ctx context.Context) { observability.ShutdownRuntimes(ctx) },
}

// Flushes stdio and file logging then ends the process with rc, skipping every registered exit hook
func exitAfterOneshot(rc int) {
	for _, stream := range []*os.File{os.Stdout, os.Stderr} {
		_ = stream.Sync()
	}
	func() {
		defer func() { _ = recover() }()
		oneshotLoggingShutdown()
	}()
	oneshotExit(rc)
}

// Runs the process global oneshot teardown once per process
func cleanupOneshotRuntime(ctx context.Context) {
	oneshotCleanupMu.Lock()
	if oneshotCleanupDone {
		oneshotCleanupMu.Unlock()
		return
	}
	oneshotCleanupDone = true
	oneshotCleanupMu.Unlock()
	tools.BeginProcessShutdown()
	for _, step := range oneshotCleanups {
		func() {
			defer func() { _ = recover() }()
			step(ctx)
		}()
	}
}

// Exit status of one in process oneshot run, 130 on interrupt and 1 when the run itself panics
func oneshotExitCode(ctx context.Context, prompt string, o app.OneshotOptions) (rc int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic: %v\n\n%s", r, debug.Stack())
			rc = 1
		}
	}()
	code, failure := app.RunOneshot(ctx, prompt, o)
	if failure == nil {
		return code
	}
	if exitCode, ok := app.OneshotSystemExitCode(failure); ok {
		return exitCode
	}
	if app.IsOneshotKeyboardInterrupt(failure) {
		return 130
	}
	fmt.Fprintln(os.Stderr, failure)
	return 1
}

// Runs one prompt in process, tears down the process globals and hard exits with the run status
func runAndExitOneshot(ctx context.Context, prompt string, o app.OneshotOptions) {
	rc := oneshotExitCode(ctx, prompt, o)
	defer exitAfterOneshot(rc)
	cleanupOneshotRuntime(ctx)
}
