package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// A provider that refuses the key ends a quiet turn with exit 1 and an explanation, a scripted one shot with exit 2
func TestScenarioChatProviderFailureExitCodes(t *testing.T) {
	refusals := make([]any, 12)
	for i := range refusals {
		refusals[i] = fakellm.Error{Status: 401, Message: "invalid api key for scenario"}
	}
	s := Start(t, Setup{Model: refusals})
	quiet := s.Run("chat", "-q", "Say hello", "-Q").Exit(1)
	for _, part := range []string{"custom rejected your API key, so the model can't be reached.", "run `icebath setup` in a terminal.",
		"Provider said: ", "invalid api key for scenario"} {
		if !strings.Contains(quiet.Stdout, part) {
			s.Failf("the failure explanation on standard output lacks %q\n%s", part, quiet.dump())
		}
	}
	quiet.Match(`(?m)^session_id: \S+$`)

	s.Model.DropUnconsumed()
	s.Model.Push(refusals...)
	usage := s.Work + "/usage.json"
	s.Run("-z", "Say hello", "--usage-file", usage).Exit(2)
	s.HasJSON(s.Read(usage), `{"completed": false, "failed": true, "provider": "custom", "model": "fake-model"}`)

	s.Model.DropUnconsumed()
	s.Model.Push(fakellm.Text{Text: "Recovered."})
	if ok := s.Run("-z", "Say hello").Exit(0); ok.Stdout != "Recovered.\n" {
		s.Failf("the endpoint recovered but -z printed %q", ok.Stdout)
	}
}
