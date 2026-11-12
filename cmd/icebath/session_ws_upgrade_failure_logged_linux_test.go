//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/dashboard"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Owner whose session socket handler fails the way the unimportable onboarding module did
const brokenSessionOwnerMode = "broken-session-owner"

// External cron worker turn and managed worker persistence call against the home owner
const sessionUpgradeDialsMode = "session-upgrade-dials"

func init() {
	childModes[brokenSessionOwnerMode] = func() {
		dashboard.ReplaceSessionHandleWS(func(context.Context, tuiserver.ServedSocket, map[string]any, *string, bool) error {
			return errors.New("NameError: name 'Path' is not defined")
		})
	}
	childModes[sessionUpgradeDialsMode] = runSessionUpgradeDials
}

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

func TestAFailedSessionUpgradeIsLoggedWithItsCause(t *testing.T) {
	tmp, h, user := gatewayCaseHomes(t, map[string]any{
		"gateway":   map[string]any{"multiplex_profiles": false},
		"model":     map[string]any{"provider": "custom", "default": "local-wire-stub", "base_url": "http://127.0.0.1:9/v1"},
		"auxiliary": map[string]any{"title_generation": map[string]any{"enabled": false}},
	})
	env := gatewayChildEnv(h, user, "OPENAI_API_KEY=loopback-only")
	owner := startGatewayChild(t, filepath.Join(tmp, "owner.out"),
		append(env, runMainEnv+"=1", childModeEnv+"="+brokenSessionOwnerMode), "gateway", "run")
	owner.waitReady(t, h, 35*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dials := exec.CommandContext(ctx, os.Args[0])
	dials.Env = append(env, childModeEnv+"="+sessionUpgradeDialsMode)
	var stdout, stderr strings.Builder
	dials.Stdout, dials.Stderr = &stdout, &stderr
	if err := dials.Run(); err != nil {
		t.Fatalf("dials: %v %v\n%s", err, ctx.Err(), stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	var result struct {
		Cron   []any `json:"cron"`
		Worker any   `json:"worker"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	if len(result.Cron) != 2 {
		t.Fatalf("result = %+v", result)
	}
	cronError, _ := result.Cron[1].(string)
	if result.Cron[0] != false || !strings.Contains(cronError, "HTTP 500") {
		t.Fatalf("result = %+v", result)
	}
	if worker, _ := result.Worker.(string); result.Worker == nil || !strings.Contains(worker, "HTTP 500") {
		t.Fatalf("result = %+v", result)
	}
	_, _ = owner.interrupt(20 * time.Second)
	raw, err := os.ReadFile(filepath.Join(h, "logs", "gateway.log"))
	if err != nil {
		t.Fatal(err)
	}
	var refused []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "session WebSocket upgrade") {
			refused = append(refused, line)
		}
	}
	if len(refused) != 2 {
		t.Fatalf("refused lines = %d\n%s", len(refused), raw)
	}
	for _, line := range refused {
		if !strings.Contains(line, "NameError: name 'Path' is not defined") {
			t.Fatalf("refused = %v", refused)
		}
	}
}
