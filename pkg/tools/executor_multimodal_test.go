package tools

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"
)

func screenshotEnvelope() map[string]any {
	return map[string]any{"_multimodal": true, "text_summary": "MEDIA: snapshot", "content": []any{
		map[string]any{"type": "text", "text": "snapshot" + strings.Repeat("x", 200_000) + " of the screen"},
		map[string]any{"type": "text", "text": "Image downscaled from 3000x2000 to 1500x1000; multiply any coordinates you report by 2.00"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"}},
	}}
}

func multimodalTexts(envelope map[string]any) []string {
	var texts []string
	for _, p := range envelope["content"].([]any) {
		if part, ok := p.(map[string]any); ok && part["type"] == "text" {
			texts = append(texts, part["text"].(string))
		}
	}
	return texts
}

func TestScaleNoteSurvivesSpillingAnOversizedToolText(t *testing.T) {
	host := &fakeExecutorHost{env: newTestToolEnv(t)}
	e := NewExecutor(host, NewDispatcher(NewRegistry()))
	bounded := e.persistMultimodalTextParts(context.Background(), screenshotEnvelope(), "mcp__srv__snap", "call-1", nil, DefaultBudget)
	texts := multimodalTexts(bounded)
	if len(texts) == 0 || len(texts[0]) >= 50_000 {
		t.Fatalf("tool text not spilled %d", len(texts[0]))
	}
	kept := false
	for _, s := range texts {
		kept = kept || strings.Contains(s, "multiply any coordinates you report by 2.00")
	}
	parts := bounded["content"].([]any)
	if !kept || parts[len(parts)-1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("coordinate map or image lost %v", texts)
	}
	tiny := DefaultBudget
	tiny.MCPResultSize = 50
	both := e.persistMultimodalTextParts(context.Background(), screenshotEnvelope(), "mcp__srv__snap", "call-2", nil, tiny)
	saved := regexp.MustCompile(`saved to: (\S+)`)
	paths := map[string]bool{}
	for _, s := range multimodalTexts(both) {
		if m := saved.FindStringSubmatch(s); m != nil {
			paths[m[1]] = true
		}
	}
	if len(paths) != 2 {
		t.Fatalf("spill files %v", paths)
	}
}

func TestAuthorizationGateTimeoutSafeAndExtendsWithConfig(t *testing.T) {
	env := approvalTestEnv(t, "overflow", map[string]any{"mode": "manual", "timeout": int64(1_000_000_000_000_000_000)}, nil)
	bound := authorizationGateLockTimeout(env)
	if bound <= 0 || bound > time.Duration(MaxSafeTimeoutSeconds*float64(time.Second)) {
		t.Fatalf("oversized config bound %v", bound)
	}
	gate := NewAuthorizationGateWith(env, AuthorizationGateOptions{LockTimeout: &bound})
	ran := false
	gate.Run(func() { ran = true })
	if !ran {
		t.Fatal("gate with the clamped bound never ran its callback")
	}
	env = approvalTestEnv(t, "overflow", map[string]any{"mode": "manual", "timeout": 3600}, nil)
	bound = authorizationGateLockTimeout(env)
	if bound <= time.Duration(authorizationGateLockTimeoutS*float64(time.Second)) || bound != time.Duration((3600+HumanWaitMarginSeconds)*float64(time.Second)) {
		t.Fatalf("configured bound %v", bound)
	}
}
