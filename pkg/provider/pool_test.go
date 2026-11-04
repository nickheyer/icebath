package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func authWithPool(provider string, rows ...map[string]any) map[string]any {
	list := make([]any, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	return map[string]any{"version": 1, "credential_pool": map[string]any{provider: list}}
}

func TestExplicitResetTimestampOverridesDefault429TTL(t *testing.T) {
	h := newTestHome(t)
	now := nowEpoch()
	writeAuthStore(t, h, authWithPool("openai-codex", row(map[string]any{
		"id": "cred-1", "label": "weekly-reset", "auth_type": "oauth", "priority": 0, "source": "manual:device_code",
		"access_token": "tok-1", "last_status": "exhausted", "last_status_at": now - 7200, "last_error_code": 429,
		"last_error_reason": "device_code_exhausted", "last_error_reset_at": now + 7*24*3600})))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	if pool.HasAvailable("") {
		t.Fatal("entry benched by an explicit reset must not be available")
	}
	if _, ok := pool.Select(""); ok {
		t.Fatal("select must return nothing")
	}
}

func TestBillingRotationMarksAllEntriesSharingFailedKey(t *testing.T) {
	h := newTestHome(t)
	shared := "sk-deepseek-shared"
	writeAuthStore(t, h, authWithPool("custom",
		row(map[string]any{"id": "cred-explicit", "label": "520555", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": shared, "base_url": "https://api.deepseek.com"}),
		row(map[string]any{"id": "cred-model-config", "label": "model_config", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": shared, "base_url": "https://api.deepseek.com"})))
	pool := LoadPool(testEnv(t, h), "custom")
	if _, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 402, APIKeyHint: shared}); ok {
		t.Fatal("rotation must converge to nothing when every entry shares the depleted key")
	}
	statuses := statusesOf(pool)
	if statuses["cred-explicit"] != StatusExhausted || statuses["cred-model-config"] != StatusExhausted {
		t.Fatalf("both entries must be exhausted: %v", statuses)
	}
}

func TestStaleCredentialIDPrefersAPIKeyHint(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("anthropic",
		row(map[string]any{"id": "cred-primary", "label": "primary", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-ant-api-primary"}),
		row(map[string]any{"id": "cred-backup", "label": "backup", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "sk-ant-api-backup"})))
	pool := LoadPool(testEnv(t, h), "anthropic")
	next, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 429, APIKeyHint: "sk-ant-api-primary", CredentialID: "cred-backup"})
	statuses := statusesOf(pool)
	if statuses["cred-primary"] != StatusExhausted {
		t.Fatalf("the key that failed must be benched: %v", statuses)
	}
	if statuses["cred-backup"] == StatusExhausted {
		t.Fatalf("the healthy backup must not inherit the 429: %v", statuses)
	}
	if ok && (next.ID != "cred-backup" || next.RuntimeAPIKey() != "sk-ant-api-backup") {
		t.Fatalf("rotation must hand the healthy backup, got %+v", next)
	}
}

func TestUnmatchedAPIKeyHintRotatesWithoutBenchingInnocentKey(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("anthropic",
		row(map[string]any{"id": "cred-1", "label": "primary", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-ant-api-primary"}),
		row(map[string]any{"id": "cred-2", "label": "secondary", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "sk-ant-api-secondary"})))
	pool := LoadPool(testEnv(t, h), "anthropic")
	if _, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 429, APIKeyHint: "sk-ant-api-rotated-away"}); !ok {
		t.Fatal("a fresh selection must still be handed back")
	}
	for id, status := range statusesOf(pool) {
		if status == StatusExhausted || status == StatusDead {
			t.Fatalf("innocent key %s was benched", id)
		}
	}
	for _, persisted := range poolRows(t, h, "anthropic") {
		if persisted["last_status"] == StatusExhausted || persisted["last_status"] == StatusDead || persisted["last_error_code"] != nil {
			t.Fatalf("innocent key benched on disk: %v", persisted)
		}
	}
}

func codexOAuthRows() []map[string]any {
	return []map[string]any{
		row(map[string]any{"id": "cred-dead", "label": "revoked", "auth_type": "oauth", "priority": 0, "source": "manual:device_code", "access_token": "revoked-at", "refresh_token": "revoked-rt"}),
		row(map[string]any{"id": "cred-ok", "label": "healthy", "auth_type": "oauth", "priority": 1, "source": "manual:device_code", "access_token": "healthy-at", "refresh_token": "healthy-rt"}),
	}
}

func TestTokenInvalidatedMarksCredentialDead(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openai-codex", codexOAuthRows()...))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	if mustSelect(t, pool, "").ID != "cred-dead" {
		t.Fatal("the first entry must be selected first")
	}
	next, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 401, ErrorContext: map[string]any{
		"reason": "token_invalidated", "message": "Your authentication token has been invalidated. Please try signing in again."}})
	if !ok || next.ID != "cred-ok" {
		t.Fatalf("rotation must hand off to the healthy credential, got %+v %t", next, ok)
	}
	persisted := poolRows(t, h, "openai-codex")[0]
	if persisted["last_status"] != StatusDead || persisted["last_error_code"] != float64(401) || persisted["last_error_reason"] != "token_invalidated" {
		t.Fatalf("revoked credential must be DEAD on disk: %v", persisted)
	}
}

func TestDeadCredentialNeverReentersRotationAfterTTL(t *testing.T) {
	h := newTestHome(t)
	rows := codexOAuthRows()
	rows[0]["last_status"] = "dead"
	rows[0]["last_status_at"] = nowEpoch() - 2*3600
	rows[0]["last_error_code"] = 401
	rows[0]["last_error_reason"] = "token_invalidated"
	writeAuthStore(t, h, authWithPool("openai-codex", rows...))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	if mustSelect(t, pool, "").ID != "cred-ok" {
		t.Fatal("the dead entry must be skipped")
	}
	if rowByID(poolRows(t, h, "openai-codex"), "cred-dead")["last_status"] != StatusDead {
		t.Fatal("the DEAD entry must stay dead on disk")
	}
}

func TestRateLimit429StillUsesExhaustedNotDead(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openai-codex",
		row(map[string]any{"id": "cred-1", "label": "primary", "auth_type": "oauth", "priority": 0, "source": "manual:device_code", "access_token": "at-1", "refresh_token": "rt-1"}),
		row(map[string]any{"id": "cred-2", "label": "secondary", "auth_type": "oauth", "priority": 1, "source": "manual:device_code", "access_token": "at-2", "refresh_token": "rt-2"})))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	if mustSelect(t, pool, "").ID != "cred-1" {
		t.Fatal("cred-1 first")
	}
	next, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 429, ErrorContext: map[string]any{"reason": "rate_limit_exceeded", "message": "Rate limit exceeded"}})
	if !ok || next.ID != "cred-2" {
		t.Fatalf("expected rotation to cred-2, got %+v", next)
	}
	persisted := poolRows(t, h, "openai-codex")[0]
	if persisted["last_status"] != StatusExhausted || persisted["last_error_code"] != float64(429) {
		t.Fatalf("429 must stay exhausted: %v", persisted)
	}
}

func TestGeneric401WithoutTerminalReasonStillUsesExhausted(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openai-codex",
		row(map[string]any{"id": "cred-1", "label": "primary", "auth_type": "oauth", "priority": 0, "source": "manual:device_code", "access_token": "at-1", "refresh_token": "rt-1"}),
		row(map[string]any{"id": "cred-2", "label": "secondary", "auth_type": "oauth", "priority": 1, "source": "manual:device_code", "access_token": "at-2", "refresh_token": "rt-2"})))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	pool.Select("")
	pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 401, ErrorContext: map[string]any{"message": "Unauthorized"}})
	persisted := poolRows(t, h, "openai-codex")[0]
	if persisted["last_status"] != StatusExhausted || persisted["last_error_code"] != float64(401) {
		t.Fatalf("generic 401 must stay exhausted: %v", persisted)
	}
}

func TestDeadManualEntryPrunedAfter24h(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openai-codex",
		row(map[string]any{"id": "cred-old-dead", "label": "ancient-dead", "auth_type": "oauth", "priority": 0, "source": "manual:device_code",
			"access_token": "stale", "refresh_token": "stale", "last_status": "dead", "last_status_at": nowEpoch() - 25*3600, "last_error_code": 401, "last_error_reason": "token_invalidated"}),
		row(map[string]any{"id": "cred-ok", "label": "healthy", "auth_type": "oauth", "priority": 1, "source": "manual:device_code", "access_token": "healthy-at", "refresh_token": "healthy-rt"})))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	if mustSelect(t, pool, "").ID != "cred-ok" {
		t.Fatal("healthy entry expected")
	}
	rows := poolRows(t, h, "openai-codex")
	if len(rows) != 1 || rows[0]["id"] != "cred-ok" {
		t.Fatalf("dead manual entry must be pruned on disk: %v", rows)
	}
}

