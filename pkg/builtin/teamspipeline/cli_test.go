package teamspipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gateway/messagetools"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Namespace with every argparse default the CLI tests start from
func makeCLIArgs(kv ...any) map[string]any {
	ns := map[string]any{"teams_pipeline_action": nil, "store_path": "", "status": "", "limit": 20, "job_id": "",
		"meeting_id": "", "join_web_url": "", "tenant_id": "", "call_record_id": "", "resource": "", "notification_url": "",
		"change_type": "updated", "expiration": "", "client_state": "", "lifecycle_notification_url": "",
		"latest_supported_tls_version": "v1_2", "subscription_id": "", "force_refresh": false, "renew_within_hours": 24,
		"extend_hours": 24, "dry_run": false}
	for i := 0; i+1 < len(kv); i += 2 {
		ns[kv[i].(string)] = kv[i+1]
	}
	return ns
}

// Captures CLI stdout for the test and isolates the home directory
func captureCLI(t *testing.T) *bytes.Buffer {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var buf bytes.Buffer
	prev := cliOut
	cliOut = &buf
	t.Cleanup(func() { cliOut = prev })
	return &buf
}

// Graph client double that records mutating calls and answers from fixed values
type cliGraph struct {
	calls   []string
	bodies  []any
	post    any
	patch   any
	deleted map[string]any
	subs    []any
}

func (g *cliGraph) GetJSON(_ context.Context, path string, _ map[string]any, _ map[string]string) (any, error) {
	panic("unexpected GET " + path)
}
func (g *cliGraph) PostJSON(_ context.Context, path string, body any, _ map[string]string) (any, error) {
	g.calls, g.bodies = append(g.calls, "POST "+path), append(g.bodies, body)
	return g.post, nil
}
func (g *cliGraph) PatchJSON(_ context.Context, path string, body any, _ map[string]string) (any, error) {
	g.calls, g.bodies = append(g.calls, "PATCH "+path), append(g.bodies, body)
	return g.patch, nil
}
func (g *cliGraph) Delete(_ context.Context, path string, _ map[string]string) (map[string]any, error) {
	g.calls = append(g.calls, "DELETE "+path)
	return g.deleted, nil
}
func (g *cliGraph) CollectPaginated(_ context.Context, path string, _ map[string]any, _ map[string]string) ([]any, error) {
	g.calls = append(g.calls, "COLLECT "+path)
	return g.subs, nil
}
func (g *cliGraph) DownloadToFile(_ context.Context, path, _ string, _ map[string]string) (map[string]any, error) {
	panic("unexpected download " + path)
}

// Installs the graph double as the client every CLI action builds
func useCLIGraph(t *testing.T, g *cliGraph) {
	prev := cliGraphClient
	cliGraphClient = func() (graphClient, error) { return g, nil }
	t.Cleanup(func() { cliGraphClient = prev })
}

func decodeCLIJSON(t *testing.T, out string) *pyjson.Object {
	t.Helper()
	v, err := pyjson.Loads(out)
	if err != nil {
		t.Fatalf("%v in %q", err, out)
	}
	return v.(*pyjson.Object)
}

func exitCode(err error) int {
	var ee *commands.ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestShowPrintsJobJSON(t *testing.T) {
	out := captureCLI(t)
	path := filepath.Join(t.TempDir(), "teams_pipeline_store.json")
	store, err := NewTeamsPipelineStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertJob("job-1", obj("event_id", "evt-1", "source_event_type", "updated", "dedupe_key", "evt-1",
		"status", "completed", "meeting_ref", obj("meeting_id", "meeting-1"))); err != nil {
		t.Fatal(err)
	}
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "show", "job_id", "job-1", "store_path", path)); err != nil {
		t.Fatal(err)
	}
	payload := decodeCLIJSON(t, out.String())
	if get(payload, "job_id") != "job-1" || get(get(payload, "meeting_ref").(*pyjson.Object), "meeting_id") != "meeting-1" {
		t.Fatal(out.String())
	}
	if !strings.HasPrefix(out.String(), "{\n  \"created_at\"") {
		t.Fatalf("not sorted two space JSON %q", out.String())
	}
}

