package mcp

import (
	"context"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthIntegrationExternalRefreshPickedUpWithoutRestart(t *testing.T) {
	dir := freshManager(t)
	ctx := context.Background()
	tokens := writeTokenFile(t, dir, "srv.json", map[string]any{"access_token": "OLD_ACCESS", "token_type": "Bearer",
		"expires_in": 3600, "refresh_token": "OLD_REFRESH"})
	writeTokenFile(t, dir, "srv.client.json", map[string]any{"client_id": "test-client", "redirect_uris": []any{"http://127.0.0.1:12345/callback"},
		"grant_types": []any{"authorization_code", "refresh_token"}, "response_types": []any{"code"}, "token_endpoint_auth_method": "none"})
	m := getManager()
	p, err := m.getOrBuildProvider(ctx, "srv", "https://example.com/mcp", nil, HTTPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p.client.Transport = newStandIn(notFound)
	if err := p.initialize(ctx); err != nil || p.currentTokens.AccessToken != "OLD_ACCESS" {
		t.Fatalf("initialize %v %+v", err, p.currentTokens)
	}
	for i := 0; i < 2; i++ {
		if changed, err := m.invalidateIfDiskChanged(ctx, "srv", ""); changed || err != nil || !p.initialized {
			t.Fatalf("baseline %d changed %v err %v initialized %v", i, changed, err, p.initialized)
		}
	}
	writeTokenFile(t, dir, "srv.json", map[string]any{"access_token": "NEW_ACCESS", "token_type": "Bearer",
		"expires_in": 3600, "refresh_token": "NEW_REFRESH"})
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(tokens, future, future); err != nil {
		t.Fatal(err)
	}
	if changed, err := m.invalidateIfDiskChanged(ctx, "srv", ""); !changed || err != nil || p.initialized {
		t.Fatalf("external refresh missed changed %v err %v initialized %v", changed, err, p.initialized)
	}
	if err := p.initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if p.currentTokens.AccessToken != "NEW_ACCESS" || derefStr(p.currentTokens.RefreshToken) != "NEW_REFRESH" {
		t.Fatalf("tokens %+v", p.currentTokens)
	}
}

func TestOAuthIntegrationHandle401DeduplicatesConcurrentCallers(t *testing.T) {
	dir := freshManager(t)
	ctx := context.Background()
	writeTokenFile(t, dir, "srv.json", map[string]any{"access_token": "TOK", "token_type": "Bearer", "expires_in": 3600})
	m := getManager()
	if _, err := m.getOrBuildProvider(ctx, "srv", "https://example.com/mcp", nil, HTTPOptions{}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	release := make(chan struct{})
	prev := recoverDiskCheck
	recoverDiskCheck = func(m *OAuthManager, ctx context.Context, name, homeDir string) (bool, error) {
		calls.Add(1)
		<-release
		return prev(m, ctx, name, homeDir)
	}
	t.Cleanup(func() { recoverDiskCheck = prev })
	var wg, started sync.WaitGroup
	results := make([]bool, 10)
	errs := make([]error, 10)
	for i := range results {
		wg.Add(1)
		started.Add(1)
		go func() {
			defer wg.Done()
			started.Done()
			results[i], errs[i] = m.handle401(ctx, "srv", "SAME_FAILED_TOKEN")
		}()
	}
	started.Wait()
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != results[0] {
			t.Fatalf("results %v errs %v", results, errs)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("expected 1 recovery attempt, got %d", n)
	}
}