func TestLoadPoolSeedsEnvAPIKey(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-seeded")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), "")
	if entry.Source != "env:OPENROUTER_API_KEY" || entry.AccessToken != "sk-or-seeded" {
		t.Fatalf("env key not seeded: %+v", entry)
	}
}

func TestLoadPoolDoesNotPersistEnvSeededSecretValue(t *testing.T) {
	h := newTestHome(t)
	sentinel := "S3NTINEL_DO_NOT_PERSIST_OPENROUTER"
	t.Setenv("OPENROUTER_API_KEY", sentinel)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), "")
	if entry.Source != "env:OPENROUTER_API_KEY" || entry.AccessToken != sentinel {
		t.Fatalf("unexpected entry %+v", entry)
	}
	raw, _ := json.Marshal(readAuthStore(t, h))
	if strings.Contains(string(raw), sentinel) {
		t.Fatal("secret value must not reach auth.json")
	}
	persisted := poolRows(t, h, "openrouter")[0]
	if persisted["source"] != "env:OPENROUTER_API_KEY" || persisted["label"] != "OPENROUTER_API_KEY" || persisted["auth_type"] != "api_key" || persisted["priority"] != float64(0) {
		t.Fatalf("unexpected persisted row %v", persisted)
	}
	if _, has := persisted["access_token"]; has {
		t.Fatal("access_token must be stripped")
	}
	if !strings.HasPrefix(anyString(persisted["secret_fingerprint"]), "sha256:") {
		t.Fatalf("fingerprint expected, got %v", persisted["secret_fingerprint"])
	}
}

func TestLoadPoolCollapsesDuplicateEnvRowsToActiveKey(t *testing.T) {
	h := newTestHome(t)
	key := "sk-or-active-main-key"
	t.Setenv("OPENROUTER_API_KEY", key)
	writeAuthStore(t, h, authWithPool("openrouter",
		row(map[string]any{"id": "current-row", "label": "OPENROUTER_API_KEY", "auth_type": "api_key", "priority": 0, "source": "env:OPENROUTER_API_KEY"}),
		row(map[string]any{"id": "stale-duplicate", "label": "OPENROUTER_API_KEY", "auth_type": "api_key", "priority": 1, "source": "env:OPENROUTER_API_KEY"})))
	pool := LoadPool(testEnv(t, h), "openrouter")
	entries := pool.Entries()
	if len(entries) != 1 || entries[0].ID != "current-row" || entries[0].RuntimeAPIKey() != key {
		t.Fatalf("duplicates must collapse to the first row: %+v", entries)
	}
	rows := poolRows(t, h, "openrouter")
	if len(rows) != 1 || rows[0]["id"] != "current-row" {
		t.Fatalf("disk must hold one row: %v", rows)
	}
}

func TestCredentialPoolNeverSelectsEmptyBorrowedEntry(t *testing.T) {
	h := newTestHome(t)
	pool := NewPool(testEnv(t, h), "openrouter", []PooledCredential{{Provider: "openrouter", ID: "metadata-only", Label: "OPENROUTER_API_KEY", AuthType: "api_key", Priority: 0, Source: "env:OPENROUTER_API_KEY", AccessToken: "", Extra: map[string]any{}}})
	if _, ok := pool.Select(""); ok {
		t.Fatal("an unhydrated borrowed entry must never be selected")
	}
	if _, ok := pool.AcquireLease(""); ok {
		t.Fatal("an unhydrated borrowed entry must never be leased")
	}
}

func TestLoadPoolPersistsBitwardenOriginMetadataWithoutSecret(t *testing.T) {
	h := newTestHome(t)
	sentinel := "S3NTINEL_DO_NOT_PERSIST_BITWARDEN"
	t.Setenv("OPENROUTER_API_KEY", sentinel)
	secretSourceCacheMu.Lock()
	secretSourceOfVar["OPENROUTER_API_KEY"] = "bitwarden"
	secretSourceCacheMu.Unlock()
	t.Cleanup(func() {
		secretSourceCacheMu.Lock()
		delete(secretSourceOfVar, "OPENROUTER_API_KEY")
		secretSourceCacheMu.Unlock()
	})
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), "")
	if entry.AccessToken != sentinel || entry.Source != "env:OPENROUTER_API_KEY" {
		t.Fatalf("unexpected entry %+v", entry)
	}
	raw, _ := json.Marshal(readAuthStore(t, h))
	if strings.Contains(string(raw), sentinel) {
		t.Fatal("secret leaked to disk")
	}
	persisted := poolRows(t, h, "openrouter")[0]
	if persisted["secret_source"] != "bitwarden" {
		t.Fatalf("secret_source expected: %v", persisted)
	}
	if _, has := persisted["access_token"]; has {
		t.Fatal("access_token must be stripped")
	}
}

func TestLoadPoolSanitizesLegacyRawBorrowedEntryWhenValueUnchanged(t *testing.T) {
	h := newTestHome(t)
	sentinel := "S3NTINEL_DO_NOT_PERSIST_LEGACY_RAW"
	t.Setenv("OPENROUTER_API_KEY", sentinel)
	writeAuthStore(t, h, authWithPool("openrouter", row(map[string]any{"id": "legacy-env", "label": "OPENROUTER_API_KEY", "auth_type": "api_key", "priority": 0,
		"source": "env:OPENROUTER_API_KEY", "access_token": sentinel, "base_url": "https://openrouter.ai/api/v1"})))
	entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), "")
	if entry.AccessToken != sentinel {
		t.Fatal("runtime token expected")
	}
	raw, _ := json.Marshal(readAuthStore(t, h))
	if strings.Contains(string(raw), sentinel) {
		t.Fatal("legacy raw row must be rewritten without its secret")
	}
	persisted := poolRows(t, h, "openrouter")[0]
	if persisted["id"] != "legacy-env" {
		t.Fatal("row id must survive")
	}
	if _, has := persisted["access_token"]; has || !strings.HasPrefix(anyString(persisted["secret_fingerprint"]), "sha256:") {
		t.Fatalf("expected fingerprint only: %v", persisted)
	}
}

func TestPooledCredentialToMapStripsBorrowedSecretFields(t *testing.T) {
	sentinel := "S3NTINEL_DO_NOT_PERSIST_TO_DICT"
	cred := PooledCredential{Provider: "openrouter", ID: "borrowed-1", Label: "vault-ref", AuthType: "api_key", Priority: 3, Source: "vault:openrouter/api-key",
		AccessToken: sentinel, RefreshToken: "refresh-" + sentinel, AgentKey: "agent-" + sentinel, RequestCount: 7, LastStatus: "ok",
		Extra: map[string]any{"api_key": "extra-" + sentinel, "client_secret": "client-" + sentinel, "secret_key": "secret-key-" + sentinel,
			"authToken": "auth-token-" + sentinel, "refreshToken": "camel-refresh-" + sentinel, "authorization": "Bearer " + sentinel,
			"tokens": map[string]any{"access_token": "nested-" + sentinel}, "token_type": "Bearer", "scope": "inference"}}
	payload := cred.ToMap()
	raw, _ := json.Marshal(payload)
	if strings.Contains(string(raw), sentinel) {
		t.Fatal("secret leaked through to_dict")
	}
	for _, key := range []string{"access_token", "refresh_token", "agent_key", "api_key", "client_secret", "secret_key", "authToken", "refreshToken", "authorization", "tokens"} {
		if _, has := payload[key]; has {
			t.Fatalf("%s must be stripped", key)
		}
	}
	if payload["source"] != "vault:openrouter/api-key" || payload["label"] != "vault-ref" || payload["request_count"] != 7 || payload["token_type"] != "Bearer" || payload["scope"] != "inference" {
		t.Fatalf("metadata must survive: %v", payload)
	}
	if !strings.HasPrefix(anyString(payload["secret_fingerprint"]), "sha256:") {
		t.Fatal("fingerprint expected")
	}
}

func TestBorrowedSourceVariantsStripSecretFields(t *testing.T) {
	for _, source := range []string{"age://openrouter/api-key", "systemd", "keyring", "1password", "pass", "sops", "future_secret_store:openrouter"} {
		sentinel := "S3NTINEL_DO_NOT_PERSIST_" + strings.NewReplacer(":", "_", "/", "_").Replace(source)
		payload := PooledCredential{Provider: "openrouter", ID: "borrowed-variant", Label: "borrowed", AuthType: "api_key", Source: source,
			AccessToken: sentinel, RefreshToken: "refresh-" + sentinel, Extra: map[string]any{}}.ToMap()
		raw, _ := json.Marshal(payload)
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("%s leaked its secret", source)
		}
		if _, has := payload["access_token"]; has {
			t.Fatalf("%s kept access_token", source)
		}
		if _, has := payload["refresh_token"]; has {
			t.Fatalf("%s kept refresh_token", source)
		}
		if payload["source"] != source || !strings.HasPrefix(anyString(payload["secret_fingerprint"]), "sha256:") {
			t.Fatalf("%s payload wrong: %v", source, payload)
		}
	}
}

