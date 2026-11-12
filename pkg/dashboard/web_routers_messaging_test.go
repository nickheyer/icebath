package dashboard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	validWorkerBotToken = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZ_1234"
	validBodyBotToken   = "987654321:ZYXWVUTSRQPONMLKJIHGFEDCBA_4321"
)

// Channels listing of the client, scoped by query
func messagingPlatformsList(t *testing.T, c *testClient, query string) *pyjson.Object {
	t.Helper()
	return appObject(t, c.get("/api/messaging/platforms"+query, sessionHeaders()...))
}

// Card of one platform in a listing
func messagingCard(t *testing.T, payload *pyjson.Object, platformID string) *pyjson.Object {
	t.Helper()
	cards, _ := payload.GetOr("platforms", []any{}).([]any)
	for _, raw := range cards {
		if card := raw.(*pyjson.Object); card.GetOr("id", nil) == platformID {
			return card
		}
	}
	t.Fatalf("no %s card in %v", platformID, payload)
	return nil
}

// Env row of one key on a card
func messagingEnvField(t *testing.T, card *pyjson.Object, key string) *pyjson.Object {
	t.Helper()
	rows, _ := card.GetOr("env_vars", []any{}).([]any)
	for _, raw := range rows {
		if row := raw.(*pyjson.Object); row.GetOr("key", nil) == key {
			return row
		}
	}
	t.Fatalf("no %s row on %v", key, card)
	return nil
}

// JSON write through the client with the session header
func messagingWrite(t *testing.T, c *testClient, method, target string, payload any) *testResponse {
	t.Helper()
	return appJSON(t, c, method, target, payload)
}

// Isolated default home holding a root telegram token and one empty named profile
func isolatedMessagingProfiles(t *testing.T) (*testClient, string, string) {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	defaultHome := os.Getenv("ICEBATH_HOME")
	worker := filepath.Join(defaultHome, "profiles", "worker_alpha")
	for _, h := range []string{defaultHome, worker} {
		writeTestFile(t, filepath.Join(h, "config.yaml"), "{}\n")
	}
	writeTestFile(t, filepath.Join(defaultHome, ".env"), "TELEGRAM_BOT_TOKEN=root-token\n")
	writeTestFile(t, filepath.Join(worker, ".env"), "")
	unsetTestEnv(t, "TELEGRAM_BOT_TOKEN")
	return c, defaultHome, worker
}

// Clears the host env vars the token free tables assume absent
func noPlatformEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GATEWAY_RELAY_URL", "GATEWAY_RELAY_ALLOW_DIRECT_PLATFORMS", "WHATSAPP_ENABLED", "WEBHOOK_ENABLED",
		"API_SERVER_ENABLED", "API_SERVER_KEY", "MSGRAPH_WEBHOOK_ENABLED", "MSGRAPH_WEBHOOK_CLIENT_STATE"} {
		unsetTestEnv(t, name)
	}
}

func TestStoppedGatewayDoesNotReportStalePlatformError(t *testing.T) {
	c := staleRuntimeClient(t)
	telegram := messagingCard(t, messagingPlatformsList(t, c, ""), "telegram")
	if telegram.GetOr("configured", nil) != true || telegram.GetOr("gateway_running", nil) != false ||
		telegram.GetOr("state", nil) != "gateway_stopped" || telegram.GetOr("error_code", 1) != nil || telegram.GetOr("error_message", 1) != nil {
		t.Fatalf("telegram %v", telegram)
	}
}

func TestOperatorStoppedGatewayDoesNotReportRetainedStartupFailure(t *testing.T) {
	c := staleRuntimeClient(t)
	writeTestFile(t, filepath.Join(home.Process(), "gateway_state.json"), `{"kind": "gateway", "pid": 999999999, "start_time": 1.0, `+
		`"gateway_state": "startup_failed", "desired_state": "stopped", "exit_reason": "Port 8642 already in use", `+
		`"updated_at": "2026-01-01T00:00:00+00:00", "platforms": {}}`)
	telegram := messagingCard(t, messagingPlatformsList(t, c, ""), "telegram")
	if telegram.GetOr("gateway_running", nil) != false || telegram.GetOr("state", nil) != "gateway_stopped" ||
		telegram.GetOr("error_code", 1) != nil || telegram.GetOr("error_message", 1) != nil {
		t.Fatalf("telegram %v", telegram)
	}
}

// Home with a saved telegram token and a dead gateway's record still saying no token was configured
func staleRuntimeClient(t *testing.T) *testClient {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	h := home.Process()
	unsetTestEnv(t, "TELEGRAM_BOT_TOKEN")
	writeTestFile(t, filepath.Join(h, ".env"), "TELEGRAM_BOT_TOKEN="+messagingValidBotToken+"\nTELEGRAM_ALLOWED_USERS=42\n")
	writeTestFile(t, filepath.Join(h, "config.yaml"), "platforms:\n  telegram:\n    enabled: true\n")
	writeTestFile(t, filepath.Join(h, "gateway_state.json"), `{"kind": "gateway", "pid": 999999999, "start_time": 1.0, `+
		`"gateway_state": "stopped", "exit_reason": "shutdown", "updated_at": "2026-01-01T00:00:00+00:00", `+
		`"platforms": {"telegram": {"state": "fatal", "error_code": "missing_credentials", `+
		`"error_message": "No bot token configured", "writer_pid": 999999999, "writer_start_time": 1.0}}}`)
	return c
}

