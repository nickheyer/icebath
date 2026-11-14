package gatewaycli_test

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/webhooksubs"
)

// Runs the real icebath entry point and returns the process exit status
func icebath(argv ...string) int {
	code, _ := commands.ExitCode(commands.NewCLI().Execute(argv))
	return code
}

func TestWebhookCLIRefusalExitsNonzeroAndWritesNothing(t *testing.T) {
	root, _ := gatewaycli.CronHomesForEntry(t)
	if code := icebath("webhook", "subscribe", "route", "--route-profile", "coder", "--cron-job", "no-such-job"); code != 1 {
		t.Fatalf("exit status %d", code)
	}
	if _, err := os.Stat(webhooksubs.Path(root)); !os.IsNotExist(err) {
		t.Fatalf("subscriptions file written: %v", err)
	}
}

func TestWebhookCLIRoutedCronSubscribeExitsZero(t *testing.T) {
	root, _ := gatewaycli.CronHomesForEntry(t)
	if code := icebath("webhook", "subscribe", "route", "--route-profile", "coder", "--cron-job", "coder-job"); code != 0 {
		t.Fatalf("exit status %d", code)
	}
	route, _ := webhooksubs.Load(root).Get("route")
	obj, ok := route.(*pyjson.Object)
	if !ok {
		t.Fatalf("route %v", route)
	}
	if job, _ := obj.Get("cron_job"); job != "coder-job" {
		t.Fatalf("cron_job %v", job)
	}
}