func TestWriteCredentialPoolSanitizesBorrowedPayloadAtDiskBoundary(t *testing.T) {
	h := newTestHome(t)
	sentinel := "S3NTINEL_DO_NOT_PERSIST_DIRECT_WRITE"
	manual := "MANUAL_SECRET_STAYS_PERSISTABLE"
	store := NewStore(h)
	if _, err := store.WriteCredentialPool("openrouter", []map[string]any{
		{"id": "borrowed-1", "label": "systemd-ref", "auth_type": "api_key", "priority": 0, "source": "systemd://icebath/openrouter",
			"access_token": sentinel, "refresh_token": "refresh-" + sentinel, "agent_key": "agent-" + sentinel, "api_key": "extra-" + sentinel},
		{"id": "manual-1", "label": "manual", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": manual},
	}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(readAuthStore(t, h))
	if strings.Contains(string(raw), sentinel) || !strings.Contains(string(raw), manual) {
		t.Fatal("borrowed secret must be stripped and the manual one kept")
	}
	rows := poolRows(t, h, "openrouter")
	borrowed, manualRow := rows[0], rows[1]
	for _, key := range []string{"access_token", "refresh_token", "agent_key", "api_key"} {
		if _, has := borrowed[key]; has {
			t.Fatalf("%s must be stripped", key)
		}
	}
	if !strings.HasPrefix(anyString(borrowed["secret_fingerprint"]), "sha256:") || manualRow["access_token"] != manual {
		t.Fatalf("unexpected rows %v", rows)
	}
}

func TestWriteCredentialPoolTreatsUnownedOAuthSourceAsBorrowed(t *testing.T) {
	h := newTestHome(t)
	sentinel := "S3NTINEL_DO_NOT_PERSIST_UNOWNED_OAUTH"
	if _, err := NewStore(h).WriteCredentialPool("openrouter", []map[string]any{{"id": "unowned-oauth", "label": "unowned-oauth", "auth_type": "oauth", "priority": 0,
		"source": "oauth", "access_token": sentinel, "refresh_token": "refresh-" + sentinel}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(readAuthStore(t, h))
	if strings.Contains(string(raw), sentinel) {
		t.Fatal("unowned oauth secret leaked")
	}
	persisted := poolRows(t, h, "openrouter")[0]
	if persisted["source"] != "oauth" || !strings.HasPrefix(anyString(persisted["secret_fingerprint"]), "sha256:") {
		t.Fatalf("unexpected row %v", persisted)
	}
}

func TestWriteCredentialPoolPreservesKnownProviderOwnedOAuthState(t *testing.T) {
	h := newTestHome(t)
	sentinel := "PROVIDER_OWNED_DEVICE_CODE_STAYS_PERSISTABLE"
	if _, err := NewStore(h).WriteCredentialPool("nous", []map[string]any{{"id": "nous-device", "label": "device-code", "auth_type": "oauth", "priority": 0,
		"source": "device_code", "access_token": sentinel, "refresh_token": "refresh-" + sentinel, "agent_key": "agent-" + sentinel}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	persisted := poolRows(t, h, "nous")[0]
	if persisted["access_token"] != sentinel || persisted["refresh_token"] != "refresh-"+sentinel || persisted["agent_key"] != "agent-"+sentinel {
		t.Fatalf("provider owned state must persist: %v", persisted)
	}
}

func TestLoadPoolPrefersDotenvOverStaleOSEnviron(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-STALE-from-shell")
	writeEnvFile(t, h, "OPENROUTER_API_KEY=sk-or-FRESH-from-dotenv\n")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), "")
	if entry.Source != "env:OPENROUTER_API_KEY" || entry.AccessToken != "sk-or-FRESH-from-dotenv" {
		t.Fatalf("the .env key must win, got %q", entry.AccessToken)
	}
}

func TestLoadPoolFallsBackToOSEnvironWhenDotenvEmpty(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("OPENROUTER_API_KEY", "sk-or-from-runtime-env")
	writeEnvFile(t, h, "SOME_OTHER_VAR=unrelated\n")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	if entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), ""); entry.AccessToken != "sk-or-from-runtime-env" {
		t.Fatalf("runtime env key expected, got %q", entry.AccessToken)
	}
}

func TestLoadPoolMirrorsNousInvokeJWTAgentKeyRuntimeAPIKey(t *testing.T) {
	h := newTestHome(t)
	expiresAt := isoIn(3600)
	token := jwtWithClaims(map[string]any{"sub": "test-user", "scope": []any{"inference:invoke"}, "exp": int(nowEpoch() + 3600)})
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "nous", "providers": map[string]any{"nous": map[string]any{
		"portal_base_url": "https://portal.example.com", "inference_base_url": "https://inference.example.com/v1", "client_id": "icebath-cli",
		"token_type": "Bearer", "scope": "inference:invoke", "access_token": token, "refresh_token": "refresh-token", "expires_at": expiresAt,
		"agent_key": token, "agent_key_expires_at": expiresAt}}})
	entry := mustSelect(t, LoadPool(testEnv(t, h), "nous"), "")
	if entry.Source != "device_code" || entry.AgentKey != token || entry.RuntimeAPIKey() != token {
		t.Fatalf("unexpected nous entry %+v", entry)
	}
	persisted := poolRows(t, h, "nous")[0]
	if persisted["agent_key"] != token || persisted["agent_key_expires_at"] != expiresAt {
		t.Fatalf("agent key must persist: %v", persisted)
	}
}

func TestNousRuntimeAPIKeyRejectsOpaqueAgentKey(t *testing.T) {
	entry := PooledCredential{Provider: "nous", ID: "nous-opaque", Label: "opaque", AuthType: "oauth", Source: "device_code", AccessToken: "opaque-access-token",
		RefreshToken: "refresh-token", AgentKey: "opaque-agent-key", AgentKeyExpiresAt: isoIn(3600), Extra: map[string]any{"scope": "inference:invoke"}}
	if entry.RuntimeAPIKey() != "" {
		t.Fatal("an opaque agent key is not a NAS invoke JWT")
	}
}

func TestLoadPoolAPIKeyPathSkipsOAuthAutodiscovery(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-explicit-user-key")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	writeClaudeCodeCredentials(t, "sk-ant-oat01-claude-code-token", "cc-refresh", time.Now().UnixMilli()+3_600_000)
	env := testEnv(t, h)
	if err := WriteHermesOAuthCredentials(env, "sk-ant-oat01-pkce-token", "pkce-refresh", time.Now().UnixMilli()+3_600_000, ""); err != nil {
		t.Fatal(err)
	}
	pool := LoadPool(env, "anthropic")
	sources := map[string]bool{}
	for _, e := range pool.Entries() {
		sources[e.Source] = true
	}
	if len(sources) != 1 || !sources["env:ANTHROPIC_API_KEY"] {
		t.Fatalf("only the explicit API key may be pooled: %v", sources)
	}
}

func TestLoadPoolAPIKeyPathPrunesStaleOAuthEntries(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-explicit-user-key")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}, "credential_pool": map[string]any{"anthropic": []any{
		row(map[string]any{"id": "stale1", "source": "claude_code", "auth_type": "oauth", "access_token": "sk-ant-oat01-stale-claude-code", "refresh_token": "stale-refresh",
			"expires_at_ms": time.Now().UnixMilli() + 3_600_000, "priority": 0, "label": "stale-claude-code", "request_count": 0})}}})
	pool := LoadPool(testEnv(t, h), "anthropic")
	sources := map[string]bool{}
	for _, e := range pool.Entries() {
		sources[e.Source] = true
	}
	if sources["claude_code"] || !sources["env:ANTHROPIC_API_KEY"] {
		t.Fatalf("stale claude_code must be pruned and the key kept: %v", sources)
	}
}

func TestLoadPoolOAuthPathStillAutodiscovers(t *testing.T) {
	h := newTestHome(t)
	t.Setenv("ANTHROPIC_TOKEN", "sk-ant-oat01-explicit-oauth-token")
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{}})
	writeClaudeCodeCredentials(t, "sk-ant-oat01-autodiscovered-cc", "cc-refresh", time.Now().UnixMilli()+3_600_000)
	pool := LoadPool(testEnv(t, h), "anthropic")
	sources := map[string]bool{}
	for _, e := range pool.Entries() {
		sources[e.Source] = true
	}
	if !sources["env:ANTHROPIC_TOKEN"] || !sources["claude_code"] {
		t.Fatalf("both the env OAuth token and the autodiscovered login must be pooled: %v", sources)
	}
}

func TestLeastUsedStrategySelectsLowestCount(t *testing.T) {
	h := newTestHome(t)
	writeConfigYAML(t, h, "credential_pool_strategies:\n  openrouter: least_used\n")
	writeAuthStore(t, h, authWithPool("openrouter",
		row(map[string]any{"id": "key-a", "label": "heavy", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-or-heavy", "request_count": 100}),
		row(map[string]any{"id": "key-b", "label": "light", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "sk-or-light", "request_count": 10}),
		row(map[string]any{"id": "key-c", "label": "medium", "auth_type": "api_key", "priority": 2, "source": "manual", "access_token": "sk-or-medium", "request_count": 50})))
	entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), "")
	if entry.ID != "key-b" || entry.AccessToken != "sk-or-light" {
		t.Fatalf("least used expected, got %+v", entry)
	}
}

