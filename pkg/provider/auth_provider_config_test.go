package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestProviderConfigRegistryFields(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	nous, err := providerConfig(ctx, "nous")
	if err != nil {
		t.Fatal(err)
	}
	if nous.DisplayName != "Nous Portal" || nous.AuthType != AuthTypeOAuthDeviceCode || nous.PortalBaseURL != DefaultNousPortalURL ||
		nous.BaseURL != DefaultNousInferenceURL || nous.ClientID != DefaultNousClientID || nous.Scope != DefaultNousScope {
		t.Fatalf("nous config %+v", nous)
	}
	mm, err := providerConfig(ctx, "minimax-oauth")
	if err != nil {
		t.Fatal(err)
	}
	wantExtra := map[string]any{"region": "global", "cn_portal_base_url": MinimaxOAuthCNBase, "cn_inference_base_url": MinimaxOAuthCNInference}
	if mm.AuthType != AuthTypeOAuthMiniMax || mm.PortalBaseURL != MinimaxOAuthGlobalBase || mm.BaseURL != MinimaxOAuthGlobalInference ||
		mm.ClientID != MinimaxOAuthClientID || mm.Scope != MinimaxOAuthScope || !reflect.DeepEqual(mm.Extra, wantExtra) {
		t.Fatalf("minimax config %+v", mm)
	}
	if _, err := providerConfig(ctx, "openrouter"); err == nil || err.Error() != "KeyError: 'openrouter'" {
		t.Fatalf("openrouter must be absent from the auth registry: %v", err)
	}
}

func TestFileLockStampsHolderPidAndClearsOnRelease(t *testing.T) {
	h := t.TempDir()
	s := NewStore(h)
	authPath := filepath.Join(h, "auth.json")
	release, err := s.fileLock(authPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(h, "auth.lock")
	raw, _ := os.ReadFile(lockPath)
	if strings.TrimSpace(string(raw)) != strconv.Itoa(os.Getpid()) {
		t.Fatalf("holder stamp %q", raw)
	}
	release()
	raw, _ = os.ReadFile(lockPath)
	if string(raw) != lockReleasePad {
		t.Fatalf("released lock file %q want %q", raw, lockReleasePad)
	}
}
