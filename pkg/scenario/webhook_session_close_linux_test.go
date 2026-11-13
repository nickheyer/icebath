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

// An admitted webhook delivery runs its one shot session to the end and closes it, a refused one opens nothing
func TestScenarioWebhookSessionCloseAfterDelivery(t *testing.T) {
	s := Start(t, Setup{Model: []any{fakellm.Text{Text: "Deploy recorded."}}})
	const secret = "scenario-close-secret-0123456789"
	s.Port("hook")
	s.Write("config.yaml", s.Read("config.yaml")+"platforms:\n  webhook:\n    enabled: true\n    extra:\n      port: ${PORT_hook}\n"+
		"      secret: scenario-global-secret-0123456789\n      routes:\n        deploy:\n          secret: "+secret+"\n"+
		"          prompt: \"Deploy of {service} is {state}\"\n")
	s.Set("HOOK", "http://127.0.0.1:${PORT_hook}")

	gw := s.Spawn("gateway", "run")
	s.WaitHTTP("${HOOK}/health")
	s.Eventually("gateway_state.json reporting running", 60*time.Second, func() bool {
		var state map[string]any
		raw, err := os.ReadFile(s.Path("gateway_state.json"))
		return err == nil && json.Unmarshal(raw, &state) == nil && state["gateway_state"] == "running"
	})

	body := `{"service": "billing", "state": "live"}`
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	signature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	closed := func() []map[string]any {
		return s.Rows("state.db", "SELECT id FROM sessions WHERE end_reason = ? AND ended_at IS NOT NULL", "webhook_complete")
	}

	s.HasJSON(s.HTTP("POST", "${HOOK}/webhooks/deploy", body, "Content-Type", "application/json", "X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64),
		"X-GitHub-Delivery", "close-refused").Code(401).Body, `{"error": "Invalid signature"}`)
	if len(s.Model.Requests()) != 0 {
		s.Failf("a refused delivery reached the model")
	}

	accepted := s.HTTP("POST", "${HOOK}/webhooks/deploy", body, "Content-Type", "application/json", "X-Hub-Signature-256", signature,
		"X-GitHub-Delivery", "close-1").Code(202)
	s.HasJSON(accepted.Body, `{"status": "accepted", "route": "deploy", "delivery_id": "close-1"}`)
	if err := s.Model.WaitForRequests(1, 2*time.Minute, "main"); err != nil {
		s.Failf("the accepted delivery never reached the model: %v", err)
	}
	prompted := false
	for _, m := range s.LastMessages() {
		text, _ := Dig(m, "content").(string)
		prompted = prompted || (Dig(m, "role") == "user" && strings.Contains(text, "Deploy of billing is live"))
	}
	if !prompted {
		s.Failf("the rendered prompt did not reach the model as a user message")
	}
	s.Eventually("the delivery's session closed as webhook_complete", 90*time.Second, func() bool { return len(closed()) == 1 })

	s.HasJSON(s.HTTP("POST", "${HOOK}/webhooks/deploy", body, "Content-Type", "application/json", "X-Hub-Signature-256", signature,
		"X-GitHub-Delivery", "close-1").Code(200).Body, `{"status": "duplicate", "delivery_id": "close-1"}`)
	if len(closed()) != 1 || len(s.Model.MainRequests()) != 1 {
		s.Failf("the provider's retry of a settled delivery ran again: %d closed sessions, %d model turns", len(closed()), len(s.Model.MainRequests()))
	}
	gw.Stop()
}