func TestCustomEndpointPoolSeedsFromConfig(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1})
	writeConfigYAML(t, h, "custom_providers:\n  - name: Together.ai\n    base_url: https://api.together.ai/v1\n    api_key: sk-config-seeded\n")
	pool := LoadPool(testEnv(t, h), "custom:together.ai")
	entries := pool.Entries()
	if !pool.HasCredentials() || len(entries) != 1 || entries[0].AccessToken != "sk-config-seeded" || entries[0].Source != "config:Together.ai" {
		t.Fatalf("config seeding failed: %+v", entries)
	}
}

func TestCustomEndpointPoolSeedsFromModelConfig(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1})
	writeConfigYAML(t, h, "custom_providers:\n  - name: Together.ai\n    base_url: https://api.together.ai/v1\nmodel:\n  provider: custom\n  base_url: https://api.together.ai/v1\n  api_key: sk-model-key\n")
	pool := LoadPool(testEnv(t, h), "custom:together.ai")
	var modelEntries []PooledCredential
	for _, e := range pool.Entries() {
		if e.Source == "model_config" {
			modelEntries = append(modelEntries, e)
		}
	}
	if !pool.HasCredentials() || len(modelEntries) != 1 || modelEntries[0].AccessToken != "sk-model-key" {
		t.Fatalf("model config seeding failed: %+v", pool.Entries())
	}
}

func TestLoadPoolDoesNotSeedClaudeCodeWhenAnthropicNotConfigured(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{}})
	writeClaudeCodeCredentials(t, "sk-ant...oken", "rt", 9999999999999)
	if entries := LoadPool(testEnv(t, h), "anthropic").Entries(); len(entries) != 0 {
		t.Fatalf("claude_code must not be seeded without explicit anthropic config: %+v", entries)
	}
}

func TestLoadPoolSeedsQwenOAuthViaCLITokens(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{}})
	prev := qwenCredentialsForSeed
	qwenCredentialsForSeed = func(env PoolEnv, refresh bool) (map[string]any, error) {
		return map[string]any{"provider": "qwen-oauth", "base_url": "https://portal.qwen.ai/v1", "api_key": "qwen_fake_token_xyz", "source": "qwen-cli",
			"expires_at_ms": float64(1900000000000), "auth_file": "/tmp/.qwen/oauth_creds.json"}, nil
	}
	t.Cleanup(func() { qwenCredentialsForSeed = prev })
	pool := LoadPool(testEnv(t, h), "qwen-oauth")
	entries := pool.Entries()
	if !pool.HasCredentials() || len(entries) != 1 || entries[0].Source != "qwen-cli" || entries[0].AccessToken != "qwen_fake_token_xyz" {
		t.Fatalf("qwen seeding failed: %+v", entries)
	}
}

func TestLoadPoolDoesNotSeedQwenOAuthWhenNoToken(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{}})
	prev := qwenCredentialsForSeed
	qwenCredentialsForSeed = func(env PoolEnv, refresh bool) (map[string]any, error) {
		return nil, &AuthError{Message: "Qwen CLI credentials not found.", Provider: "qwen-oauth", Code: "qwen_auth_missing"}
	}
	t.Cleanup(func() { qwenCredentialsForSeed = prev })
	pool := LoadPool(testEnv(t, h), "qwen-oauth")
	if pool.HasCredentials() || len(pool.Entries()) != 0 {
		t.Fatal("no qwen credentials expected")
	}
}

func TestNousSeedFromSingletonsPreservesObtainedAtTimestamps(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "providers": map[string]any{"nous": map[string]any{
		"access_token": "at_XXXXXXXX", "refresh_token": "rt_YYYYYYYY", "client_id": "icebath-cli", "portal_base_url": "https://portal.nousresearch.com",
		"inference_base_url": "https://inference.nousresearch.com/v1", "token_type": "Bearer", "scope": "openid profile",
		"obtained_at": "2026-04-24T10:00:00+00:00", "expires_at": "2026-04-24T11:00:00+00:00", "expires_in": 3600,
		"agent_key": "sk-nous-AAAA", "agent_key_id": "ak_123", "agent_key_expires_at": "2026-04-25T10:00:00+00:00", "agent_key_expires_in": 86400,
		"agent_key_reused": false, "agent_key_obtained_at": "2026-04-24T10:00:05+00:00", "tls": map[string]any{"insecure": false, "ca_bundle": nil}}}})
	var device []PooledCredential
	for _, e := range LoadPool(testEnv(t, h), "nous").Entries() {
		if e.Source == "device_code" {
			device = append(device, e)
		}
	}
	if len(device) != 1 {
		t.Fatalf("expected one device_code entry, got %d", len(device))
	}
	e := device[0]
	if e.AccessToken != "at_XXXXXXXX" || e.RefreshToken != "rt_YYYYYYYY" || e.ExpiresAt != "2026-04-24T11:00:00+00:00" || e.AgentKey != "sk-nous-AAAA" || e.AgentKeyExpiresAt != "2026-04-25T10:00:00+00:00" {
		t.Fatalf("fields dropped: %+v", e)
	}
	if e.ExtraString("obtained_at") != "2026-04-24T10:00:00+00:00" || e.ExtraString("agent_key_obtained_at") != "2026-04-24T10:00:05+00:00" {
		t.Fatalf("obtained_at timestamps dropped: %v", e.Extra)
	}
	if n, _ := anyInt(e.Extra["expires_in"]); n != 3600 {
		t.Fatalf("expires_in dropped: %v", e.Extra["expires_in"])
	}
	if e.ExtraString("agent_key_id") != "ak_123" {
		t.Fatal("agent_key_id dropped")
	}
	if n, _ := anyInt(e.Extra["agent_key_expires_in"]); n != 86400 {
		t.Fatal("agent_key_expires_in dropped")
	}
	if reused, ok := e.Extra["agent_key_reused"].(bool); !ok || reused {
		t.Fatalf("agent_key_reused must be the persisted false, got %v", e.Extra["agent_key_reused"])
	}
}

func TestPersistPreservesConcurrentDiskOnlyEntry(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("anthropic",
		row(map[string]any{"id": "cred-A", "label": "primary", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "sk-A"}),
		row(map[string]any{"id": "cred-B", "label": "secondary", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "sk-B"})))
	env := testEnv(t, h)
	pool := LoadPool(env, "anthropic")
	ids := map[string]bool{}
	for _, e := range pool.Entries() {
		ids[e.ID] = true
	}
	if !ids["cred-A"] || !ids["cred-B"] || len(ids) != 2 {
		t.Fatalf("unexpected ids %v", ids)
	}
	snapshot, _ := env.Store.ReadCredentialPool("anthropic")
	snapshot = append(snapshot, map[string]any{"id": "cred-C", "label": "added-concurrently", "auth_type": "api_key", "priority": 2, "source": "manual", "access_token": "sk-C"})
	if _, err := env.Store.WriteCredentialPool("anthropic", snapshot, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 429})
	rows := poolRows(t, h, "anthropic")
	final := map[string]bool{}
	for _, r := range rows {
		final[anyString(r["id"])] = true
	}
	if !final["cred-A"] || !final["cred-B"] || !final["cred-C"] {
		t.Fatalf("the concurrent entry must survive: %v", final)
	}
	if rowByID(rows, "cred-A")["last_status"] != "exhausted" {
		t.Fatal("cred-A must be exhausted on disk")
	}
}

func makeAnthropicClaudeCodePool(t *testing.T, h, access, refresh string, expiresAtMS int64) (*Pool, PooledCredential) {
	t.Helper()
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "anthropic", "credential_pool": map[string]any{}})
	writeClaudeCodeCredentials(t, access, refresh, expiresAtMS)
	pool := LoadPool(testEnv(t, h), "anthropic")
	entry := mustSelect(t, pool, "")
	if entry.Source != "claude_code" {
		t.Fatalf("expected the claude_code row, got %+v", entry)
	}
	return pool, entry
}

func TestSyncAnthropicEntryClearsAllErrorFields(t *testing.T) {
	h := newTestHome(t)
	pool, entry := makeAnthropicClaudeCodePool(t, h, "stale-access", "stale-refresh", 9_999_999_999_000)
	now := nowEpoch()
	exhausted := entry.Clone()
	exhausted.LastStatus, exhausted.LastStatusAt, exhausted.LastErrorCode = StatusExhausted, now, 401
	exhausted.LastErrorReason, exhausted.LastErrorMessage, exhausted.LastErrorResetAt = "token_expired", "Access token has expired", now+300
	pool.replaceEntry(exhausted, false)
	writeClaudeCodeCredentials(t, "fresh-access", "fresh-refresh", 9_999_999_999_000)
	synced := pool.syncAnthropicEntryFromCredentialsFile(exhausted, false)
	if synced.AccessToken != "fresh-access" {
		t.Fatalf("fresh token expected, got %+v", synced)
	}
	if synced.LastStatus != "" || synced.LastStatusAt != 0 || synced.LastErrorCode != 0 || synced.LastErrorReason != "" || synced.LastErrorMessage != "" || synced.LastErrorResetAt != 0 {
		t.Fatalf("every error field must be cleared: %+v", synced)
	}
}

