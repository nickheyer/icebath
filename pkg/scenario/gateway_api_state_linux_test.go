package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// A stored response replays its tool call, chains a second request onto its history, is fetched and deleted
func TestScenarioGatewayAPIResponses(t *testing.T) {
	s := Start(t, Setup{Model: []any{
		fakellm.ToolCall{Name: "terminal", Args: map[string]any{"command": "echo alpha.txt beta.txt"}},
		fakellm.Text{Text: "Two files."}, fakellm.Text{Text: "Still two."},
	}})
	gw := startAPIGateway(s)
	auth := []string{"Authorization", "Bearer " + apiKey}
	first := map[string]any{"model": "scenario-agent", "input": "What files are here?", "instructions": "You are terse.", "store": true}
	s.HTTP("POST", "${API}/v1/responses", first).Code(401)
	reply := s.HTTP("POST", "${API}/v1/responses", first, auth...).Code(200)
	s.HasJSON(reply.Body, `{"object": "response", "status": "completed", "model": "scenario-agent", "output": [
		{"type": "function_call", "status": "completed", "name": "terminal"},
		{"type": "function_call_output", "status": "completed"},
		{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Two files."}]}]}`)
	answer := reply.JSON()
	id, _ := Dig(answer, "id").(string)
	if !strings.HasPrefix(id, "resp_") {
		s.Failf("response id %q does not start with resp_", id)
	}
	if Dig(answer, "output", 0, "call_id") != Dig(answer, "output", 1, "call_id") {
		s.Failf("the tool call and its output carry different call ids")
	}
	if out, _ := Dig(answer, "output", 1, "output").(string); !strings.Contains(out, "alpha.txt beta.txt") {
		s.Failf("the replayed tool output is %q", out)
	}
	if command := s.value(Dig(answer, "output", 0, "arguments")); Dig(command, "command") != "echo alpha.txt beta.txt" {
		s.Failf("the replayed tool arguments are %v", Dig(answer, "output", 0, "arguments"))
	}

	second := s.HTTP("POST", "${API}/v1/responses", map[string]any{"model": "scenario-agent", "input": "And now?", "previous_response_id": id}, auth...).Code(200)
	s.HasJSON(second.Body, `{"status": "completed", "output": [{"type": "message", "content": [{"type": "output_text", "text": "Still two."}]}]}`)
	var roles []string
	for _, m := range s.LastMessages() {
		role, _ := Dig(m, "role").(string)
		roles = append(roles, role)
	}
	if strings.Join(roles, " ") != "system user assistant tool assistant user" {
		s.Failf("the chained request carried the roles %v, want the stored history then the new input", roles)
	}
	if Dig(s.LastMessages(), 1, "content") != "What files are here?" || Dig(s.LastMessages(), -1, "content") != "And now?" {
		s.Failf("the chained request lost the first input or the new one")
	}

	s.HasJSON(s.HTTP("GET", "${API}/v1/responses/"+id, nil, auth...).Code(200).Body, `{"id": "`+id+`", "object": "response", "status": "completed"}`)
	s.HTTP("DELETE", "${API}/v1/responses/"+id, nil, auth...).Code(200)
	s.HTTP("GET", "${API}/v1/responses/"+id, nil, auth...).Code(404)
	s.HTTP("POST", "${API}/v1/responses", map[string]any{"model": "scenario-agent", "input": "Again", "previous_response_id": id}, auth...).Code(404)
	gw.Stop()
}

