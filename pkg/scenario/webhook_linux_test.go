package scenario

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/fakellm"
)

// A subscribed route accepts a signed POST, turns its payload into an agent prompt and refuses a bad signature
func TestScenarioWebhookSubscribeAndDeliver(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Noted the build."}}})
	const secret = "scenario-route-secret-0123456789"
	s.Run("webhook", "list").Exit(1).Has("Webhook platform is not enabled. To set it up:", "WEBHOOK_ENABLED=true")
	s.Port("hook")
	s.Write("config.yaml", s.Read("config.yaml")+"platforms:\n  webhook:\n    enabled: true\n    extra:\n      port: ${PORT_hook}\n      secret: scenario-global-secret-0123456789\n")
	s.Set("HOOK", "http://127.0.0.1:${PORT_hook}")
	s.Run("webhook", "list").Exit(0).Has("No dynamic webhook subscriptions.", "Create one with: icebath webhook subscribe <name>")

	made := s.Run("webhook", "subscribe", "build-done", "--prompt", "Build {repository.name} finished with {status}", "--secret", secret).Exit(0)
	made.Has("Created webhook subscription: build-done", "URL:    http://localhost:${PORT_hook}/webhooks/build-done", "Profile: default", "Secret: "+secret, "Events: (all)", "Deliver: log",
		"Prompt: Build {repository.name} finished with {status}", "Use the secret for HMAC-SHA256 signature validation.")
	s.Run("webhook", "subscribe", "bad!name").Exit(1).Has("Error: Invalid name 'bad!name'. Use lowercase alphanumeric with hyphens/underscores.")
	s.Run("webhook", "list").Exit(0).Has("1 webhook subscription(s):", "◆ build-done", "/webhooks/build-done", "Events:", "(all)", "Deliver:", "log")

	gw := s.Spawn("gateway", "run")
	s.WaitHTTP("${HOOK}/health")
	s.Eventually("gateway_state.json reporting running", waitLimit, func() bool {
		var state map[string]any
		raw, err := os.ReadFile(s.Path("gateway_state.json"))
		return err == nil && json.Unmarshal(raw, &state) == nil && state["gateway_state"] == "running"
	})
	s.SameJSON(s.HTTP("GET", "${HOOK}/health", nil).Code(200).Body, `{"status": "ok", "platform": "webhook"}`)

	body := `{"repository": {"name": "icebath"}, "status": "green"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	s.HasJSON(s.HTTP("POST", "${HOOK}/webhooks/build-done", body, "Content-Type", "application/json", "X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64)).Code(401).Body,
		`{"error": "Invalid signature"}`)
	s.HasJSON(s.HTTP("POST", "${HOOK}/webhooks/no-such-route", body, "Content-Type", "application/json", "X-Hub-Signature-256", signature).Code(404).Body,
		`{"error": "Unknown route: no-such-route"}`)
	if len(s.Model.Requests()) != 0 {
		s.Failf("a refused delivery reached the model")
	}
	accepted := s.HTTP("POST", "${HOOK}/webhooks/build-done", body, "Content-Type", "application/json", "X-Hub-Signature-256", signature,
		"X-GitHub-Delivery", "scenario-delivery-1").Code(202)
	s.HasJSON(accepted.Body, `{"status": "accepted", "route": "build-done", "delivery_id": "scenario-delivery-1"}`)
	if err := s.Model.WaitForRequests(1, 2*time.Minute, "main"); err != nil {
		s.Failf("the accepted delivery never reached the model: %v", err)
	}
	prompted := false
	for _, m := range s.LastMessages() {
		text, _ := Dig(m, "content").(string)
		prompted = prompted || (Dig(m, "role") == "user" && strings.Contains(text, "Build icebath finished with green"))
	}
	if !prompted {
		s.Failf("the rendered prompt did not reach the model as a user message")
	}
	s.HasJSON(s.HTTP("POST", "${HOOK}/webhooks/build-done", body, "Content-Type", "application/json", "X-Hub-Signature-256", signature,
		"X-GitHub-Delivery", "scenario-delivery-1").Code(200).Body, `{"status": "duplicate", "delivery_id": "scenario-delivery-1"}`)
	gw.Stop()

	s.Run("webhook", "remove", "build-done").Exit(0).Has("Removed webhook subscription: build-done")
	s.Run("webhook", "remove", "build-done").Exit(0).Has("No subscription named 'build-done'.", "Static routes from config.yaml cannot be removed here.")
	s.Run("webhook", "list").Exit(0).Has("No dynamic webhook subscriptions.")
}
