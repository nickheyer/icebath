package provider

import "testing"

func TestFallbackCandidateCallUsesEntryTimeout(t *testing.T) {
	ctx := auxMainTestHome(t, "auxiliary:\n  compression:\n    fallback_chain:\n      - provider: custom\n        timeout: 240\n")
	fbClient := auxScriptedCallClient("https://example.invalid/v1", "k", auxCallOK("ok"))
	resp, err := auxCallFallbackCandidate(ctx, fbClient, "deepseek-v4-flash", "fallback_chain[0](custom)", auxFallbackRequest{
		Task: "compression", Messages: []map[string]any{{"role": "user", "content": "hi"}}, EffectiveTimeout: 30.0, EffectiveExtraBody: map[string]any{},
	}, false)
	if err != nil || resp == nil {
		t.Fatalf("got (%v, %v)", resp, err)
	}
	fbClient.mu.Lock()
	defer fbClient.mu.Unlock()
	if len(fbClient.sent) != 1 || fbClient.sent[0]["timeout"] != 240.0 {
		t.Fatalf("sent %v", fbClient.sent)
	}
}
