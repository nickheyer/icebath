package scenario

import (
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// When the primary endpoint keeps answering overloaded the turn is served by the configured fallback provider
func TestScenarioFallbackProviderServesTheTurn(t *testing.T) {
	overloaded := make([]any, 12)
	for i := range overloaded {
		overloaded[i] = fakellm.Error{Status: 503, Message: "the engine is overloaded"}
	}
	s := Start(t, Setup{Model: overloaded})
	s.Finding("SF-8")
	backup := s.AddModel("backup", []any{fakellm.Text{Text: "Answer from the backup."}}, fakellm.Options{})
	s.Write("config.yaml", s.Read("config.yaml")+"fallback_providers:\n  - provider: custom\n    model: backup-model\n    base_url: ${MODEL_backup}\n")

	turn := s.RunStep(Step{Args: []string{"chat", "-q", "Say hello", "-Q"}, Timeout: 4 * time.Minute})
	if turn.Code != 0 || turn.Stdout != "Answer from the backup.\n" {
		s.Failf("the turn exited %d after %d primary and %d fallback requests, want the answer of the fallback provider\n%s",
			turn.Code, len(s.Model.Requests()), len(backup.Requests()), turn.dump())
	}
	if len(s.Model.MainRequests()) == 0 {
		s.Failf("the primary endpoint was never tried")
	}
	served := backup.MainRequests()
	if len(served) != 1 || served[0]["model"] != "backup-model" {
		s.Failf("the fallback endpoint received %d requests, the first for model %v", len(served), Dig(served, 0, "model"))
	}
	if Dig(served[0]["messages"], -1, "content") != "Say hello" {
		s.Failf("the fallback request does not end with the question")
	}
}