func TestScopedTokenFreePlatformStatusMatchesTheGateway(t *testing.T) {
	on := func(id string) string { return "platforms:\n  " + id + ":\n    enabled: true\n" }
	cases := []struct {
		id, platform, config, env string
		enabled, configured       bool
	}{
		{"whatsapp-paired", "whatsapp", on("whatsapp"), "WHATSAPP_ENABLED=true\n", true, true},
		{"whatsapp-env-flag-only", "whatsapp", "{}\n", "WHATSAPP_ENABLED=true\n", true, true},
		{"whatsapp-unpaired", "whatsapp", on("whatsapp"), "", true, false},
		{"whatsapp-flag-off", "whatsapp", "platforms:\n  whatsapp:\n    enabled: true\n    extra:\n      mode: bot\n", "WHATSAPP_ENABLED=false\n", false, false},
		{"whatsapp-disabled-flag-on", "whatsapp", "platforms:\n  whatsapp:\n    enabled: false\n", "WHATSAPP_ENABLED=true\n", false, true},
		{"webhook", "webhook", on("webhook"), "", true, true},
		{"webhook-not-enabled", "webhook", "platforms:\n  webhook:\n    extra:\n      port: 8123\n", "", false, true},
		{"api-server-absent", "api_server", "{}\n", "", false, false},
		{"api-server-no-key", "api_server", on("api_server"), "", true, false},
		{"api-server-key", "api_server", "platforms:\n  api_server:\n    enabled: true\n    extra:\n      key: " + strings.Repeat("k", 24) + "\n", "", true, true},
		{"api-server-disabled-env-key", "api_server", "platforms:\n  api_server:\n    enabled: false\n", "API_SERVER_KEY=" + strings.Repeat("k", 32) + "\n", false, true},
		{"whatsapp-toplevel-disabled-flag-on", "whatsapp", "whatsapp:\n  enabled: false\n", "WHATSAPP_ENABLED=true\n", false, true},
		{"msgraph-no-client-state", "msgraph_webhook", on("msgraph_webhook"), "", true, false},
		{"msgraph-client-state", "msgraph_webhook", "platforms:\n  msgraph_webhook:\n    enabled: true\n    extra:\n      client_state: state\n", "", true, true},
		{"msgraph-disabled-env-on", "msgraph_webhook", "platforms:\n  msgraph_webhook:\n    enabled: false\n", "MSGRAPH_WEBHOOK_ENABLED=true\nMSGRAPH_WEBHOOK_CLIENT_STATE=state\n", false, true},
		{"relay-no-url", "relay", on("relay"), "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			c, _, worker := isolatedMessagingProfiles(t)
			noPlatformEnv(t)
			writeTestFile(t, filepath.Join(worker, "config.yaml"), tc.config)
			writeTestFile(t, filepath.Join(worker, ".env"), tc.env)
			card := messagingCard(t, messagingPlatformsList(t, c, "?profile=worker_alpha"), tc.platform)
			if card.GetOr("enabled", nil) != tc.enabled || card.GetOr("configured", nil) != tc.configured {
				t.Fatalf("(enabled, configured) = (%v, %v) want (%v, %v)", card.GetOr("enabled", nil), card.GetOr("configured", nil),
					tc.enabled, tc.configured)
			}
		})
	}
}

func TestScopedStatusFollowsTheProfilesRelayBlock(t *testing.T) {
	msgraph := "  msgraph_webhook:\n    enabled: true\n    extra:\n      client_state: state\n"
	cases := []struct {
		id, config    string
		directEnabled bool
	}{
		{"relay-owns-connections", "platforms:\n" + msgraph + "  relay:\n    enabled: true\n", false},
		{"relay-opted-out", "platforms:\n" + msgraph + "  relay:\n    enabled: false\n", true},
		{"relay-opted-out-toplevel", "platforms:\n" + msgraph + "relay:\n  enabled: false\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			c, _, worker := isolatedMessagingProfiles(t)
			noPlatformEnv(t)
			t.Setenv("GATEWAY_RELAY_URL", "https://relay.example.test")
			writeTestFile(t, filepath.Join(worker, "config.yaml"), tc.config)
			card := messagingCard(t, messagingPlatformsList(t, c, "?profile=worker_alpha"), "msgraph_webhook")
			if card.GetOr("enabled", nil) != tc.directEnabled || card.GetOr("configured", nil) != true {
				t.Fatalf("msgraph %v", card)
			}
		})
	}
}

// 1Password source whose fetch answers the given token and records the homes it served
type fakeOnePasswordSource struct {
	provider.OnePasswordSource
	token   string
	fetched *[]string
}

func (s *fakeOnePasswordSource) Fetch(cfg map[string]any, homePath string, environ *provider.SourceEnvironment) *provider.SecretFetchResult {
	*s.fetched = append(*s.fetched, homePath)
	result := provider.NewSecretFetchResult()
	result.Secrets = map[string]string{"TELEGRAM_BOT_TOKEN": s.token}
	return result
}