func TestValidateAcceptsMsgraphCredentialsForGraphDelivery(t *testing.T) {
	out := captureCLI(t)
	t.Setenv("MSGRAPH_TENANT_ID", "tenant")
	t.Setenv("MSGRAPH_CLIENT_ID", "client")
	t.Setenv("MSGRAPH_CLIENT_SECRET", "secret")
	prev := loadGatewayConfig
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) {
		webhook, teams := gateway.NewPlatformConfig(), gateway.NewPlatformConfig()
		webhook.Enabled, teams.Enabled = true, true
		teams.Extra = map[string]any{"delivery_mode": "graph", "team_id": "team-1", "channel_id": "channel-1"}
		return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{
			gateway.PlatformMSGraphWebhook: webhook, gateway.Platform("teams"): teams}}, nil
	}
	t.Cleanup(func() { loadGatewayConfig = prev })
	path := filepath.Join(t.TempDir(), "teams_pipeline_store.json")
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "validate", "store_path", path)); err != nil {
		t.Fatal(err)
	}
	payload := decodeCLIJSON(t, out.String())
	if get(payload, "ok") != true || len(get(payload, "issues").([]any)) != 0 || get(payload, "teams_delivery_mode") != "graph" ||
		get(payload, "store_path") != path {
		t.Fatal(out.String())
	}
}

func TestTeamsPipelineValidateReportsMissingSetup(t *testing.T) {
	out := captureCLI(t)
	for _, k := range []string{"MSGRAPH_TENANT_ID", "MSGRAPH_CLIENT_ID", "MSGRAPH_CLIENT_SECRET"} {
		t.Setenv(k, "")
	}
	prev := loadGatewayConfig
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) {
		teams := gateway.NewPlatformConfig()
		teams.Enabled = true
		teams.Extra = map[string]any{"delivery_mode": " incoming_webhook "}
		return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.Platform("teams"): teams}}, nil
	}
	t.Cleanup(func() { loadGatewayConfig = prev })
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "validate", "store_path", filepath.Join(t.TempDir(), "s.json"))); err != nil {
		t.Fatal(err)
	}
	payload := decodeCLIJSON(t, out.String())
	want := []any{"Microsoft Graph app-only credentials are incomplete.", "MSGRAPH_WEBHOOK_ENABLED is not enabled.",
		"TEAMS_INCOMING_WEBHOOK_URL is required for incoming_webhook mode."}
	if get(payload, "ok") != false || !reflect.DeepEqual(get(payload, "issues"), want) || len(get(payload, "warnings").([]any)) != 0 {
		t.Fatal(out.String())
	}
}

func TestTeamsPipelineCommandUsageUnknownAndRequiredArgs(t *testing.T) {
	out := captureCLI(t)
	if code := exitCode(teamsPipelineCommand(makeCLIArgs())); code != 2 ||
		out.String() != "Usage: icebath teams-pipeline {list|show|run|fetch|subscriptions|subscribe|renew-subscription|delete-subscription|maintain-subscriptions|token-health|validate}\n" {
		t.Fatal(code, out.String())
	}
	out.Reset()
	if code := exitCode(teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "bogus"))); code != 2 || out.String() != "Unknown teams-pipeline action: bogus\n" {
		t.Fatal(code, out.String())
	}
	out.Reset()
	if code := exitCode(teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "renew-subscription", "subscription_id", "sub-1"))); code != 0 ||
		out.String() != "subscription_id and --expiration are required\n" {
		t.Fatal(code, out.String())
	}
	out.Reset()
	if code := exitCode(teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "replay", "job_id", "  "))); code != 0 || out.String() != "job_id is required\n" {
		t.Fatal(code, out.String())
	}
	out.Reset()
	if code := exitCode(teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "test"))); code != 0 || out.String() != "meeting_id or join_web_url is required\n" {
		t.Fatal(code, out.String())
	}
}

func TestTeamsPipelineGraphSetupHintWhenCredentialsMissing(t *testing.T) {
	out := captureCLI(t)
	for _, k := range []string{"MSGRAPH_TENANT_ID", "MSGRAPH_CLIENT_ID", "MSGRAPH_CLIENT_SECRET"} {
		t.Setenv(k, "")
	}
	for _, action := range []string{"fetch", "token-health"} {
		out.Reset()
		code := exitCode(teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", action, "meeting_id", "meeting-1")))
		want := "\n  Microsoft Graph is not configured. Add these to " + home.Display(home.Dir(context.Background())) + "/.env:\n\n" +
			"    MSGRAPH_TENANT_ID=...\n    MSGRAPH_CLIENT_ID=...\n    MSGRAPH_CLIENT_SECRET=...\n\n" +
			"  Then restart the gateway or rerun this command.\n\n"
		if code != 1 || out.String() != want {
			t.Fatalf("%s: %d %q", action, code, out.String())
		}
	}
}

func TestTeamsPipelineListSortsFiltersAndCaps(t *testing.T) {
	out := captureCLI(t)
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := NewTeamsPipelineStore(path)
	prev := nowUTC
	t.Cleanup(func() { nowUTC = prev })
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, j := range []*pyjson.Object{
		obj("status", "completed", "meeting_ref", obj("meeting_id", "m-1")),
		obj("status", "FAILED", "error_info", obj("code", "x")),
		obj("status", "completed", "selected_artifact_strategy", "transcript"),
	} {
		nowUTC = func() time.Time { return base.Add(time.Duration(i) * time.Hour) }
		if _, err := store.UpsertJob("job-"+string(rune('a'+i)), j); err != nil {
			t.Fatal(err)
		}
	}
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "ls", "store_path", path, "status", "Completed ")); err != nil {
		t.Fatal(err)
	}
	want := "\n2 Teams pipeline job(s):\n\n" +
		"  ◆ job-c\n    status: completed\n    meeting: unknown\n    strategy: transcript\n    updated: 2026-01-01T02:00:00+00:00\n\n" +
		"  ◆ job-a\n    status: completed\n    meeting: m-1\n    updated: 2026-01-01T00:00:00+00:00\n\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "list", "store_path", path, "limit", 0, "status", "failed")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "    error: {'code': 'x'}\n") || !strings.HasPrefix(out.String(), "\n1 Teams pipeline job(s):") {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "list", "store_path", path, "limit", -5)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "\n1 Teams pipeline job(s):\n\n  ◆ job-c\n") {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "list", "store_path", filepath.Join(t.TempDir(), "e.json"))); err != nil {
		t.Fatal(err)
	}
	if out.String() != "No Teams meeting pipeline jobs found.\n" {
		t.Fatalf("%q", out.String())
	}
}