func loadTwoOKPool(t *testing.T, h string) *Pool {
	t.Helper()
	writeAuthStore(t, h, authWithPool("anthropic",
		row(map[string]any{"id": "cred-1", "label": "primary", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "***", "last_status": "ok", "last_status_at": nil, "last_error_code": nil}),
		row(map[string]any{"id": "cred-2", "label": "secondary", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "***", "last_status": "ok", "last_status_at": nil, "last_error_code": nil})))
	return LoadPool(testEnv(t, h), "anthropic")
}

func TestQueryMethodsDoNotDeadlock(t *testing.T) {
	h := newTestHome(t)
	pool := loadTwoOKPool(t, h)
	pool.Select("")
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok := pool.Current(); !ok {
			t.Error("current expected")
		}
		if _, ok := pool.Peek(); !ok {
			t.Error("peek expected")
		}
		if !pool.HasAvailable("") || !pool.HasCredentials() {
			t.Error("availability expected")
		}
		if _, _, errText := pool.ResolveTarget("cred-1"); errText != "" {
			t.Error(errText)
		}
		ids := map[string]bool{}
		for _, e := range pool.Entries() {
			ids[e.ID] = true
		}
		if !ids["cred-1"] || !ids["cred-2"] {
			t.Error("entries missing")
		}
		pool.TryRefreshMatching("", "")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pool query methods deadlocked")
	}
}

func exhaustedBillingStore(t *testing.T, h string, age float64) {
	t.Helper()
	writeAuthStore(t, h, authWithPool("deepseek", row(map[string]any{"id": "cred-1", "label": "api-key-1", "auth_type": "api_key", "priority": 0, "source": "manual",
		"access_token": "sk-test", "last_status": "exhausted", "last_status_at": nowEpoch() - age, "last_error_code": 402,
		"last_error_reason": "invalid_request_error", "last_error_message": "Insufficient Balance", "failure_reason": "billing"})))
}

func diskEntry(t *testing.T, h string) map[string]any {
	t.Helper()
	rows := poolRows(t, h, "deepseek")
	if len(rows) != 1 {
		t.Fatalf("expected one row, got %v", rows)
	}
	return rows[0]
}

func TestResetStatusesClearsACooldownThatIsStillBinding(t *testing.T) {
	h := newTestHome(t)
	exhaustedBillingStore(t, h, 5)
	env := testEnv(t, h)
	if LoadPool(env, "deepseek").ResetStatuses() != 1 {
		t.Fatal("one reset expected")
	}
	entry := diskEntry(t, h)
	if entry["last_status"] != nil || entry["last_status_at"] != nil || entry["last_error_code"] != nil {
		t.Fatalf("reset must reach disk: %v", entry)
	}
	if !LoadPool(env, "deepseek").HasAvailable("") {
		t.Fatal("a fresh load must see the credential as available")
	}
}

func TestResetStatusesClearsTheClassifiedFailureReason(t *testing.T) {
	h := newTestHome(t)
	exhaustedBillingStore(t, h, 5)
	if LoadPool(testEnv(t, h), "deepseek").ResetStatuses() != 1 {
		t.Fatal("one reset expected")
	}
	if diskEntry(t, h)["failure_reason"] != nil {
		t.Fatal("failure_reason must be cleared")
	}
}

func TestAPersistWithoutDeclaredIntentStillCannotEraseACooldown(t *testing.T) {
	h := newTestHome(t)
	exhaustedBillingStore(t, h, 5)
	if _, err := NewStore(h).WriteCredentialPool("deepseek", []map[string]any{{"id": "cred-1", "label": "api-key-1", "auth_type": "api_key", "priority": 0, "source": "manual",
		"access_token": "sk-test", "last_status": nil, "last_status_at": nil, "last_error_code": nil}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	entry := diskEntry(t, h)
	if entry["last_status"] != "exhausted" || entry["last_error_code"] != float64(402) {
		t.Fatalf("the binding cooldown must survive an undeclared write: %v", entry)
	}
}

func TestLivePoolFlushDoesNotResurrectACooldownResetByAnotherProcess(t *testing.T) {
	h := newTestHome(t)
	exhaustedBillingStore(t, h, 5)
	env := testEnv(t, h)
	live := LoadPool(env, "deepseek")
	if live.HasAvailable("") {
		t.Fatal("benched at first")
	}
	if LoadPool(env, "deepseek").ResetStatuses() != 1 {
		t.Fatal("reset expected")
	}
	live.persist(false, nil, nil)
	if diskEntry(t, h)["last_status"] != nil {
		t.Fatal("the flush must not resurrect the cooldown")
	}
	if _, ok := live.Select(""); !ok {
		t.Fatal("the live pool must honour the reset")
	}
	if diskEntry(t, h)["last_status"] == "exhausted" {
		t.Fatal("disk must stay cleared")
	}
}

func TestAnExhaustionNewerThanTheResetStillBinds(t *testing.T) {
	h := newTestHome(t)
	exhaustedBillingStore(t, h, 5)
	env := testEnv(t, h)
	if LoadPool(env, "deepseek").ResetStatuses() != 1 {
		t.Fatal("reset expected")
	}
	live := LoadPool(env, "deepseek")
	if _, ok := live.Select(""); !ok {
		t.Fatal("selectable after reset")
	}
	live.MarkExhaustedAndRotate(RotationRequest{StatusCode: 402, APIKeyHint: "sk-test", ErrorContext: map[string]any{"message": "Insufficient Balance"}})
	live.persist(false, nil, nil)
	if diskEntry(t, h)["last_status"] != "exhausted" {
		t.Fatal("the newer bench must survive")
	}
	if _, ok := live.Select(""); ok {
		t.Fatal("the newer bench must keep binding")
	}
}

// ---- test_credential_pool_sole_cooldown.py ----

func soleEntry(code int, age float64, id string, priority int, failureReason string) map[string]any {
	e := row(map[string]any{"id": id, "label": id, "auth_type": "api_key", "priority": priority, "source": "manual", "access_token": "***",
		"base_url": "https://openrouter.ai/api/v1", "last_status": "exhausted", "last_status_at": nowEpoch() - age, "last_error_code": code})
	if failureReason != "" {
		e["failure_reason"] = failureReason
	}
	return e
}

func loadOpenRouter(t *testing.T, rows ...map[string]any) *Pool {
	t.Helper()
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openrouter", rows...))
	return LoadPool(testEnv(t, h), "openrouter")
}

func TestSoleCredential429RecoversAfterShortCooldown(t *testing.T) {
	entry := mustSelect(t, loadOpenRouter(t, soleEntry(429, 90, "cred-1", 0, "")), "")
	if entry.ID != "cred-1" || entry.LastStatus != StatusOK {
		t.Fatalf("sole key must recover: %+v", entry)
	}
}

func TestSoleCredential403RecoversAfterShortCooldown(t *testing.T) {
	if entry := mustSelect(t, loadOpenRouter(t, soleEntry(403, 90, "cred-1", 0, "")), ""); entry.LastStatus != StatusOK {
		t.Fatalf("403 must recover: %+v", entry)
	}
}

func TestSoleCredentialBilling403KeepsFullBench(t *testing.T) {
	pool := loadOpenRouter(t, soleEntry(403, 90, "cred-1", 0, "billing"))
	if pool.HasAvailable("") {
		t.Fatal("billing bench must hold")
	}
	if _, ok := pool.Select(""); ok {
		t.Fatal("billing bench must hold on select")
	}
}

func TestSoleCredential402KeepsFullBench(t *testing.T) {
	pool := loadOpenRouter(t, soleEntry(402, 90, "cred-1", 0, ""))
	if pool.HasAvailable("") {
		t.Fatal("402 bench must hold")
	}
}

func TestSoleCredentialNextAvailableAtUsesShortCooldown(t *testing.T) {
	pool := loadOpenRouter(t, soleEntry(429, 10, "cred-1", 0, ""))
	next, ok := pool.NextAvailableAt("")
	if !ok {
		t.Fatal("next available expected")
	}
	remaining := time.Until(next)
	if remaining >= ExhaustedTTLSoleCredential || remaining >= 300*time.Second {
		t.Fatalf("remaining %v must be the sole credential cooldown", remaining)
	}
}

func TestMultiKey429KeepsFullBench(t *testing.T) {
	pool := loadOpenRouter(t, soleEntry(429, 90, "cred-1", 0, ""), soleEntry(429, 90, "cred-2", 1, ""))
	if pool.HasAvailable("") {
		t.Fatal("both throttled keys stay benched")
	}
}

func TestSoleCredentialUnverifiedBilling400RecoversQuickly(t *testing.T) {
	if entry := mustSelect(t, loadOpenRouter(t, soleEntry(400, 90, "cred-1", 0, "billing_unverified")), ""); entry.LastStatus != StatusOK {
		t.Fatalf("unverified billing must recover: %+v", entry)
	}
}

func TestMultiKeyUnverifiedBilling400RecoversQuickly(t *testing.T) {
	if entry := mustSelect(t, loadOpenRouter(t, soleEntry(400, 90, "cred-1", 0, "billing_unverified"), soleEntry(400, 90, "cred-2", 1, "billing_unverified")), ""); entry.LastStatus != StatusOK {
		t.Fatalf("unverified billing must recover regardless of pool size: %+v", entry)
	}
}

func TestUnverifiedBillingTTLValues(t *testing.T) {
	if exhaustedTTL(400, true, "billing_unverified") != ExhaustedTTLSoleCredential || exhaustedTTL(400, false, "billing_unverified") != ExhaustedTTLSoleCredential {
		t.Fatal("unverified billing is transient sized")
	}
	if exhaustedTTL(400, true, "billing") != ExhaustedTTLDefault || exhaustedTTL(402, true, "billing_unverified") != ExhaustedTTLDefault {
		t.Fatal("confirmed billing keeps the full bench")
	}
}

// ---- test_credential_pool_unmatched_rotation_bound.py ----

func TestMultiEntryPoolUnmatchedHintLoopTerminates(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openrouter",
		row(map[string]any{"id": "cred-0", "label": "key-0", "auth_type": "api_key", "priority": 0, "source": "manual", "access_token": "key-a"}),
		row(map[string]any{"id": "cred-1", "label": "key-1", "auth_type": "api_key", "priority": 1, "source": "manual", "access_token": "key-b"}),
		row(map[string]any{"id": "cred-2", "label": "key-2", "auth_type": "api_key", "priority": 2, "source": "manual", "access_token": "key-c"})))
	pool := LoadPool(testEnv(t, h), "openrouter")
	mustSelect(t, pool, "")
	rotations := 0
	terminated := false
	for i := 0; i < 10; i++ {
		rotations++
		if _, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 401, ErrorContext: map[string]any{"reason": "unauthorized"}, APIKeyHint: "oauth-runtime-token-that-matches-nothing"}); !ok {
			terminated = true
			break
		}
	}
	if !terminated || rotations > 4 {
		t.Fatalf("unmatched hint rotation must terminate within one lap, rotations=%d terminated=%t", rotations, terminated)
	}
	for id, status := range statusesOf(pool) {
		if status == StatusExhausted {
			t.Fatalf("innocent key %s was quarantined", id)
		}
	}
}