func TestScopedOnepasswordCredentialCountsOnlyWhenTheSourceIsEnabled(t *testing.T) {
	cases := []struct {
		id            string
		sourceEnabled bool
		token         string
		counts        bool
	}{
		{"enabled", true, strings.Repeat("1", 9) + ":" + strings.Repeat("a", 35), true},
		{"disabled", false, strings.Repeat("1", 9) + ":" + strings.Repeat("a", 35), false},
		{"enabled-empty-value", true, "", false},
		{"enabled-blank-value", true, "  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			c, _, worker := isolatedMessagingProfiles(t)
			var fetched []string
			provider.RegisterSecretSource(&fakeOnePasswordSource{token: tc.token, fetched: &fetched}, true, true, "")
			t.Cleanup(func() { provider.RegisterSecretSource(&provider.OnePasswordSource{}, true, true, "") })
			writeTestFile(t, filepath.Join(worker, "config.yaml"), fmt.Sprintf("secrets:\n  onepassword:\n    enabled: %v\n"+
				"    env:\n      TELEGRAM_BOT_TOKEN: op://vault/item/credential\n", tc.sourceEnabled))
			telegram := messagingCard(t, messagingPlatformsList(t, c, "?profile=worker_alpha"), "telegram")
			if telegram.GetOr("configured", nil) != tc.counts || messagingEnvField(t, telegram, "TELEGRAM_BOT_TOKEN").GetOr("is_set", nil) != tc.counts {
				t.Fatalf("telegram %v", telegram)
			}
			if (len(fetched) > 0) != tc.sourceEnabled {
				t.Fatalf("fetched %v", fetched)
			}
		})
	}
}

func TestProfileScopedMessagingReadsScopedReadDoesNotShowRootCredentials(t *testing.T) {
	c, _, _ := isolatedMessagingProfiles(t)
	telegram := messagingCard(t, messagingPlatformsList(t, c, "?profile=worker_alpha"), "telegram")
	if messagingEnvField(t, telegram, "TELEGRAM_BOT_TOKEN").GetOr("is_set", nil) != false || telegram.GetOr("configured", nil) != false {
		t.Fatalf("telegram %v", telegram)
	}
}

func TestProfileScopedMessagingReadsAllowlistValueIsReadableButSecretsStayRedacted(t *testing.T) {
	c, _, worker := isolatedMessagingProfiles(t)
	writeTestFile(t, filepath.Join(worker, ".env"), "TELEGRAM_BOT_TOKEN="+validWorkerBotToken+"\nTELEGRAM_ALLOWED_USERS=111,222\n")
	telegram := messagingCard(t, messagingPlatformsList(t, c, "?profile=worker_alpha"), "telegram")
	allowlist := messagingEnvField(t, telegram, "TELEGRAM_ALLOWED_USERS")
	token := messagingEnvField(t, telegram, "TELEGRAM_BOT_TOKEN")
	if allowlist.GetOr("is_list", nil) != true || allowlist.GetOr("value", nil) != "111,222" {
		t.Fatalf("allowlist %v", allowlist)
	}
	if token.GetOr("is_list", nil) != false || token.GetOr("value", 1) != nil {
		t.Fatalf("token %v", token)
	}
	if text, _ := pyjson.Dumps(token); strings.Contains(text, validWorkerBotToken) {
		t.Fatalf("token row leaks the secret %s", text)
	}
}

func TestProfileScopedMessagingReadsUnknownProfileReturns404(t *testing.T) {
	c, _, _ := isolatedMessagingProfiles(t)
	if r := c.get("/api/messaging/platforms?profile=no_such_profile", sessionHeaders()...); r.Status != 404 {
		t.Fatalf("status %d", r.Status)
	}
}

func TestProfileScopedMessagingReadsScopedReadReturnsProfilePathCommandAndStartupFailure(t *testing.T) {
	c, _, worker := isolatedMessagingProfiles(t)
	writeTestFile(t, filepath.Join(worker, ".env"), "TELEGRAM_BOT_TOKEN=worker-token\n")
	writeTestFile(t, filepath.Join(worker, "config.yaml"), "platforms:\n  telegram:\n    enabled: true\n")
	savedProbe, savedRead := messagingPidProbe, messagingReadRuntimeStatus
	t.Cleanup(func() { messagingPidProbe, messagingReadRuntimeStatus = savedProbe, savedRead })
	messagingPidProbe = func(string) (int, bool, error) { return 0, false, nil }
	messagingReadRuntimeStatus = func(string) *pyjson.Object {
		return pyjson.ObjectOf("gateway_state", "startup_failed",
			"exit_reason", "all configured messaging platforms failed to connect", "platforms", pyjson.NewObject())
	}
	payload := messagingPlatformsList(t, c, "?profile=worker_alpha")
	if payload.GetOr("env_path", nil) != filepath.Join(worker, ".env") ||
		payload.GetOr("gateway_start_command", nil) != "icebath -p worker_alpha gateway start" {
		t.Fatalf("payload %v", payload)
	}
	telegram := messagingCard(t, payload, "telegram")
	if telegram.GetOr("state", nil) != "startup_failed" || telegram.GetOr("error_code", nil) != "startup_failed" ||
		telegram.GetOr("error_message", nil) != "all configured messaging platforms failed to connect" {
		t.Fatalf("telegram %v", telegram)
	}
}

