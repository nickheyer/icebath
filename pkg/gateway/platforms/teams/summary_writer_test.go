package teams

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Graph client double recording each post and answering a fixed message
type recordingGraph struct {
	paths  []string
	bodies []any
}

func (g *recordingGraph) PostJSON(ctx context.Context, path string, jsonBody any, headers map[string]string) (any, error) {
	g.paths = append(g.paths, path)
	g.bodies = append(g.bodies, jsonBody)
	return pyjson.ObjectOf("id", "msg-123", "webUrl", "https://teams.example/messages/123"), nil
}

func makeSummaryPayload() SummaryPayload {
	return SummaryPayload{MeetingID: "meeting-123", Title: "Weekly Sync", Summary: "Discussed launch readiness.",
		KeyDecisions: []any{"Proceed with staged rollout."}, ActionItems: []any{"Send launch checklist."}, Risks: []any{"QA sign-off still pending."}}
}

func TestTeamsSummaryWriterGraphDeliveryPostsToChannel(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	graph := &recordingGraph{}
	writer := NewTeamsSummaryWriter(nil, graph, nil)
	result, err := writer.WriteSummary(context.Background(), makeSummaryPayload(),
		pyjson.ObjectOf("delivery_mode", "graph", "team_id", "team-1", "channel_id", "channel-1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.GetOr("target_type", nil) != "channel" || result.GetOr("message_id", nil) != "msg-123" {
		t.Fatalf("result = %v", result.Vals)
	}
	if len(graph.paths) != 1 || graph.paths[0] != "/teams/team-1/channels/channel-1/messages" {
		t.Fatalf("paths = %v", graph.paths)
	}
	body := graph.bodies[0].(*pyjson.Object).GetOr("body", nil).(*pyjson.Object)
	content, _ := body.GetOr("content", nil).(string)
	if body.GetOr("contentType", nil) != "html" || !strings.Contains(content, "Weekly Sync") {
		t.Fatalf("body = %v", body.Vals)
	}
}

// Transport answering every request with one status and recording the request body
type webhookTransport struct {
	status int
	url    string
	body   string
}

func (w *webhookTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(req.Body)
	w.url, w.body = req.URL.String(), string(raw)
	return &http.Response{StatusCode: w.status, Status: strconv.Itoa(w.status) + " " + http.StatusText(w.status), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("1")), Request: req}, nil
}

func TestTeamsSummaryWriterIncomingWebhookDeliveryAndRefusals(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	transport := &webhookTransport{status: 200}
	writer := NewTeamsSummaryWriter(nil, nil, transport)
	config := pyjson.ObjectOf("incoming_webhook_url", " https://hooks.example/abc ")
	result, err := writer.WriteSummary(ctx, makeSummaryPayload(), config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.GetOr("delivery_mode", nil) != "incoming_webhook" || result.GetOr("webhook_url", nil) != "https://hooks.example/abc" ||
		result.GetOr("status_code", nil) != 200 || result.GetOr("delivered", nil) != true {
		t.Fatalf("result = %v", result.Vals)
	}
	sent, err := pyjson.LoadsDict(transport.body)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := sent.GetOr("text", nil).(string)
	if !strings.HasPrefix(text, "**Weekly Sync**\n\n") || !strings.Contains(text, "- Proceed with staged rollout.") {
		t.Fatalf("markdown = %q", text)
	}
	existing := pyjson.ObjectOf("delivered", true, "marker", "kept")
	again, err := writer.WriteSummary(ctx, makeSummaryPayload(), config, existing)
	if err != nil || again == existing || again.GetOr("marker", nil) != "kept" {
		t.Fatalf("existing record = %v %v", again, err)
	}
	transport.status = 404
	if _, err := writer.WriteSummary(ctx, makeSummaryPayload(), config, nil); err == nil || !strings.HasPrefix(err.Error(), "Client error '404 Not Found' for url 'https://hooks.example/abc'") {
		t.Fatalf("status error = %v", err)
	}
	if _, err := writer.WriteSummary(ctx, makeSummaryPayload(), pyjson.NewObject(), nil); err == nil || err.Error() != "Teams delivery_mode must be 'incoming_webhook' or 'graph'." {
		t.Fatalf("mode error = %v", err)
	}
	withGraph := NewTeamsSummaryWriter(nil, &recordingGraph{}, nil)
	if _, err := withGraph.WriteSummary(ctx, makeSummaryPayload(), pyjson.ObjectOf("delivery_mode", "graph", "team_id", "t"), nil); err == nil ||
		err.Error() != "Graph delivery mode requires chat_id, or both team_id and channel_id." {
		t.Fatalf("target error = %v", err)
	}
}

func TestTeamsSummaryWriterImportDoesNotLoadCwdDotenv(t *testing.T) {
	const canary = "ICEBATH_TEAMS_DOTENV_CANARY"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(canary+"=leaked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("ICEBATH_HOME", t.TempDir())
	writer := NewTeamsSummaryWriter(nil, &recordingGraph{}, nil)
	if _, err := writer.WriteSummary(context.Background(), makeSummaryPayload(), pyjson.ObjectOf("delivery_mode", "graph", "chat_id", "19:chat"), nil); err != nil {
		t.Fatal(err)
	}
	if _, leaked := os.LookupEnv(canary); leaked || writer == nil {
		t.Fatal("the working directory .env was loaded into the process env")
	}
}
