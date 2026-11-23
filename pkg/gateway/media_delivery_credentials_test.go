package gateway

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRootCredentialPathsFollowThePythonOrder(t *testing.T) {
	want := []string{".env", "auth.json", "auth.lock", "credentials", "config.yaml", ".anthropic_oauth.json",
		"google_token.json", "google_oauth_pending.json", filepath.Join("auth", "google_oauth.json"), "webhook_subscriptions.json",
		"bws_cache.json", "bws_cache.enc.json", "op_cache.json", "photon-sidecar.json",
		"cache/bws_cache.json", "cache/bws_cache.enc.json", "cache/op_cache.json", "runtime/photon-sidecar.json",
		"pairing", "mcp-tokens", "webapp", "sessions", "browser-profile",
		"state.db", "state.db-wal", "state.db-shm", "state.db-journal", "kanban.db", "kanban.db-wal", "kanban.db-shm", "kanban.db-journal"}
	if got := rootCredentialPaths(); !reflect.DeepEqual(got, want) {
		t.Fatalf("root credential paths %v", got)
	}
}

func TestStoredWebappSignInStateIsNeverDelivered(t *testing.T) {
	_, dir := withRemoteEnv(t)
	token, notes := filepath.Join(dir, "webapp", "session-token"), filepath.Join(dir, "notes", "plan.txt")
	for _, p := range []string{token, notes} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if got, err := ValidateMediaDeliveryPath(ctx, token, ""); err != nil || got != "" {
		t.Fatalf("webapp credential delivered as %q %v", got, err)
	}
	if got, err := ValidateMediaDeliveryPath(ctx, notes, ""); err != nil || got == "" {
		t.Fatalf("an ordinary home file refused %q %v", got, err)
	}
	if !RemotePathIsDenied("/srv/a/.icebath/webapp/session-token", "") {
		t.Fatal("a sandbox webapp credential is not denied")
	}
}