// Raw config of a home
func homeRawConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	cfg, err := config.ReadRawConfig(context.Background(), h)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Value at a key path of a config mapping, nil when any step is missing
func configPath(cfg map[string]any, keys ...string) any {
	var cur any = cfg
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func TestProfileScopedMessagingWritesScopedWriteLandsInTargetProfileEnv(t *testing.T) {
	c, defaultHome, worker := isolatedMessagingProfiles(t)
	r := messagingWrite(t, c, "PUT", "/api/messaging/platforms/telegram?profile=worker_alpha",
		pyjson.ObjectOf("enabled", true, "env", pyjson.ObjectOf("TELEGRAM_BOT_TOKEN", validWorkerBotToken)))
	if r.Status != 200 {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	if got, _ := os.ReadFile(filepath.Join(worker, ".env")); !strings.Contains(string(got), "TELEGRAM_BOT_TOKEN="+validWorkerBotToken) {
		t.Fatalf("worker env %q", got)
	}
	rootEnv, _ := os.ReadFile(filepath.Join(defaultHome, ".env"))
	if strings.Contains(string(rootEnv), validWorkerBotToken) || !strings.Contains(string(rootEnv), "TELEGRAM_BOT_TOKEN=root-token") {
		t.Fatalf("root env %q", rootEnv)
	}
	if configPath(homeRawConfig(t, worker), "platforms", "telegram", "enabled") != true {
		t.Fatal("enablement missing from the worker config")
	}
	if configPath(homeRawConfig(t, defaultHome), "platforms", "telegram") != nil {
		t.Fatal("enablement leaked into the root config")
	}
}

func TestProfileScopedMessagingWritesScopedReadAfterScopedWriteRoundTrips(t *testing.T) {
	c, _, _ := isolatedMessagingProfiles(t)
	messagingWrite(t, c, "PUT", "/api/messaging/platforms/telegram?profile=worker_alpha",
		pyjson.ObjectOf("enabled", true, "env", pyjson.ObjectOf("TELEGRAM_BOT_TOKEN", validWorkerBotToken)))
	telegram := messagingCard(t, messagingPlatformsList(t, c, "?profile=worker_alpha"), "telegram")
	if telegram.GetOr("enabled", nil) != true || messagingEnvField(t, telegram, "TELEGRAM_BOT_TOKEN").GetOr("is_set", nil) != true ||
		telegram.GetOr("configured", nil) != true {
		t.Fatalf("telegram %v", telegram)
	}
}

// Turns multiplex_profiles on in the default profile's config
func enableMultiplex(t *testing.T, defaultHome string) {
	t.Helper()
	writeTestFile(t, filepath.Join(defaultHome, "config.yaml"), "gateway:\n  multiplex_profiles: true\n")
}

func TestMultiplexPortBindingGuardRejectsOnlyMirroredListenersOnSecondary(t *testing.T) {
	c, defaultHome, _ := isolatedMessagingProfiles(t)
	unsetTestEnv(t, "GATEWAY_MULTIPLEX_PROFILES")
	enableMultiplex(t, defaultHome)
	if len(gateway.SharedListenerMirrorPlatforms) == 0 {
		t.Fatal("guard set must not be empty")
	}
	var ids []string
	for _, raw := range messagingPlatformsList(t, c, "").GetOr("platforms", []any{}).([]any) {
		if id := raw.(*pyjson.Object).GetOr("id", "").(string); gateway.PortBindingPlatformValues[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, platformID := range ids {
		r := messagingWrite(t, c, "PUT", "/api/messaging/platforms/"+platformID+"?profile=worker_alpha", pyjson.ObjectOf("enabled", true))
		want := 200
		if gateway.SharedListenerMirrorPlatforms[platformID] {
			want = 409
		}
		if r.Status != want {
			t.Fatalf("%s status %d body %s", platformID, r.Status, r.Body)
		}
	}
}

func TestMultiplexPortBindingGuardSecondaryCanDisableAndClearInvalidConfig(t *testing.T) {
	c, defaultHome, worker := isolatedMessagingProfiles(t)
	unsetTestEnv(t, "GATEWAY_MULTIPLEX_PROFILES")
	enableMultiplex(t, defaultHome)
	writeTestFile(t, filepath.Join(worker, "config.yaml"), "platforms:\n  api_server:\n    enabled: true\n")
	if r := messagingWrite(t, c, "PUT", "/api/messaging/platforms/api_server?profile=worker_alpha", pyjson.ObjectOf("enabled", false)); r.Status != 200 {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	if configPath(homeRawConfig(t, worker), "platforms", "api_server", "enabled") != false {
		t.Fatal("disable not saved")
	}
	apiServer := messagingCard(t, messagingPlatformsList(t, c, "?profile=worker_alpha"), "api_server")
	if rows, _ := apiServer.GetOr("env_vars", []any{}).([]any); len(rows) > 0 {
		key := rows[0].(*pyjson.Object).GetOr("key", "")
		if r := messagingWrite(t, c, "PUT", "/api/messaging/platforms/api_server?profile=worker_alpha",
			pyjson.ObjectOf("clear_env", []any{key})); r.Status != 200 {
			t.Fatalf("clear status %d body %s", r.Status, r.Body)
		}
	}
}

func TestNamedCurrentHomeMatchesUnscoped(t *testing.T) {
	c, defaultHome, worker := isolatedMessagingProfiles(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "root-token")
	for _, query := range []string{"", "?profile=current", "?profile=default"} {
		if enabled := messagingCard(t, messagingPlatformsList(t, c, query), "telegram").GetOr("enabled", nil); enabled != true {
			t.Fatalf("%q enabled %v", query, enabled)
		}
	}
	sctx, scoped, err := configProfileScope(home.WithOverride(context.Background(), worker), "default")
	if err != nil {
		t.Fatal(err)
	}
	if scoped != "" || home.Dir(sctx) != defaultHome {
		t.Fatalf("scoped %q home %s", scoped, home.Dir(sctx))
	}
}

func TestScopedEnablementUsesOnlyOwnCredentials(t *testing.T) {
	c, defaultHome, worker := isolatedMessagingProfiles(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "root-token")
	query := "?profile=worker_alpha"
	if enabled := messagingCard(t, messagingPlatformsList(t, c, query), "telegram").GetOr("enabled", nil); enabled != false {
		t.Fatalf("enabled %v", enabled)
	}
	writeTestFile(t, filepath.Join(worker, ".env"), "TELEGRAM_BOT_TOKEN=worker-token\n")
	payload := messagingPlatformsList(t, c, query)
	telegram := messagingCard(t, payload, "telegram")
	if telegram.GetOr("enabled", nil) != true || telegram.GetOr("configured", nil) != true || telegram.GetOr("state", nil) == "disabled" {
		t.Fatalf("telegram %v", telegram)
	}
	empty := map[string]bool{}
	for _, entry := range messagingPlatformCatalog(context.Background()) {
		if len(entry.GetOr("required_env", []any{}).([]any)) == 0 {
			empty[entry.GetOr("id", "").(string)] = true
		}
	}
	for _, raw := range payload.GetOr("platforms", []any{}).([]any) {
		card := raw.(*pyjson.Object)
		if empty[card.GetOr("id", "").(string)] && (card.GetOr("enabled", nil) != false || card.GetOr("configured", nil) != false) {
			t.Fatalf("token free card %v", card)
		}
	}
	for _, enabled := range []bool{false, true} {
		writeTestFile(t, filepath.Join(worker, "config.yaml"), fmt.Sprintf("platforms:\n  telegram:\n    enabled: %v\n", enabled))
		card := messagingCard(t, messagingPlatformsList(t, c, query), "telegram")
		if card.GetOr("enabled", nil) != enabled || card.GetOr("configured", nil) != true {
			t.Fatalf("enabled %v card %v", enabled, card)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(defaultHome, ".env")); !strings.Contains(string(got), "root-token") {
		t.Fatalf("root env %q", got)
	}
}

// Swaps the multiplexer hooks of the hot serve notice for the test
func swapHotServeHooks(t *testing.T) {
	t.Helper()
	savedCurrent, savedServed, savedNotify := messagingCurrentProfileName, messagingProfileServedByMux, messagingNotifyProfilesChange
	t.Cleanup(func() {
		messagingCurrentProfileName, messagingProfileServedByMux, messagingNotifyProfilesChange = savedCurrent, savedServed, savedNotify
	})
}

func TestCredentialWriteHotServesAMultiplexedProfile(t *testing.T) {
	for _, topology := range []string{"scoped_query", "pooled_unscoped"} {
		t.Run(topology, func(t *testing.T) {
			c, _, _ := isolatedMessagingProfiles(t)
			swapHotServeHooks(t)
			var notified []string
			messagingProfileServedByMux = func(_ context.Context, name *string) (bool, error) {
				return name != nil && *name == "worker_alpha", nil
			}
			messagingNotifyProfilesChange = func(_ context.Context, name string, _ time.Duration) ([]string, bool) {
				notified = append(notified, name)
				return []string{"default", name}, true
			}
			query := "?profile=worker_alpha"
			if topology == "pooled_unscoped" {
				messagingCurrentProfileName = func(context.Context) (string, error) { return "worker_alpha", nil }
				query = ""
			}
			r := messagingWrite(t, c, "PUT", "/api/messaging/platforms/telegram"+query,
				pyjson.ObjectOf("enabled", true, "env", pyjson.ObjectOf("TELEGRAM_BOT_TOKEN", validWorkerBotToken)))
			if r.Status != 200 {
				t.Fatalf("status %d body %s", r.Status, r.Body)
			}
			if appObject(t, r).GetOr("hot_served", nil) != true || len(notified) != 1 || notified[0] != "worker_alpha" {
				t.Fatalf("body %s notified %v", r.Body, notified)
			}
		})
	}
}

func TestCredentialWriteOnDefaultProfileIsNotHotServed(t *testing.T) {
	c, _, _ := isolatedMessagingProfiles(t)
	swapHotServeHooks(t)
	messagingNotifyProfilesChange = func(context.Context, string, time.Duration) ([]string, bool) {
		t.Error("default profile must not ping the multiplexer")
		return nil, false
	}
	r := messagingWrite(t, c, "PUT", "/api/messaging/platforms/telegram",
		pyjson.ObjectOf("enabled", true, "env", pyjson.ObjectOf("TELEGRAM_BOT_TOKEN", validWorkerBotToken)))
	if r.Status != 200 || appObject(t, r).GetOr("hot_served", nil) != false {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
}

func TestPluginEntryDeclaresCardMetadataAndEnvValueRules(t *testing.T) {
	ctx := context.Background()
	entry := gateway.NewPlatformEntry("cardtest", "Card Test", nil, func() bool { return true })
	entry.RequiredEnv = []string{"CARDTEST_TOKEN", "CARDTEST_HOST"}
	entry.InstallHint = "pip install cardtest"
	entry.DashboardLabel = "Card Test (beta)"
	entry.DashboardDescription = "Talk to Hermes over Card Test."
	entry.DocsURL = "https://example.invalid/cardtest"
	entry.DashboardEnvVars = []string{"CARDTEST_TOKEN"}
	entry.DashboardRequiredEnv = []string{"CARDTEST_TOKEN"}
	entry.EnvValueRules = map[string]gateway.EnvValueRule{"CARDTEST_TOKEN": {
		Check: func(v string) bool { return strings.HasPrefix(v, "ct-") }, Message: "must start with ct-"}}
	scope := gateway.CurrentScopeKey(ctx)
	if err := gateway.Registry.Register(ctx, entry, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Registry.Unregister(ctx, "cardtest", scope) })
	card := buildCatalogEntry("cardtest", entry)
	envVars := card.GetOr("env_vars", []any{}).([]any)
	required, _ := pyjson.Dumps(card.GetOr("required_env", nil))
	if card.GetOr("name", nil) != "Card Test (beta)" || card.GetOr("description", nil) != "Talk to Hermes over Card Test." ||
		card.GetOr("docs_url", nil) != "https://example.invalid/cardtest" || len(envVars) == 0 || envVars[0] != "CARDTEST_TOKEN" ||
		required != `["CARDTEST_TOKEN"]` {
		t.Fatalf("card %v", card)
	}
	rules := messagingEnvValueRules(ctx, "cardtest")
	if err := validateMessagingEnvValue(rules, "CARDTEST_TOKEN", "ct-123"); err != nil {
		t.Fatal(err)
	}
	refusal, ok := asHTTPException(validateMessagingEnvValue(rules, "CARDTEST_TOKEN", "nope"))
	if !ok || refusal.StatusCode != 400 || refusal.Detail != "must start with ct-" {
		t.Fatalf("refusal %v", refusal)
	}
}

func TestMessagingPlatformsProfileScopesGatewayReads(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	workerHome, err := profiles.GetProfileDir("worker")
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(workerHome, "config.yaml"), "")
	var pidPaths, statusPaths, expectedHomes []string
	savedProbe, savedRead, savedRuntimePid := messagingPidProbe, messagingReadRuntimeStatus, messagingRuntimePidProbe
	t.Cleanup(func() {
		messagingPidProbe, messagingReadRuntimeStatus, messagingRuntimePidProbe = savedProbe, savedRead, savedRuntimePid
	})
	messagingPidProbe = func(p string) (int, bool, error) {
		pidPaths = append(pidPaths, p)
		return 0, false, nil
	}
	messagingReadRuntimeStatus = func(p string) *pyjson.Object {
		statusPaths = append(statusPaths, p)
		return nil
	}
	messagingRuntimePidProbe = func(_ *pyjson.Object, expectedHome string) (int, bool, error) {
		expectedHomes = append(expectedHomes, expectedHome)
		return 0, false, nil
	}
	if r := c.get("/api/messaging/platforms?profile=worker", sessionHeaders()...); r.Status != 200 {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	if !slices.Contains(pidPaths, filepath.Join(workerHome, "gateway.pid")) ||
		!slices.Contains(statusPaths, filepath.Join(workerHome, "gateway_state.json")) || !slices.Contains(expectedHomes, workerHome) {
		t.Fatalf("pid paths %v status paths %v expected homes %v", pidPaths, statusPaths, expectedHomes)
	}
}

func TestMessagingCatalogPrefersPluginLabelOverEnumPseudoMember(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	ctx := context.Background()
	entry := gateway.NewPlatformEntry("pseudofake", "Pseudo Fake (plugin label)", nil, func() bool { return true })
	entry.Source = "plugin"
	if err := gateway.Registry.Register(ctx, entry, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		gateway.Registry.Unregister(ctx, "pseudofake", "")
		gateway.ForgetPseudoPlatform("pseudofake")
	})
	member, err := gateway.ParsePlatform(ctx, "pseudofake")
	if err != nil || string(member) != "pseudofake" {
		t.Fatalf("member %q %v", member, err)
	}
	if !slices.Contains(gateway.PlatformMemberMap(), member) {
		t.Fatal("PSEUDOFAKE missing from the platform members")
	}
	card := messagingCard(t, messagingPlatformsList(t, c, ""), "pseudofake")
	if card.GetOr("name", nil) != "Pseudo Fake (plugin label)" {
		t.Fatalf("card %v", card)
	}
}

func TestTelegramOnboardingApplyReportsRestartFailureAfterSave(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	telegramOnboardingMu.Lock()
	telegramOnboardingPairings = map[string]*telegramOnboardingPairing{}
	telegramOnboardingMu.Unlock()
	savedRequest, savedSpawn := telegramOnboardingRequestSync, spawnHermesAction
	t.Cleanup(func() { telegramOnboardingRequestSync, spawnHermesAction = savedRequest, savedSpawn })
	telegramOnboardingRequestSync = func(_ context.Context, method, path string, body *pyjson.Object, bearerToken string) (*pyjson.Object, error) {
		if method == "POST" {
			return pyjson.ObjectOf("pairing_id", "pair-restart-fails", "poll_token", "poll-secret",
				"suggested_username", "hermes_pair_restart_fails_bot",
				"deep_link", "https://t.me/newbot/HermesSetupBot/hermes_pair_restart_fails_bot",
				"qr_payload", "https://t.me/newbot/HermesSetupBot/hermes_pair_restart_fails_bot",
				"expires_at", "2027-05-18T00:00:00.000Z"), nil
		}
		if method != "GET" || path != "/v1/telegram/pairings/pair-restart-fails" || bearerToken != "poll-secret" {
			t.Errorf("request %s %s %q", method, path, bearerToken)
		}
		return pyjson.ObjectOf("status", "ready", "bot_username", "hermes_pair_restart_fails_bot",
			"owner_user_id", 123456789, "token", "123456:SECRET"), nil
	}
	actionMu.Lock()
	delete(actionProcs, "gateway-restart")
	actionMu.Unlock()
	spawnHermesAction = func(_ context.Context, subcommand []string, name string, _ map[string]string) (actionProc, error) {
		if !slices.Equal(subcommand, []string{"-p", "default", "gateway", "restart"}) || name != "gateway-restart" {
			t.Errorf("spawn %v %s", subcommand, name)
		}
		return nil, errors.New("supervisor unavailable")
	}
	if r := messagingWrite(t, c, "POST", "/api/messaging/telegram/onboarding/start", pyjson.NewObject()); r.Status != 200 {
		t.Fatalf("start %d %s", r.Status, r.Body)
	}
	ready := c.get("/api/messaging/telegram/onboarding/pair-restart-fails", sessionHeaders()...)
	if appObject(t, ready).GetOr("status", nil) != "ready" {
		t.Fatalf("ready %s", ready.Body)
	}
	applied := messagingWrite(t, c, "POST", "/api/messaging/telegram/onboarding/pair-restart-fails/apply",
		pyjson.ObjectOf("allowed_user_ids", []any{"123456789"}))
	data := appObject(t, applied)
	if data.GetOr("ok", nil) != true || data.GetOr("needs_restart", nil) != true || data.GetOr("restart_started", nil) != false ||
		!strings.Contains(fmt.Sprint(data.GetOr("restart_error", "")), "supervisor unavailable") {
		t.Fatalf("applied %v", data)
	}
	if _, has := data.Get("token"); has {
		t.Fatalf("applied leaks the token %v", data)
	}
	h := home.Process()
	env := config.LoadEnv(h)
	if env["TELEGRAM_BOT_TOKEN"] != "123456:SECRET" || env["TELEGRAM_ALLOWED_USERS"] != "123456789" {
		t.Fatalf("env %v", env)
	}
	loaded, err := webLoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if configPath(loaded, "platforms", "telegram", "enabled") != true {
		t.Fatalf("config %v", loaded)
	}
}

func TestMessagingAndCustomEndpointRejectStalePreviews(t *testing.T) {
	c := newTestClient(t, dashboardTestApp(t))
	ctx := context.Background()
	h := home.Process()
	key, real := "DISCORD_BOT_TOKEN", "discord-live-secret-abcdef1234567890"
	if err := config.SaveEnvValueContext(ctx, h, key, real); err != nil {
		t.Fatal(err)
	}
	r := messagingWrite(t, c, "PUT", "/api/messaging/platforms/discord",
		pyjson.ObjectOf("clear_env", []any{key}, "env", pyjson.ObjectOf(key, config.RedactKey(real))))
	if r.Status != 400 {
		t.Fatalf("status %d body %s", r.Status, r.Body)
	}
	if got := config.LoadEnv(h)[key]; got != real {
		t.Fatalf("env %q", got)
	}
	for k, v := range map[string]string{"OLD_ENDPOINT_KEY": "old-secret-1234567890", "NEW_ENDPOINT_KEY": "new-secret-0987654321"} {
		if err := config.SaveEnvValueContext(ctx, h, k, v); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := webLoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["providers"] = map[string]any{
		"env-preview": map[string]any{"name": "Env Preview", "base_url": "https://env-preview.example.com/v1",
			"model": "m", "key_env": "OLD_ENDPOINT_KEY", "models": map[string]any{"m": map[string]any{}}},
		"legacy-preview": map[string]any{"name": "Legacy Preview", "base_url": "https://legacy-preview.example.com/v1",
			"model": "m", "api_key": "legacy-secret-A-1234567890", "models": map[string]any{"m": map[string]any{}}},
	}
	if err := webSaveConfig(ctx, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	endpoints := map[string]*pyjson.Object{}
	for _, raw := range appObject(t, c.get("/api/providers/custom-endpoints", sessionHeaders()...)).GetOr("endpoints", []any{}).([]any) {
		e := raw.(*pyjson.Object)
		endpoints[e.GetOr("id", "").(string)] = e
	}
	if endpoints["env-preview"].GetOr("api_key_preview", nil) != "${OLD_ENDPOINT_KEY}" ||
		!strings.HasPrefix(fmt.Sprint(endpoints["legacy-preview"].GetOr("api_key_preview", "")), "«redacted") {
		t.Fatalf("endpoints %v", endpoints)
	}
	cfg, err = webLoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cfg["providers"].(map[string]any)["env-preview"].(map[string]any)["key_env"] = "NEW_ENDPOINT_KEY"
	cfg["providers"].(map[string]any)["legacy-preview"].(map[string]any)["api_key"] = "legacy-secret-B-0987654321"
	if err := webSaveConfig(ctx, cfg, config.SaveOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range [][2]string{{"env-preview", "https://env-preview.example.com/v1"}, {"legacy-preview", "https://legacy-preview.example.com/v1"}} {
		r := messagingWrite(t, c, "POST", "/api/providers/custom-endpoints", pyjson.ObjectOf("id", endpoint[0], "name", "x",
			"base_url", endpoint[1], "model", "m", "api_key", endpoints[endpoint[0]].GetOr("api_key_preview", nil)))
		if r.Status != 400 {
			t.Fatalf("%s status %d body %s", endpoint[0], r.Status, r.Body)
		}
	}
	loaded, err := webLoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if configPath(loaded, "providers", "env-preview", "key_env") != "NEW_ENDPOINT_KEY" ||
		configPath(loaded, "providers", "legacy-preview", "api_key") != "legacy-secret-B-0987654321" {
		t.Fatalf("providers %v", loaded["providers"])
	}
}

const workchatPlatformPlugin = `
def register(ctx):
    ctx.register_platform(
        name="workchat", label="Work Chat", adapter_factory=lambda cfg: None, check_fn=lambda: True,
        required_env=["WORKCHAT_TOKEN"],
        env_value_rules={"WORKCHAT_TOKEN": (lambda v: v.startswith("wc-"), "WORKCHAT_TOKEN starts with wc-")})
`

// Root home and a work profile holding a workchat platform plugin it enables, with the plugin host on python3
func workchatHomes(t *testing.T) (*testClient, string, string) {
	t.Helper()
	c := newTestClient(t, dashboardTestApp(t))
	root := os.Getenv("ICEBATH_HOME")
	work := filepath.Join(root, "profiles", "work")
	pluginDir := filepath.Join(work, "plugins", "workchat")
	writeTestFile(t, filepath.Join(pluginDir, "plugin.yaml"), "kind: platform\nname: workchat\nversion: '1.0'\n")
	writeTestFile(t, filepath.Join(pluginDir, "__init__.py"), workchatPlatformPlugin)
	writeTestFile(t, filepath.Join(work, "config.yaml"), "plugins:\n  enabled:\n  - workchat\n")
	writeTestFile(t, filepath.Join(root, "config.yaml"), "model:\n  default: gpt-5\n")
	for _, h := range []string{root, work} {
		writeTestFile(t, filepath.Join(h, ".env"), "")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required to run the plugin host child: %v", err)
	}
	t.Cleanup(plugins.SetHostInterpreterForTests(python))
	plugins.ResetPluginManagersForTests()
	t.Cleanup(plugins.ResetPluginManagersForTests)
	return c, root, work
}

func TestAGatewayBlockForAProfilePluginStaysInThatProfile(t *testing.T) {
	c, _, work := workchatHomes(t)
	writeTestFile(t, filepath.Join(work, "config.yaml"), "gateway:\n  workchat:\n    enabled: true\nplatforms:\n  webhook:\n    enabled: true\n"+
		"plugins:\n  enabled:\n  - workchat\n")
	leakedBefore := slices.Contains(gateway.PlatformMemberMap(), gateway.Platform("workchat"))
	scoped := messagingPlatformsList(t, c, "?profile=work")
	root := messagingPlatformsList(t, c, "")
	if messagingCard(t, scoped, "webhook").GetOr("configured", nil) != true {
		t.Fatalf("webhook %v", messagingCard(t, scoped, "webhook"))
	}
	if slices.Contains(gateway.PlatformMemberMap(), gateway.Platform("workchat")) != leakedBefore {
		t.Fatal("the profile plugin registered process wide")
	}
	for _, raw := range root.GetOr("platforms", []any{}).([]any) {
		if raw.(*pyjson.Object).GetOr("id", nil) == "workchat" {
			t.Fatal("workchat listed for the root profile")
		}
	}
}

func TestAManagedGatewayBlockForAProfilePluginStaysInThatProfile(t *testing.T) {
	c, _, work := workchatHomes(t)
	managed := filepath.Join(t.TempDir(), "managed")
	writeTestFile(t, filepath.Join(managed, "config.yaml"), "gateway:\n  workchat:\n    enabled: true\n")
	t.Setenv(config.EnvManagedDir, managed)
	config.InvalidateManagedCache()
	t.Cleanup(config.InvalidateManagedCache)
	writeTestFile(t, filepath.Join(work, "config.yaml"), "plugins:\n  enabled:\n  - workchat\nplatforms:\n  webhook:\n    enabled: true\n")
	leakedBefore := slices.Contains(gateway.PlatformMemberMap(), gateway.Platform("workchat"))
	scoped := messagingPlatformsList(t, c, "?profile=work")
	if messagingCard(t, scoped, "webhook").GetOr("configured", nil) != true {
		t.Fatalf("webhook %v", messagingCard(t, scoped, "webhook"))
	}
	if slices.Contains(gateway.PlatformMemberMap(), gateway.Platform("workchat")) != leakedBefore {
		t.Fatal("the managed block registered the profile plugin process wide")
	}
}