// ---- test_credential_pool_key_rotation.py ----

func TestKeyRotationClearsExhaustedStatus(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openrouter", row(map[string]any{"id": "cred-1", "label": "OPENROUTER_API_KEY", "auth_type": "api_key", "priority": 0,
		"source": "env:OPENROUTER_API_KEY", "access_token": "old-key", "last_status": "exhausted", "last_status_at": 1000.0, "last_error_code": 429,
		"last_error_reason": "rate_limit", "last_error_message": "Too many requests", "last_error_reset_at": 2000.0})))
	t.Setenv("OPENROUTER_API_KEY", "new-rotated-key")
	entry := mustSelect(t, LoadPool(testEnv(t, h), "openrouter"), "")
	if entry.AccessToken != "new-rotated-key" || entry.LastStatus != "" || entry.LastStatusAt != 0 || entry.LastErrorCode != 0 || entry.LastErrorReason != "" || entry.LastErrorMessage != "" || entry.LastErrorResetAt != 0 {
		t.Fatalf("rotation must clear the exhaustion: %+v", entry)
	}
}

func TestSameKeyPreservesExhaustedStatus(t *testing.T) {
	existing := PooledCredentialFromMap("openrouter", map[string]any{"id": "cred-1", "label": "OPENROUTER_API_KEY", "auth_type": "api_key", "priority": 0,
		"source": "env:OPENROUTER_API_KEY", "access_token": "same-key", "last_status": "exhausted", "last_status_at": 1000.0, "last_error_code": 429,
		"last_error_reason": "rate_limit", "last_error_message": "Too many requests", "last_error_reset_at": 2000.0})
	entries := []PooledCredential{existing}
	upsertEntry(&entries, "openrouter", "env:OPENROUTER_API_KEY", map[string]any{"source": "env:OPENROUTER_API_KEY", "auth_type": "api_key", "access_token": "same-key"}, nil)
	if entries[0].LastStatus != "exhausted" {
		t.Fatal("same key must keep its exhaustion state")
	}
}

// ---- test_credential_pool_operations.py ----

