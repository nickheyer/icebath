package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

var grantTestSecret = bytes.Repeat([]byte("s"), 32)

// Dispatch the peer cases verify grants against, overrides replacing single fields
func grantTestDispatch(t *testing.T, overrides map[string]any) (*HostedMemberDispatch, error) {
	t.Helper()
	policy, err := ExecutionPolicyMapping(context.Background(), "reviewer", map[string]any{"approvals": map[string]any{"mode": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Review the current room state."
	if value, ok := overrides["prompt"].(string); ok {
		prompt = value
		delete(overrides, "prompt")
	}
	sum := sha256.Sum256([]byte(prompt))
	value := map[string]any{"protocol_version": int64(2), "room_id": "room-1", "home_install_id": "install-home",
		"authority_gateway_id": "gateway-home", "authority_epoch": int64(2), "member_id": "member-reviewer",
		"target_install_id": "install-peer", "target_profile": "reviewer", "task_id": "task-1",
		"execution_generation": int64(1), "source_event_seq": int64(9), "cancellation_scope_id": "cancel-1",
		"prompt": prompt, "prompt_digest": hex.EncodeToString(sum[:]), "capability_digest": strings.Repeat("a", 64),
		"execution_policy_digest": policy.GetOr("policy_digest", nil), "trace_id": "trace-1"}
	for key, override := range overrides {
		value[key] = override
	}
	return HostedMemberDispatchFromMapping(value)
}

func mustGrantDispatch(t *testing.T, overrides map[string]any) *HostedMemberDispatch {
	t.Helper()
	dispatch, err := grantTestDispatch(t, overrides)
	if err != nil {
		t.Fatal(err)
	}
	return dispatch
}

// Grant issued for every coordinate of the dispatch
func grantForDispatch(t *testing.T, dispatch *HostedMemberDispatch, o RoomGrantIssue) string {
	t.Helper()
	o.GrantID, o.RoomID, o.HomeInstallID, o.AuthorityGatewayID = "grant-1", dispatch.RoomID, dispatch.HomeInstallID, dispatch.AuthorityGatewayID
	o.AuthorityEpoch, o.MemberID, o.TargetInstallID, o.TargetProfile = dispatch.AuthorityEpoch, dispatch.MemberID, dispatch.TargetInstallID, dispatch.TargetProfile
	o.ExecutionPolicyDigest = dispatch.ExecutionPolicyDigest
	token, err := IssueRoomGrant(context.Background(), grantTestSecret, o)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// Asserts err is a grant error whose message carries fragment
func requireGrantError(t *testing.T, err error, fragment string) {
	t.Helper()
	var grantErr *HostedRoomGrantError
	if !errors.As(err, &grantErr) || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("err = %v, want HostedRoomGrantError matching %q", err, fragment)
	}
}

func TestGatewayRoomGrantSecretIsPrivatePersistentAndNotAnAPIKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	profileHome := filepath.Join(root, "profiles", "reviewer")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", root)
	first, err := GatewayRoomGrantSecret(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := GatewayRoomGrantSecret(home.WithOverride(context.Background(), profileHome), "")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(root, ".room-link-grant-secret"))
	if err != nil {
		t.Fatal(err)
	}
	derived, err := DeriveRoomGrantSecret("gateway-api-key-1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || len(first) != 32 || bytes.Equal(stored, first) || bytes.Equal(first, derived) {
		t.Fatalf("secret not private, persistent and distinct: first=%x second=%x stored=%x", first, second, stored)
	}
}

func TestGatewayRoomGrantSecretHasOwnerOnlyMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix permission bits")
	}
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	if _, err := GatewayRoomGrantSecret(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, ".room-link-grant-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestGatewayRoomGrantSecretIsAtomicAcrossConcurrentWorkers(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", root)
	secrets := make([][]byte, 8)
	errs := make([]error, 8)
	var wg sync.WaitGroup
	for i := range secrets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			secrets[i], errs[i] = GatewayRoomGrantSecret(context.Background(), "")
		}()
	}
	wg.Wait()
	for i := range secrets {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if !bytes.Equal(secrets[i], secrets[0]) {
			t.Fatalf("worker %d secret differs", i)
		}
	}
	info, err := os.Stat(filepath.Join(root, ".room-link-grant-secret"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 32 {
		t.Fatalf("secret size = %d", info.Size())
	}
}

func TestDispatchRejectsUnknownFieldsAndPromptDigestMismatch(t *testing.T) {
	var peerErr *HostedRoomPeerError
	if _, err := grantTestDispatch(t, map[string]any{"extra": true}); !errors.As(err, &peerErr) || !strings.Contains(err.Error(), "unknown fields") {
		t.Fatalf("extra field err = %v", err)
	}
	if _, err := grantTestDispatch(t, map[string]any{"prompt_digest": strings.Repeat("0", 64)}); !errors.As(err, &peerErr) ||
		!strings.Contains(err.Error(), "prompt_digest") {
		t.Fatalf("digest mismatch err = %v", err)
	}
}

func TestRoomGrantIsScopedToExactRoomHomeTargetAndProfile(t *testing.T) {
	dispatch := mustGrantDispatch(t, map[string]any{})
	token := grantForDispatch(t, dispatch, RoomGrantIssue{IssuedAt: floatPtr(100), TTLSeconds: floatPtr(60)})
	claims, err := VerifyRoomGrant(grantTestSecret, token, dispatch, "dispatch", floatPtr(120))
	if err != nil {
		t.Fatal(err)
	}
	if claims.GetOr("grant_id", nil) != "grant-1" {
		t.Fatalf("grant_id = %v", claims.GetOr("grant_id", nil))
	}
	for _, other := range []map[string]any{{"target_profile": "other"}, {"member_id": "member-other"}, {"authority_epoch": int64(999)}} {
		_, err := VerifyRoomGrant(grantTestSecret, token, mustGrantDispatch(t, other), "dispatch", floatPtr(120))
		requireGrantError(t, err, "scope")
	}
}

func TestRoomGrantFailsClosedForTamperExpiryAndPermission(t *testing.T) {
	dispatch := mustGrantDispatch(t, map[string]any{})
	token := grantForDispatch(t, dispatch, RoomGrantIssue{Permissions: []any{"status"}, IssuedAt: floatPtr(100), TTLSeconds: floatPtr(10),
		StatusExpiresAt: floatPtr(120)})
	_, err := VerifyRoomGrant(grantTestSecret, token, dispatch, "dispatch", floatPtr(105))
	requireGrantError(t, err, "allow")
	claims, err := VerifyRoomGrant(grantTestSecret, token, dispatch, "status", floatPtr(111))
	if err != nil || claims.GetOr("grant_id", nil) != "grant-1" {
		t.Fatalf("status grant = %v, %v", claims, err)
	}
	_, err = VerifyRoomGrant(grantTestSecret, token, dispatch, "status", floatPtr(100+30*24*60*60))
	requireGrantError(t, err, "expired")
	_, err = VerifyRoomGrant(grantTestSecret, token[:len(token)-1]+"A", dispatch, "dispatch", floatPtr(105))
	requireGrantError(t, err, "signature")
}
