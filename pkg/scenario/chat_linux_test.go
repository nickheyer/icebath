package scenario

import (
	"regexp"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// A quiet one shot turn prints only the answer on standard output and the session id on standard error
func TestScenarioChatQuietTurn(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Hello from the scripted model"}}})
	r := s.Run("chat", "-q", "Say hello", "-Q").Exit(0)
	if r.Stdout != "Hello from the scripted model\n" {
		s.Failf("standard output of a quiet turn is the answer and a newline, got %q\n%s", r.Stdout, r.dump())
	}
	id := regexp.MustCompile(`(?m)^session_id: (\S+)$`).FindStringSubmatch(r.Stderr)
	if id == nil {
		s.Failf("standard error of a quiet turn ends with the session id line\n%s", r.dump())
	}
	msgs := s.LastMessages()
	if Dig(msgs, -1, "role") != "user" || Dig(msgs, -1, "content") != "Say hello" {
		s.Failf("the model did not receive the question as the last user message, got %v", Dig(msgs, -1))
	}
	if Dig(msgs, 0, "role") != "system" {
		s.Failf("the request does not open with the system prompt, got role %v", Dig(msgs, 0, "role"))
	}
	for _, log := range []string{"logs/agent.log", "logs/errors.log"} {
		if s.Exists(log) && strings.Contains(s.Read(log), "CLI cleanup memory shutdown failed") {
			s.Failf("%s reports a failed memory shutdown at exit", log)
		}
	}
	rows := s.Rows("state.db", "SELECT role, content FROM messages WHERE session_id = ? ORDER BY id", id[1])
	s.SameJSON(rows, `[{"role":"user","content":"Say hello"},{"role":"assistant","content":"Hello from the scripted model"}]`)
}