func adminPool(t *testing.T, h, provider string, exhausted bool) *Pool {
	t.Helper()
	env := testEnv(t, h)
	var rows []PooledCredential
	for i := 0; i < 2; i++ {
		e := PooledCredential{Provider: provider, ID: "row" + string(rune('0'+i)), Label: "account" + string(rune('0'+i)), Source: "manual", AuthType: "api_key",
			AccessToken: "fixture-" + string(rune('0'+i)), Priority: i, Extra: map[string]any{}}
		if exhausted {
			e.LastStatus, e.LastStatusAt, e.LastErrorCode, e.LastErrorResetAt = "exhausted", nowEpoch(), 429, nowEpoch()+3600
		}
		rows = append(rows, e)
	}
	var payloads []map[string]any
	for _, e := range rows {
		payloads = append(payloads, e.ToMap())
	}
	if _, err := env.Store.WriteCredentialPool(provider, payloads, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	return NewPool(env, provider, rows)
}

func TestTargetResetPreservesSiblingCooldown(t *testing.T) {
	h := newTestHome(t)
	pool := adminPool(t, h, "openrouter", true)
	before := poolRows(t, h, "openrouter")
	if _, ok := pool.ResetStatus("missing"); ok {
		t.Fatal("missing target must be refused")
	}
	if !sameRows(poolRows(t, h, "openrouter"), before) {
		t.Fatal("a refused reset must not touch disk")
	}
	cleared, ok := pool.ResetStatus("row1")
	if !ok || cleared.LastStatus != "" {
		t.Fatal("row1 must be cleared")
	}
	after := poolRows(t, h, "openrouter")
	if !sameAnyMap(rowByID(after, "row0"), before[0]) {
		t.Fatalf("row0 must keep its cooldown: %v vs %v", rowByID(after, "row0"), before[0])
	}
	if rowByID(after, "row1")["last_error_reset_at"] != nil {
		t.Fatal("row1 reset must reach disk")
	}
	if pool.ResetStatuses() != 1 {
		t.Fatal("one remaining reset expected")
	}
	for _, r := range poolRows(t, h, "openrouter") {
		if r["last_status"] != nil {
			t.Fatalf("every row cleared expected: %v", r)
		}
	}
}

func sameRows(a, b []map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameAnyMap(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestSelectionCountsOnlyReturnedSelections(t *testing.T) {
	for _, strategy := range []string{StrategyFillFirst, StrategyRoundRobin, StrategyRandom, StrategyLeastUsed} {
		t.Run(strategy, func(t *testing.T) {
			h := newTestHome(t)
			pool := adminPool(t, h, "openrouter", false)
			pool.strategy = strategy
			selected := map[string]int{}
			for i := 0; i < 2; i++ {
				selected[mustSelect(t, pool, "").ID]++
			}
			for _, e := range pool.Entries() {
				if e.RequestCount != selected[e.ID] {
					t.Fatalf("request_count %d for %s, selected %d", e.RequestCount, e.ID, selected[e.ID])
				}
			}
			pool.ResetStatus("row0")
			total := 0
			for _, r := range poolRows(t, h, "openrouter") {
				n, _ := anyInt(r["request_count"])
				total += n
			}
			if total != 2 {
				t.Fatalf("persisted counts must sum to 2, got %d", total)
			}
			pool.mu.Lock()
			pool.currentID = ""
			pool.mu.Unlock()
			pool.TryRefreshMatching("", "")
			if countOf(pool) != 2 {
				t.Fatal("a refresh lookup must not count")
			}
			if _, ok := pool.Peek(); !ok || countOf(pool) != 2 {
				t.Fatal("peek must not count")
			}
		})
	}
}

func countOf(pool *Pool) int {
	total := 0
	for _, e := range pool.Entries() {
		total += e.RequestCount
	}
	return total
}

func TestPriorityPersistsContiguousOrderWithoutClearingCooldown(t *testing.T) {
	h := newTestHome(t)
	pool := adminPool(t, h, "openrouter", true)
	before := map[string]float64{}
	for _, e := range pool.Entries() {
		before[e.ID] = e.LastErrorResetAt
	}
	moved, ok := pool.MoveEntry("row1", -5)
	if !ok || moved.Priority != 0 {
		t.Fatalf("row1 must move to the front: %+v", moved)
	}
	rows := poolRows(t, h, "openrouter")
	if rows[0]["id"] != "row1" || rows[1]["id"] != "row0" {
		t.Fatalf("disk order must follow: %v", rows)
	}
	moved, ok = pool.MoveEntry("row1", 99)
	if !ok || moved.Priority != 1 {
		t.Fatalf("row1 must move to the back: %+v", moved)
	}
	entries := pool.Entries()
	if entries[0].ID != "row0" || entries[0].Priority != 0 || entries[1].ID != "row1" || entries[1].Priority != 1 {
		t.Fatalf("contiguous order expected: %+v", entries)
	}
	for _, e := range entries {
		if e.LastErrorResetAt != before[e.ID] {
			t.Fatal("moving must not clear cooldowns")
		}
	}
	snapshot := poolRows(t, h, "openrouter")
	if _, ok := pool.MoveEntry("missing", 0); ok {
		t.Fatal("missing entry refused")
	}
	if !sameRows(poolRows(t, h, "openrouter"), snapshot) {
		t.Fatal("a refused move must not touch disk")
	}
}

func TestPriorityHonorsAnthropicManualFirst(t *testing.T) {
	h := newTestHome(t)
	pool := adminPool(t, h, "anthropic", false)
	pool.mu.Lock()
	pool.entries[1].Source = "env:ANTHROPIC_API_KEY"
	pool.mu.Unlock()
	moved, ok := pool.MoveEntry("row1", 0)
	if !ok || moved.Priority != 1 {
		t.Fatalf("seeded rows sort after manual rows: %+v", moved)
	}
	entries := pool.Entries()
	if entries[0].ID != "row0" || entries[1].ID != "row1" {
		t.Fatalf("manual first expected: %+v", entries)
	}
}

// ---- test_credential_pool_no_entries_log_throttle.py ----

func TestEmptyPoolLogsOnceWithinThrottleWindow(t *testing.T) {
	h := newTestHome(t)
	sink := &recordingHandler{}
	env := testEnv(t, h)
	env.Log = newRecordingLogger(sink)
	clock := time.Unix(1000, 0)
	nowFunc = func() time.Time { return clock }
	t.Cleanup(func() { nowFunc = time.Now })
	pool := NewPool(env, "test", nil)
	for i := 0; i < 50; i++ {
		clock = clock.Add(100 * time.Millisecond)
		if _, ok := pool.Select(""); ok {
			t.Fatal("empty pool selects nothing")
		}
	}
	if n := sink.count("credential pool: no available entries (all exhausted or empty)"); n != 1 {
		t.Fatalf("expected one throttled log line, got %d", n)
	}
}

func TestLogsAgainAfterThrottleWindowElapses(t *testing.T) {
	h := newTestHome(t)
	sink := &recordingHandler{}
	env := testEnv(t, h)
	env.Log = newRecordingLogger(sink)
	clock := time.Unix(1000, 0)
	nowFunc = func() time.Time { return clock }
	t.Cleanup(func() { nowFunc = time.Now })
	pool := NewPool(env, "test", nil)
	pool.Select("")
	clock = clock.Add(NoAvailableEntriesLogThrottle + time.Second)
	pool.Select("")
	if n := sink.count("credential pool: no available entries (all exhausted or empty)"); n != 2 {
		t.Fatalf("expected two log lines, got %d", n)
	}
}

func TestSuccessfulSelectionRearmsThrottle(t *testing.T) {
	h := newTestHome(t)
	sink := &recordingHandler{}
	env := testEnv(t, h)
	env.Log = newRecordingLogger(sink)
	clock := time.Unix(1000, 0)
	nowFunc = func() time.Time { return clock }
	t.Cleanup(func() { nowFunc = time.Now })
	pool := NewPool(env, "test", nil)
	pool.Select("")
	clock = clock.Add(time.Second)
	pool.Select("")
	pool.mu.Lock()
	pool.entries = []PooledCredential{{Provider: "test", ID: "a", Label: "a", AuthType: "api_key", Source: "manual", AccessToken: "tok-a", Extra: map[string]any{}}}
	pool.mu.Unlock()
	clock = clock.Add(time.Second)
	if _, ok := pool.Select(""); !ok {
		t.Fatal("recovered pool selects")
	}
	pool.mu.Lock()
	pool.entries = nil
	pool.mu.Unlock()
	clock = clock.Add(time.Second)
	pool.Select("")
	if n := sink.count("credential pool: no available entries (all exhausted or empty)"); n != 2 {
		t.Fatalf("re-armed throttle must log the flip immediately, got %d", n)
	}
}

// ---- test_credential_pool_oat_authtype.py ----

func TestAnthropicRealAPIKeyUnchanged(t *testing.T) {
	if PooledCredentialFromMap("anthropic", map[string]any{"auth_type": "api_key", "access_token": "sk-ant-api-EXAMPLE"}).AuthType != AuthTypeAPIKey {
		t.Fatal("a real API key stays api_key")
	}
}

func TestLoadHealsLegacyRowAndExposesItToResolver(t *testing.T) {
	h := newTestHome(t)
	token := "sk-ant-oat-legacy-manual"
	writeAuthStore(t, h, authWithPool("anthropic", row(map[string]any{"id": "legacy-oat", "label": "Legacy setup token", "auth_type": AuthTypeAPIKey, "priority": 0, "source": "manual", "access_token": token})))
	env := testEnv(t, h)
	entry := LoadPool(env, "anthropic").Entries()[0]
	if entry.AuthType != AuthTypeOAuth || poolRows(t, h, "anthropic")[0]["auth_type"] != AuthTypeOAuth {
		t.Fatal("legacy row must be healed to oauth in memory and on disk")
	}
	if ResolveAnthropicToken(env, "") != token {
		t.Fatal("the healed row must reach the resolver")
	}
}

// ---- test_credential_pool_seed_existing_env_sources.py ----

const synPrimary = "syn-primary-aaaaaaaaaaaaaaaaaaaaaaaa"
const synSecondary = "syn-secondary-bbbbbbbbbbbbbbbbbbbbbbbb"

func writeEnvHome(t *testing.T, h, provider string, envFile map[string]string, extraVar string) {
	t.Helper()
	text := ""
	for k, v := range envFile {
		text += k + "=" + v + "\n"
	}
	writeEnvFile(t, h, text)
	writeConfigYAML(t, h, "credential_pool_strategies:\n  "+provider+": round_robin\n")
	writeAuthStore(t, h, authWithPool(provider, row(map[string]any{"id": "x2", "label": extraVar, "auth_type": "api_key", "priority": 1, "source": "env:" + extraVar})))
}

func TestOnDiskEnvRowOutsideRegistryTupleJoinsRotation(t *testing.T) {
	for _, tc := range []struct{ provider, primary string }{{"deepseek", "DEEPSEEK_API_KEY"}, {"openrouter", "OPENROUTER_API_KEY"}} {
		t.Run(tc.provider, func(t *testing.T) {
			h := newTestHome(t)
			extra := tc.primary + "_2"
			writeEnvHome(t, h, tc.provider, map[string]string{tc.primary: synPrimary, extra: synSecondary}, extra)
			pool := LoadPool(testEnv(t, h), tc.provider)
			pool.mu.Lock()
			available, _ := pool.availableEntries(false, false, "")
			pool.mu.Unlock()
			keys := map[string]bool{}
			for _, e := range available {
				keys[e.RuntimeAPIKey()] = true
			}
			if len(keys) != 2 || !keys[synPrimary] || !keys[synSecondary] {
				t.Fatalf("both env keys must be in rotation: %v", keys)
			}
			sources := map[string]bool{}
			for i := 0; i < 4; i++ {
				sources[mustSelect(t, pool, "").Source] = true
			}
			if !sources["env:"+tc.primary] || !sources["env:"+extra] {
				t.Fatalf("round robin must visit both: %v", sources)
			}
			for _, r := range poolRows(t, h, tc.provider) {
				if _, has := r["access_token"]; has {
					t.Fatal("hydration is runtime only")
				}
			}
		})
	}
}

func TestUnsetEnvRowStaysOutOfRotationAndOnDisk(t *testing.T) {
	h := newTestHome(t)
	writeEnvHome(t, h, "deepseek", map[string]string{"DEEPSEEK_API_KEY": synPrimary}, "DEEPSEEK_API_KEY_2")
	pool := LoadPool(testEnv(t, h), "deepseek")
	pool.mu.Lock()
	available, _ := pool.availableEntries(false, false, "")
	pool.mu.Unlock()
	if len(available) != 1 || available[0].Source != "env:DEEPSEEK_API_KEY" {
		t.Fatalf("only the set var rotates: %+v", available)
	}
	found := false
	for _, e := range pool.Entries() {
		if e.Source == "env:DEEPSEEK_API_KEY_2" {
			found = true
		}
	}
	if !found {
		t.Fatal("the on disk reference must survive a process without the var")
	}
}

func TestNumberedEnvSiblingsSeedRotationWithoutConfig(t *testing.T) {
	for _, tc := range []struct{ provider, primary string }{{"deepseek", "DEEPSEEK_API_KEY"}, {"openrouter", "OPENROUTER_API_KEY"}} {
		t.Run(tc.provider, func(t *testing.T) {
			h := newTestHome(t)
			writeEnvFile(t, h, tc.primary+"="+synPrimary+"\n"+tc.primary+"_2="+synSecondary+"\n"+tc.primary+"_3=syn-third-cccccccccccccccccccccccc\n"+tc.primary+"_5=syn-fifth-eeeeeeeeeeeeeeeeeeeeeeee\n")
			writeConfigYAML(t, h, "credential_pool_strategies:\n  "+tc.provider+": round_robin\n")
			pool := LoadPool(testEnv(t, h), tc.provider)
			pool.mu.Lock()
			available, _ := pool.availableEntries(false, false, "")
			pool.mu.Unlock()
			sources := map[string]bool{}
			for _, e := range available {
				sources[e.Source] = true
			}
			if len(sources) != 3 || !sources["env:"+tc.primary] || !sources["env:"+tc.primary+"_2"] || !sources["env:"+tc.primary+"_3"] {
				t.Fatalf("siblings up to the first gap expected: %v", sources)
			}
			seen := map[string]bool{}
			for i := 0; i < 6; i++ {
				seen[mustSelect(t, pool, "").Source] = true
			}
			if len(seen) != 3 {
				t.Fatalf("round robin must visit all three: %v", seen)
			}
			rows := poolRows(t, h, tc.provider)
			if len(rows) != 3 {
				t.Fatalf("three rows expected: %v", rows)
			}
			for _, r := range rows {
				if _, has := r["access_token"]; has {
					t.Fatal("no secret on disk")
				}
			}
		})
	}
}

// ---- test_credential_pool_codex_singleton_isolation.py ----

func codexPoolJWT(account, sub string, exp float64) string {
	return jwtWithClaims(map[string]any{"exp": int(exp), "sub": sub, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
}

func isoAt(ts float64) string { return time.Unix(int64(ts), 0).UTC().Format("2006-01-02T15:04:05Z") }

func writeCodexStore(t *testing.T, h string, singletonTokens map[string]any, lastRefresh string, manual map[string]any) {
	t.Helper()
	seeded := map[string]any{"id": "seeded", "label": "device_code", "auth_type": "oauth", "priority": 0, "source": "device_code", "last_refresh": lastRefresh}
	for k, v := range singletonTokens {
		seeded[k] = v
	}
	manualRow := map[string]any{"id": "manual", "label": "second", "auth_type": "oauth", "priority": 1, "source": "manual:device_code"}
	for k, v := range manual {
		manualRow[k] = v
	}
	writeAuthStore(t, h, map[string]any{"version": 1, "active_provider": "openai-codex",
		"providers":       map[string]any{"openai-codex": map[string]any{"tokens": singletonTokens, "last_refresh": lastRefresh, "auth_mode": "chatgpt"}},
		"credential_pool": map[string]any{"openai-codex": []any{seeded, manualRow}}})
}

func stubCodexRefresh(t *testing.T, minted, mintedRT string, posted *[]string) {
	t.Helper()
	prev := tokensSingletonProviders["openai-codex"]
	spec := prev
	spec.refresh = func(ctx context.Context, accessToken, refreshToken string, timeout time.Duration) (map[string]any, error) {
		*posted = append(*posted, refreshToken)
		return map[string]any{"access_token": minted, "refresh_token": mintedRT, "last_refresh": isoAt(nowEpoch())}, nil
	}
	tokensSingletonProviders["openai-codex"] = spec
	t.Cleanup(func() { tokensSingletonProviders["openai-codex"] = prev })
}

func TestIndependentManualAccountRefreshesWithItsOwnPair(t *testing.T) {
	h := newTestHome(t)
	now := nowEpoch()
	aAT := codexPoolJWT("acct-A", "user-A", now+8*3600)
	bAT := codexPoolJWT("acct-B", "user-B", now+60)
	writeCodexStore(t, h, map[string]any{"access_token": aAT, "refresh_token": "rt-A"}, isoAt(now-3600), map[string]any{"access_token": bAT, "refresh_token": "rt-B", "last_refresh": isoAt(now - 7200)})
	var posted []string
	bNew := codexPoolJWT("acct-B", "user-B", now+8*3600)
	stubCodexRefresh(t, bNew, "rt-B2", &posted)
	pool := LoadPool(testEnv(t, h), "openai-codex")
	manual, _ := entryByID(pool, "manual")
	refreshed, ok := pool.refreshEntry(manual, false, "", false)
	if len(posted) != 1 || posted[0] != "rt-B" {
		t.Fatalf("the manual account must POST its own refresh token: %v", posted)
	}
	if !ok || refreshed.RefreshToken != "rt-B2" {
		t.Fatalf("rotation expected: %+v", refreshed)
	}
	onDisk := readAuthStore(t, h)
	manualRow := rowByID(poolRows(t, h, "openai-codex"), "manual")
	if manualRow["access_token"] != bNew || manualRow["refresh_token"] != "rt-B2" {
		t.Fatalf("manual row must hold the rotated pair: %v", manualRow)
	}
	tokens := onDisk["providers"].(map[string]any)["openai-codex"].(map[string]any)["tokens"].(map[string]any)
	if tokens["access_token"] != aAT || tokens["refresh_token"] != "rt-A" {
		t.Fatalf("the singleton must be untouched: %v", tokens)
	}
}

func TestSameAccountAliasAdoptsOnlyANewerSingleton(t *testing.T) {
	h := newTestHome(t)
	now := nowEpoch()
	aliasAT := codexPoolJWT("acct-A", "user-A", now+8*3600)
	staleSingleton := codexPoolJWT("acct-A", "user-A", now+3600)
	writeCodexStore(t, h, map[string]any{"access_token": staleSingleton, "refresh_token": "rt-consumed"}, isoAt(now-7200), map[string]any{"access_token": aliasAT, "refresh_token": "rt-alias", "last_refresh": isoAt(now - 60)})
	env := testEnv(t, h)
	pool := LoadPool(env, "openai-codex")
	alias, _ := entryByID(pool, "manual")
	synced := pool.syncEntryFromAuthStore(alias, false)
	if synced.AccessToken != aliasAT || synced.RefreshToken != "rt-alias" {
		t.Fatalf("an older singleton must not be adopted: %+v", synced)
	}
	freshAT := codexPoolJWT("acct-A", "user-A", now+9*3600)
	writeCodexStore(t, h, map[string]any{"access_token": freshAT, "refresh_token": "rt-fresh"}, isoAt(now+5), map[string]any{"access_token": aliasAT, "refresh_token": "rt-alias", "last_refresh": isoAt(now - 60)})
	synced = LoadPool(env, "openai-codex").syncEntryFromAuthStore(alias, false)
	if synced.AccessToken != freshAT || synced.RefreshToken != "rt-fresh" {
		t.Fatalf("a newer re-auth must be followed: %+v", synced)
	}
}

// ---- test_credential_pool_sole_rotate_recovery.py ----

func codexDeviceEntry(idx int, token, refresh string) map[string]any {
	return row(map[string]any{"id": "codex-" + string(rune('0'+idx)), "label": "codex-login", "auth_type": "oauth", "priority": idx, "source": "device_code", "access_token": token, "refresh_token": refresh})
}

func TestRevivedSoleEntryIsNoRecovery(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"openai-codex": []any{codexDeviceEntry(1, "tok-a", "rf-a")}},
		"providers": map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{"access_token": "tok-a", "refresh_token": "rf-a"}}}})
	pool := LoadPool(testEnv(t, h), "openai-codex")
	writeAuthStore(t, h, map[string]any{"version": 1, "credential_pool": map[string]any{"openai-codex": []any{codexDeviceEntry(1, "tok-a", "rf-a")}},
		"providers": map[string]any{"openai-codex": map[string]any{"tokens": map[string]any{"access_token": "tok-a-adopted", "refresh_token": "rf-a-adopted"}}}})
	if _, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 429, ErrorContext: map[string]any{"reason": "usage_limit_reached", "reset_at": nowEpoch() + 95.5*3600}, CredentialID: "codex-1"}); ok {
		t.Fatal("a rotation that hands back the just marked entry is no recovery")
	}
}

func TestMultiEntryPoolStillRotatesToHealthySibling(t *testing.T) {
	h := newTestHome(t)
	writeAuthStore(t, h, authWithPool("openai-codex", codexDeviceEntry(1, "tok-a", "rf-a"), codexDeviceEntry(2, "tok-b", "rf-b")))
	pool := LoadPool(testEnv(t, h), "openai-codex")
	next, ok := pool.MarkExhaustedAndRotate(RotationRequest{StatusCode: 429, ErrorContext: map[string]any{"reason": "usage_limit_reached", "reset_at": nowEpoch() + 95.5*3600}, CredentialID: "codex-1"})
	if !ok || next.ID != "codex-2" {
		t.Fatalf("healthy sibling expected: %+v", next)
	}
}