// A session is created over REST, renamed, given one turn, read back with its messages and deleted
func TestScenarioGatewayAPISessions(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Session turn answer."}}})
	gw := startAPIGateway(s)
	auth := []string{"Authorization", "Bearer " + apiKey}
	s.HTTP("GET", "${API}/api/sessions", nil).Code(401)
	created := s.HTTP("POST", "${API}/api/sessions", map[string]any{"title": "API made"}, auth...).Code(201)
	s.HasJSON(created.Body, `{"object": "hermes.session", "session": {"source": "api_server", "title": "API made", "message_count": 0}}`)
	id, _ := Dig(created.JSON(), "session", "id").(string)
	if !strings.HasPrefix(id, "api_") {
		s.Failf("session id %q does not start with api_", id)
	}
	s.HasJSON(s.HTTP("GET", "${API}/api/sessions", nil, auth...).Code(200).Body, `{"object": "list", "data": [{"id": "`+id+`", "title": "API made"}]}`)
	s.HasJSON(s.HTTP("PATCH", "${API}/api/sessions/"+id, map[string]any{"title": "Renamed by API"}, auth...).Code(200).Body,
		`{"object": "hermes.session", "session": {"id": "`+id+`", "title": "Renamed by API"}}`)

	turn := s.HTTP("POST", "${API}/api/sessions/"+id+"/chat", map[string]any{"input": "One session turn"}, auth...).Code(200)
	s.HasJSON(turn.Body, `{"object": "hermes.session.chat.completion", "status": "completed", "session_id": "`+id+`",
		"message": {"role": "assistant", "content": "Session turn answer."}}`)
	if Dig(s.LastMessages(), -1, "content") != "One session turn" {
		s.Failf("the session turn input did not reach the model")
	}
	history := s.HTTP("GET", "${API}/api/sessions/"+id+"/messages", nil, auth...).Code(200).JSON()
	s.HasJSON(history, `{"object": "list", "session_id": "`+id+`"}`)
	var spoken []string
	for _, m := range Dig(history, "data").([]any) {
		role, _ := Dig(m, "role").(string)
		text, _ := Dig(m, "content").(string)
		if role == "user" || role == "assistant" {
			spoken = append(spoken, role+": "+text)
		}
	}
	if strings.Join(spoken, " | ") != "user: One session turn | assistant: Session turn answer." {
		s.Failf("the stored conversation reads %v", spoken)
	}
	s.HasJSON(s.HTTP("GET", "${API}/api/sessions/"+id, nil, auth...).Code(200).Body, `{"session": {"id": "`+id+`", "title": "Renamed by API", "model": "fake-model"}}`)
	s.Run("sessions", "list", "--source", "api_server").Exit(0).Has(id)

	s.SameJSON(s.HTTP("DELETE", "${API}/api/sessions/"+id, nil, auth...).Code(200).Body, `{"object": "hermes.session.deleted", "id": "`+id+`", "deleted": true}`)
	s.HTTP("GET", "${API}/api/sessions/"+id, nil, auth...).Code(404)
	gw.Stop()
}

// A scheduled job is created over REST, listed, paused, resumed, updated and deleted, each answer carrying the job
func TestScenarioGatewayAPIJobs(t *testing.T) {
	s := Start(t, Setup{})
	s.Finding("SF-5")
	gw := startAPIGateway(s)
	auth := []string{"Authorization", "Bearer " + apiKey}
	s.HTTP("GET", "${API}/api/jobs", nil).Code(401)
	created := s.HTTP("POST", "${API}/api/jobs", map[string]any{"name": "api-job", "schedule": "0 4 * * *", "prompt": "tidy"}, auth...).Code(200)
	s.HasJSON(created.Body, `{"job": {"name": "api-job", "prompt": "tidy", "enabled": true, "schedule": {"kind": "cron", "expr": "0 4 * * *"}}}`)
	id, _ := Dig(created.JSON(), "job", "id").(string)
	s.HasJSON(s.Read("cron/jobs.json"), `{"jobs": [{"id": "`+id+`", "name": "api-job"}]}`)
	s.HasJSON(s.HTTP("GET", "${API}/api/jobs", nil, auth...).Code(200).Body, `{"jobs": [{"id": "`+id+`", "name": "api-job"}]}`)
	s.HasJSON(s.HTTP("GET", "${API}/api/jobs/"+id, nil, auth...).Code(200).Body, `{"job": {"id": "`+id+`"}}`)
	s.HasJSON(s.HTTP("POST", "${API}/api/jobs/"+id+"/pause", nil, auth...).Code(200).Body, `{"job": {"id": "`+id+`", "enabled": false, "state": "paused"}}`)
	s.HasJSON(s.HTTP("POST", "${API}/api/jobs/"+id+"/resume", nil, auth...).Code(200).Body, `{"job": {"id": "`+id+`", "enabled": true, "state": "scheduled"}}`)
	s.HasJSON(s.HTTP("PATCH", "${API}/api/jobs/"+id, map[string]any{"prompt": "tidy more"}, auth...).Code(200).Body, `{"job": {"id": "`+id+`", "prompt": "tidy more"}}`)
	s.Run("cron", "list").Exit(0).Has(id, "api-job")
	s.HTTP("GET", "${API}/api/jobs/not-a-job-id", nil, auth...).Code(400)
	s.HTTP("DELETE", "${API}/api/jobs/"+id, nil, auth...).Code(200)
	s.HTTP("GET", "${API}/api/jobs/"+id, nil, auth...).Code(404)
	s.SameJSON(Dig(s.value(s.Read("cron/jobs.json")), "jobs"), `[]`)
	gw.Stop()
}