func TestTeamsPipelineShowCompactsTranscriptAndReportsUnknownJob(t *testing.T) {
	out := captureCLI(t)
	path := filepath.Join(t.TempDir(), "s.json")
	store, _ := NewTeamsPipelineStore(path)
	long := strings.Repeat("é", 300)
	if _, err := store.UpsertJob("job-1", obj("summary_payload", obj("title", "T", "transcript_text", long))); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertJob("job-2", obj("summary_payload", obj("transcript_text", ""))); err != nil {
		t.Fatal(err)
	}
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "show", "job_id", "job-1", "store_path", path)); err != nil {
		t.Fatal(err)
	}
	summary := get(decodeCLIJSON(t, out.String()), "summary_payload").(*pyjson.Object)
	if get(summary, "transcript_preview") != strings.Repeat("é", 240) || get(summary, "transcript_text") != nil || get(summary, "title") != "T" {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "show", "job_id", "job-2", "store_path", path)); err != nil {
		t.Fatal(err)
	}
	if v, ok := decodeCLIJSON(t, out.String()).Get("summary_payload"); !ok || v != nil {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "show", "job_id", "nope", "store_path", path)); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Unknown job: nope\n" {
		t.Fatalf("%q", out.String())
	}
}

func TestTeamsPipelineSubscriptionCommands(t *testing.T) {
	out := captureCLI(t)
	path := filepath.Join(t.TempDir(), "s.json")
	g := &cliGraph{post: obj("id", "sub-1", "resource", "communications/callRecords", "changeType", "created",
		"expirationDateTime", "2030-01-01T00:00:00Z", "notificationUrl", "https://hook")}
	useCLIGraph(t, g)
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "subscribe", "store_path", path, "change_type", "",
		"resource", " Communications/CallRecords ", "notification_url", "https://hook", "expiration", "2030-01-01T00:00:00Z",
		"client_state", "secret")); err != nil {
		t.Fatal(err)
	}
	body, _ := pyjson.Dumps(g.bodies[0])
	if body != `{"changeType": "created", "notificationUrl": "https://hook", "resource": "Communications/CallRecords", "expirationDateTime": "2030-01-01T00:00:00Z", "latestSupportedTlsVersion": "v1_2", "clientState": "secret"}` {
		t.Fatal(body)
	}
	store, _ := NewTeamsPipelineStore(path)
	if rec := store.GetSubscription("sub-1"); rec == nil || get(rec, "status") != "active" {
		t.Fatal(rec)
	}
	out.Reset()
	g.patch = obj("id", "sub-1", "resource", "communications/callRecords", "changeType", "created", "notificationUrl", "https://hook")
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "renew-subscription", "store_path", path,
		"subscription_id", "sub-1", "expiration", "2031-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	merged := decodeCLIJSON(t, out.String())
	if get(merged, "expirationDateTime") != "2031-01-01T00:00:00Z" || get(merged, "resource") != "communications/callRecords" {
		t.Fatal(out.String())
	}
	out.Reset()
	g.subs = []any{obj("id", "sub-1", "resource", "r", "changeType", "updated"), obj("resource", "x")}
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "subs", "store_path", path)); err != nil {
		t.Fatal(err)
	}
	want := "\n2 Microsoft Graph subscription(s):\n\n  ◆ sub-1\n    resource: r\n    changeType: updated\n\n" +
		"  ◆ unknown\n    resource: x\n    changeType: unknown\n\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
	out.Reset()
	g.deleted = map[string]any{"deleted": true, "status_code": int64(204)}
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "delete-subscription", "store_path", path, "subscription_id", "sub-1")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"subscription_id": "sub-1"`) || !strings.Contains(out.String(), `"status_code": 204`) {
		t.Fatal(out.String())
	}
	store, _ = NewTeamsPipelineStore(path)
	if store.GetSubscription("sub-1") != nil {
		t.Fatal("subscription not deleted")
	}
	want2 := []string{"POST /subscriptions", "PATCH /subscriptions/sub-1", "COLLECT /subscriptions", "DELETE /subscriptions/sub-1"}
	if !reflect.DeepEqual(g.calls, want2) {
		t.Fatal(g.calls)
	}
}

func TestTeamsPipelineDefaultChangeTypeForResource(t *testing.T) {
	for resource, want := range map[string]string{
		"communications/onlineMeetings/getAllTranscripts": "created", " COMMUNICATIONS/CALLRECORDS": "created",
		"communications/onlineMeetings/getAllRecordings": "created", "users/u/onlineMeetings/m": "updated", "": "updated"} {
		if got := defaultChangeTypeForResource(resource); got != want {
			t.Fatalf("%q -> %q", resource, got)
		}
	}
}

// Round tripper answering every token request with a fixed bearer token or a failure
type tokenTransport struct{ fail bool }

func (tt tokenTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if tt.fail {
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_client"}`)), Header: http.Header{}, Request: r}, nil
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"tok-12345","expires_in":3600,"token_type":"Bearer"}`)),
		Header: http.Header{"Content-Type": {"application/json"}}, Request: r}, nil
}

func TestTeamsPipelineTokenHealthForceRefresh(t *testing.T) {
	out := captureCLI(t)
	t.Setenv("MSGRAPH_TENANT_ID", "tenant")
	t.Setenv("MSGRAPH_CLIENT_ID", "client")
	t.Setenv("MSGRAPH_CLIENT_SECRET", "secret")
	fail := false
	prev := graphTokenProviderFromEnv
	graphTokenProviderFromEnv = func() (*messagetools.MicrosoftGraphTokenProvider, error) {
		return messagetools.MicrosoftGraphTokenProviderFromEnv(nil, messagetools.MicrosoftGraphTokenProviderOptions{Transport: tokenTransport{fail}})
	}
	t.Cleanup(func() { graphTokenProviderFromEnv = prev })
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "token", "force_refresh", true)); err != nil {
		t.Fatal(err)
	}
	p := decodeCLIJSON(t, out.String())
	if get(p, "last_refresh_succeeded") != true || get(p, "access_token_length") != int64(9) || get(p, "tenant_id") != "tenant" {
		t.Fatal(out.String())
	}
	out.Reset()
	fail = true
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "token-health", "force_refresh", true)); err != nil {
		t.Fatal(err)
	}
	p = decodeCLIJSON(t, out.String())
	if get(p, "last_refresh_succeeded") != false || get(p, "refresh_error") == nil {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := teamsPipelineCommand(makeCLIArgs("teams_pipeline_action", "token-health")); err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeCLIJSON(t, out.String()).Get("last_refresh_succeeded"); ok {
		t.Fatal(out.String())
	}
}

func TestTeamsPipelineRegisterCLIRoutesAliasesThroughDispatcher(t *testing.T) {
	out := captureCLI(t)
	root := &cobra.Command{Use: "teams-pipeline", SilenceUsage: true, SilenceErrors: true}
	registerCLI(root)
	var names []string
	for _, c := range root.Commands() {
		names = append(names, c.Name()+strings.Join(c.Aliases, ","))
	}
	if len(names) != len(teamsSubcommands) {
		t.Fatal(names)
	}
	path := filepath.Join(t.TempDir(), "s.json")
	root.SetArgs([]string{"ls", "--store-path", path, "--limit", "3"})
	if err := root.Execute(); err != nil || out.String() != "No Teams meeting pipeline jobs found.\n" {
		t.Fatal(err, out.String())
	}
	out.Reset()
	root.SetArgs([]string{"show", "missing", "--store-path", path})
	if err := root.Execute(); err != nil || out.String() != "Unknown job: missing\n" {
		t.Fatal(err, out.String())
	}
	out.Reset()
	root.SetArgs([]string{})
	if err := root.Execute(); exitCode(err) != 2 || !strings.HasPrefix(out.String(), "Usage: icebath teams-pipeline {list|") {
		t.Fatal(err, out.String())
	}
	root.SetArgs([]string{"subscribe", "--resource", "r"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "notification-url") {
		t.Fatal(err)
	}
}
