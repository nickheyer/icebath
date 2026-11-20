//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tuiserver"
)

// Owner whose session socket handler fails the way the unimportable onboarding module did
const brokenSessionOwnerMode = "broken-session-owner"

// External cron worker turn and managed worker persistence call against the home owner
const sessionUpgradeDialsMode = "session-upgrade-dials"

func init() {
	childModes[brokenSessionOwnerMode] = breakSessionOwner
	childModes[sessionUpgradeDialsMode] = runSessionUpgradeDials
}

// NameError the broken owner raises where it resolves its session socket handler
type sessionHandlerNameError struct{}

func (sessionHandlerNameError) Error() string { return "name 'Path' is not defined" }

func (sessionHandlerNameError) PyTypeName() string { return "NameError" }

// Makes every admitted session socket fail with the NameError before it answers and returns to the ordinary gateway entry
func breakSessionOwner() {
	sessionHandleWS = func(context.Context, tuiserver.ServedSocket, map[string]any, *string, bool) error {
		return sessionHandlerNameError{}
	}
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
		append(append([]string{}, env...), childModeEnv+"="+brokenSessionOwnerMode), gateway.GatewayRunModuleEntry)
	owner.waitReady(t, h, 35*time.Second)
	code, stdout, stderr := runCLIProgram(t, cliProgramBinary(t),
		append(append([]string{}, env...), childModeEnv+"="+sessionUpgradeDialsMode), 60*time.Second)
	if code != 0 {
		t.Fatalf("dials exit %d\n%s", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var result struct {
		Cron   []any `json:"cron"`
		Worker any   `json:"worker"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(result.Cron) != 2 {
		t.Fatalf("cron = %v", result.Cron)
	}
	if cronError, _ := result.Cron[1].(string); result.Cron[0] != false || !strings.Contains(cronError, "HTTP 500") {
		t.Fatalf("cron = %v", result.Cron)
	}
	if worker, _ := result.Worker.(string); !strings.Contains(worker, "HTTP 500") {
		t.Fatalf("worker = %v", result.Worker)
	}
	if exited, err := owner.interrupt(20 * time.Second); !exited {
		t.Fatalf("the owner outlived its interrupt: %v\n%s", err, owner.output())
	}
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
		t.Fatalf("%d refused upgrade lines\n%s", len(refused), raw)
	}
	for _, line := range refused {
		if !strings.Contains(line, "NameError: name 'Path' is not defined") {
			t.Fatalf("refused = %q", refused)
		}
	}
}
