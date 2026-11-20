package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// An editor speaks the Agent Client Protocol over standard input and output for one prompt turn
func TestScenarioACPInitializeSessionPrompt(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Hello from the editor session."}}})
	acp := s.SpawnStep(Step{Args: []string{"acp"}, Pipe: true})
	acp.Send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}` + "\n")
	initialized := s.value(acp.Await(`(?m)^(\{.*"id":\s*1\b.*\})$`)[1])
	s.HasJSON(initialized, `{"jsonrpc": "2.0", "id": 1, "result": {"protocolVersion": 1}}`)
	if Dig(initialized, "result", "agentCapabilities") == nil || Dig(initialized, "result", "agentInfo", "name") == nil {
		s.Failf("initialize answered without agent capabilities or agent info: %v", initialized)
	}

	acp.Send(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"${WORK}","mcpServers":[]}}` + "\n")
	created := s.value(acp.Await(`(?m)^(\{.*"id":\s*2\b.*\})$`)[1])
	session, _ := Dig(created, "result", "sessionId").(string)
	if session == "" {
		s.Failf("session/new returned no sessionId: %v", created)
	}

	acp.Send(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"` + session + `","prompt":[{"type":"text","text":"Say hello"}]}}` + "\n")
	finished := s.value(acp.Await(`(?m)^(\{.*"id":\s*3\b.*\})$`)[1])
	s.HasJSON(finished, `{"id": 3, "result": {"stopReason": "end_turn"}}`)
	var spoken strings.Builder
	for _, line := range strings.Split(acp.stdout.String(), "\n") {
		if !strings.Contains(line, `"session/update"`) {
			continue
		}
		update := s.value(line)
		if Dig(update, "params", "sessionId") == session && Dig(update, "params", "update", "sessionUpdate") == "agent_message_chunk" {
			piece, _ := Dig(update, "params", "update", "content", "text").(string)
			spoken.WriteString(piece)
		}
	}
	if spoken.String() != "Hello from the editor session." {
		s.Failf("the agent message chunks read %q", spoken.String())
	}
	if Dig(s.LastMessages(), -1, "content") != "Say hello" {
		s.Failf("the prompt text did not reach the model")
	}
	acp.CloseInput()
	if ended := acp.Wait(); ended.Code != 0 {
		s.Failf("the ACP server exits 0 at the end of input, got %d\n%s", ended.Code, clip(ended.Stderr))
	}
}
