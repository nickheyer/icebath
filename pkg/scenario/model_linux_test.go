package scenario

import (
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// Returns the model name and the bearer token of the newest request a scripted endpoint received
func lastRoute(s *S, m *fakellm.Server) (string, string) {
	s.t.Helper()
	reqs := m.Requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Kind == "main" {
			model, _ := reqs[i].Body["model"].(string)
			return model, reqs[i].Auth
		}
	}
	s.Failf("the endpoint received no request with tools")
	return "", ""
}

// The model and the provider of one run come from the config, the flags, a named provider or a launch only endpoint
func TestScenarioModelAndProviderSelection(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "default route"}, fakellm.Text{Text: "flag model"}}})
	alt := s.AddModel("alt", []any{fakellm.Text{Text: "named provider"}}, fakellm.Options{})
	launch := s.AddModel("launch", []any{fakellm.Text{Text: "launch endpoint"}}, fakellm.Options{})
	s.Write("config.yaml", s.Read("config.yaml")+"providers:\n  alt:\n    api: ${MODEL_alt}\n    api_key: sk-alt-key\n    default_model: alt-model\n")
	config := s.Read("config.yaml")

	plain := s.Run("-z", "one").Exit(0)
	if plain.Stdout != "default route\n" || plain.Stderr != "" {
		s.Failf("-z prints the final answer and nothing else on either stream\n%s", plain.dump())
	}
	if model, auth := lastRoute(s, s.Model); model != "fake-model" || auth != "Bearer no-key-required" {
		s.Failf("the default route used model %q with %q, OPENAI_API_KEY belongs to openai.com and a keyless custom host gets the stand-in", model, auth)
	}

	usage := s.Work + "/usage.json"
	s.Run("-z", "two", "-m", "flag-model", "--usage-file", usage).Exit(0)
	if model, _ := lastRoute(s, s.Model); model != "flag-model" {
		s.Failf("-m flag-model sent model %q", model)
	}
	s.HasJSON(s.Read(usage), `{"model": "flag-model", "provider": "custom", "api_calls": 1, "completed": true, "failed": false}`)
	for _, key := range []string{"estimated_cost_usd", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens",
		"reasoning_tokens", "total_tokens", "session_id", "total_including_auxiliary", "auxiliary"} {
		if _, ok := s.value(s.Read(usage)).(map[string]any)[key]; !ok {
			s.Failf("the usage report lacks %s\n%s", key, s.Read(usage))
		}
	}

	named := s.Run("-z", "three", "--provider", "alt", "--model", "alt-model").Exit(0)
	if named.Stdout != "named provider\n" {
		s.Failf("--provider alt printed %q\n%s", named.Stdout, named.dump())
	}
	if model, auth := lastRoute(s, alt); model != "alt-model" || auth != "Bearer sk-alt-key" {
		s.Failf("the named provider received model %q with %q", model, auth)
	}

	direct := s.Run("chat", "--provider", "custom", "--base-url", "${MODEL_launch}", "--api-key", "sk-launch-only", "--model", "launch-model", "-q", "four", "-Q").Exit(0)
	if direct.Stdout != "launch endpoint\n" {
		s.Failf("the launch endpoint run printed %q\n%s", direct.Stdout, direct.dump())
	}
	if model, auth := lastRoute(s, launch); model != "launch-model" || auth != "Bearer sk-launch-only" {
		s.Failf("the launch endpoint received model %q with %q", model, auth)
	}
	if len(s.Model.MainRequests()) != 2 {
		s.Failf("the configured endpoint received %d turns, want the two routed to it", len(s.Model.MainRequests()))
	}
	if s.Read("config.yaml") != config {
		s.Failf("per run overrides changed config.yaml\n%s", s.Read("config.yaml"))
	}
	for _, file := range []string{".env", "config.yaml"} {
		if strings.Contains(s.Read(file), "sk-launch-only") {
			s.Failf("the launch only key was written to %s", file)
		}
	}
}
