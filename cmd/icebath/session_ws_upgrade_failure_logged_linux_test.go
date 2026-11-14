//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/home"
)

// Owner whose session socket handler fails the way the unimportable onboarding module did
const brokenSessionOwnerMode = "broken-session-owner"

// External cron worker turn and managed worker persistence call against the home owner
const sessionUpgradeDialsMode = "session-upgrade-dials"

// Class name and message of a Python classed error, the text type(exc).__name__ and str(exc) give
func exceptionText(err error) string {
	var named interface {
		error
		PyTypeName() string
	}
	if errors.As(err, &named) {
		return named.PyTypeName() + ": " + named.Error()
	}
	return err.Error()
}

// Prints the cron outcome and the worker failure as one JSON line, exit 1 when either raised past its caller
func runSessionUpgradeDials() {
	ctx := context.Background()
	startProcess(ctx)
	executionID := "cron4-exec"
	success, _, _, failure, err := cron.RunCanonicalJob(ctx, &cron.Job{ID: "cron4job", Name: "cron-4"}, nil, nil, &executionID)
	if err != nil {
		fmt.Fprintln(os.Stderr, exceptionText(err))
		os.Exit(1)
	}
	var cronError, worker any
	if failure != nil {
		cronError = *failure
	}
	rpc, err := agent.NewWorkerRPC(home.Dir(ctx))
	if err != nil {
		fmt.Fprintln(os.Stderr, exceptionText(err))
		os.Exit(1)
	}
	if _, err := rpc.Call(ctx, "worker.heartbeat", nil); err != nil {
		worker = exceptionText(err)
	}
	rpc.Close()
	raw, err := json.Marshal(map[string]any{"cron": []any{success, cronError}, "worker": worker})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(string(raw))
	os.Exit(0)
}
