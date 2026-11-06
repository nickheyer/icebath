package provider

import "testing"

func TestRuntimeCredentialPoolSetOnlyByPoolPaths(t *testing.T) {
	pool := &Pool{}
	r := testResolver(t, nil, &fakeCreds{creds: map[string]Credential{"openrouter": {APIKey: "sk-or-pooled-key-12345", Source: "manual", Pool: pool}}}, "")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.CredentialPool != pool || rt.APIKey != "sk-or-pooled-key-12345" {
		t.Fatalf("pool entry runtime %+v", rt)
	}
	r = testResolver(t, nil, &fakeCreds{}, "OPENROUTER_API_KEY=sk-or-env-key-12345\n")
	if rt := mustResolve(t, r, "openrouter", "", "", ""); rt.CredentialPool != nil || rt.APIKey != "sk-or-env-key-12345" {
		t.Fatalf("env runtime carries a pool %+v", rt)
	}
	cfg := map[string]any{"providers": map[string]any{"local-ollama": map[string]any{"api": "http://localhost:11434/v1"}}}
	r = testResolver(t, cfg, &fakeCreds{creds: map[string]Credential{"local-ollama": {APIKey: "sk-genuinely-long-real-key-12345", Pool: pool}}}, "")
	if rt := r.tryResolveFromCustomPool("http://localhost:11434/v1", "custom", "", ""); rt == nil || rt.CredentialPool != pool || rt.Source != "pool:local-ollama" {
		t.Fatalf("custom pool runtime %+v", rt)
	}
	if rt := mustResolve(t, r, "custom", "sk-explicit-key-12345", "http://127.0.0.1:18080/v1", "m"); rt.CredentialPool != nil {
		t.Fatalf("explicit endpoint runtime carries a pool %+v", rt)
	}
}

func TestPoolCredentialsHandBackTheSelectingPool(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"openrouter": []any{
		map[string]any{"id": "p1", "label": "pool", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-or-pooled-key-12345"},
	}}})
	creds := NewPoolCredentials(testEnv(t, h))
	cred, ok := creds.CredentialForModel("openrouter", "")
	if !ok || cred.Pool == nil || cred.Pool != creds.Pool("openrouter") {
		t.Fatalf("credential %+v %v", cred, ok)
	}
	direct, ok := cred.Pool.Credential("openrouter")
	if !ok || direct.Pool != cred.Pool {
		t.Fatalf("pool credential %+v %v", direct, ok)
	}
}
