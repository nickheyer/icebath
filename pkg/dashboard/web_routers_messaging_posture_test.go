package dashboard

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const messagingValidBotToken = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_1234"

// Makes this process wear the given command line, the verified gateway identity the hermes cases fake
func fakeGatewayCmdline(t *testing.T, cmdline string) {
	t.Helper()
	t.Cleanup(gateway.SwapProcessCmdlineReader(func(int) (string, bool) { return cmdline, true }))
}

// Live gateway record of this process in h with the given platform states
func writeMessagingGatewayState(t *testing.T, h string, platforms *pyjson.Object) {
	t.Helper()
	pidRecord, _ := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "hermes_home", h))
	state, _ := pyjson.Dumps(pyjson.ObjectOf("pid", os.Getpid(), "hermes_home", h, "gateway_state", "running",
		"kind", "hermes-gateway", "start_time", gateway.GetProcessStartTime(os.Getpid()),
		"updated_at", "2026-01-01T00:00:00+00:00", "platforms", platforms))
	writeTestFile(t, filepath.Join(h, "gateway.pid"), pidRecord)
	writeTestFile(t, filepath.Join(h, "gateway_state.json"), state)
}

// Client over a home whose telegram bot is configured, enabled and connected on this process's gateway
func postureClient(t *testing.T) (*testClient, string) {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	h := home.Process()
	for _, name := range []string{"TELEGRAM_BOT_TOKEN", "TELEGRAM_ALLOWED_USERS", "GATEWAY_ALLOWED_USERS",
		"GATEWAY_ALLOW_ALL_USERS", "TELEGRAM_GROUP_ALLOWED_USERS", "TELEGRAM_GROUP_ALLOWED_CHATS"} {
		unsetTestEnv(t, name)
	}
	writeTestFile(t, filepath.Join(h, ".env"), "TELEGRAM_BOT_TOKEN="+messagingValidBotToken+"\n")
	writeTestFile(t, filepath.Join(h, "config.yaml"), "platforms:\n  telegram:\n    enabled: true\n")
	writeMessagingGatewayState(t, h, pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "connected", "writer_pid", os.Getpid())))
	fakeGatewayCmdline(t, "icebath gateway run")
	return c, h
}

// Removes an env var for the test, restoring its value after
func unsetTestEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	os.Unsetenv(name)
}

// Test route result of the telegram card
func postureTest(t *testing.T, c *testClient, query string) *pyjson.Object {
	t.Helper()
	return appObject(t, c.request("POST", "/api/messaging/platforms/telegram/test"+query, nil, sessionHeaders()...))
}

func TestConnectedDenyAllReportsPosture(t *testing.T) {
	c, _ := postureClient(t)
	payload := postureTest(t, c, "")
	if payload.GetOr("ok", nil) != true || payload.GetOr("state", nil) != "connected" || payload.GetOr("auth_posture", nil) != "deny_all" {
		t.Fatalf("payload %v", payload)
	}
	if !strings.Contains(payload.GetOr("message", "").(string), "TELEGRAM_ALLOWED_USERS") {
		t.Fatalf("message %v", payload.GetOr("message", nil))
	}
}

func TestConnectedWithAllowlistReportsAllowlist(t *testing.T) {
	c, _ := postureClient(t)
	t.Setenv("TELEGRAM_ALLOWED_USERS", "42, 43")
	payload := postureTest(t, c, "")
	if payload.GetOr("ok", nil) != true || payload.GetOr("auth_posture", nil) != "allowlist" ||
		!strings.Contains(payload.GetOr("message", "").(string), "2 users allowlisted via TELEGRAM_ALLOWED_USERS") {
		t.Fatalf("payload %v", payload)
	}
}

func TestConnectedWithGlobalAllowAllReportsOpen(t *testing.T) {
	c, _ := postureClient(t)
	t.Setenv("GATEWAY_ALLOW_ALL_USERS", "true")
	payload := postureTest(t, c, "")
	if payload.GetOr("ok", nil) != true || payload.GetOr("auth_posture", nil) != "open" {
		t.Fatalf("payload %v", payload)
	}
}

func TestConnectedWithGatewayAllowedUsersReportsAllowlist(t *testing.T) {
	c, _ := postureClient(t)
	t.Setenv("GATEWAY_ALLOWED_USERS", "42")
	payload := postureTest(t, c, "")
	if payload.GetOr("ok", nil) != true || payload.GetOr("auth_posture", nil) != "allowlist" ||
		!strings.Contains(payload.GetOr("message", "").(string), "GATEWAY_ALLOWED_USERS") {
		t.Fatalf("payload %v", payload)
	}
}

func TestConnectedWithPairingGrantCountsIt(t *testing.T) {
	c, _ := postureClient(t)
	ctx := context.Background()
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	code, ok, err := store.GenerateCode(ctx, "telegram", "42", "operator")
	if err != nil || !ok {
		t.Fatalf("code %q %v %v", code, ok, err)
	}
	if _, err := store.ApproveCode(ctx, "telegram", code); err != nil {
		t.Fatal(err)
	}
	defer store.Revoke(ctx, "telegram", "42")
	payload := postureTest(t, c, "")
	if payload.GetOr("auth_posture", nil) != "pairing" || !strings.Contains(payload.GetOr("message", "").(string), "1 user paired") {
		t.Fatalf("payload %v", payload)
	}
}

func TestConnectedDenyAllMentionsPendingPairingRequests(t *testing.T) {
	c, _ := postureClient(t)
	ctx := context.Background()
	store, err := gateway.NewPairingStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.GenerateCode(ctx, "telegram", "42", "operator"); err != nil || !ok {
		t.Fatalf("pending request %v %v", ok, err)
	}
	defer store.ClearPending(ctx, "telegram")
	payload := postureTest(t, c, "")
	if payload.GetOr("auth_posture", nil) != "deny_all" ||
		!strings.Contains(payload.GetOr("message", "").(string), "1 pairing request pending approval") {
		t.Fatalf("payload %v", payload)
	}
}

func TestProfileScopedPostureIgnoresRootEnvAndReportsProfileGrants(t *testing.T) {
	c, defaultHome := postureClient(t)
	workerHome := filepath.Join(defaultHome, "profiles", "worker")
	writeTestFile(t, filepath.Join(workerHome, "config.yaml"), "platforms:\n  telegram:\n    enabled: true\n")
	writeTestFile(t, filepath.Join(workerHome, ".env"), "TELEGRAM_BOT_TOKEN="+messagingValidBotToken+"\n")
	writeMessagingGatewayState(t, workerHome, pyjson.ObjectOf("telegram", pyjson.ObjectOf("state", "connected", "writer_pid", os.Getpid())))
	t.Setenv("TELEGRAM_ALLOWED_USERS", "111,222")
	fakeGatewayCmdline(t, "icebath gateway run -p worker")
	payload := postureTest(t, c, "?profile=worker")
	if payload.GetOr("auth_posture", nil) != "deny_all" {
		t.Fatalf("payload %v", payload)
	}
	writeTestFile(t, filepath.Join(workerHome, ".env"), "TELEGRAM_BOT_TOKEN="+messagingValidBotToken+"\nTELEGRAM_ALLOWED_USERS=7\n")
	payload = postureTest(t, c, "?profile=worker")
	if payload.GetOr("auth_posture", nil) != "allowlist" || !strings.Contains(payload.GetOr("message", "").(string), "TELEGRAM_ALLOWED_USERS") {
		t.Fatalf("payload %v", payload)
	}
}
