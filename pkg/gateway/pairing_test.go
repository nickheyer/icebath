package gateway

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Context bound to a fresh home with a store over the given pairing directory
func makePairingStore(t *testing.T, dir string) (context.Context, *PairingStore) {
	t.Helper()
	ctx := home.WithOverride(context.Background(), t.TempDir())
	store, err := newPairingStoreAt(ctx, dir, home.Dir(ctx), "")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store
}

func writePairingFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPairingObject(t *testing.T, path string) *pyjson.Object {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	object, err := pyjson.LoadsDict(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return object
}

func mustGenerate(t *testing.T, ctx context.Context, store *PairingStore, platform, userID, userName string) string {
	t.Helper()
	code, ok, err := store.GenerateCode(ctx, platform, userID, userName)
	if err != nil || !ok || len(code) != PairingCodeLength {
		t.Fatalf("generate_code(%s) = %q %v %v", userID, code, ok, err)
	}
	return code
}

func mustApproved(t *testing.T, ctx context.Context, store *PairingStore, platform, userID string) bool {
	t.Helper()
	approved, err := store.IsApproved(ctx, platform, userID)
	if err != nil {
		t.Fatal(err)
	}
	return approved
}

func mustApproveCode(t *testing.T, ctx context.Context, store *PairingStore, platform, code string) *PairingApproval {
	t.Helper()
	result, err := store.ApproveCode(ctx, platform, code)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustLockedOut(t *testing.T, store *PairingStore, platform string) bool {
	t.Helper()
	locked, err := store.isLockedOut(platform)
	if err != nil {
		t.Fatal(err)
	}
	return locked
}

func TestSplitPairingDirMigrationMergesNewApprovedIntoActiveLegacyDir(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	legacy, fresh := filepath.Join(h, "pairing"), filepath.Join(h, "platforms", "pairing")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	writePairingFile(t, filepath.Join(fresh, "feishu-approved.json"), `{"ou_user": {"user_name": "Alice", "approved_at": 123.0}}`)
	ctx := home.WithOverride(context.Background(), h)
	store, err := newPairingStoreAt(ctx, legacy, h, "")
	if err != nil {
		t.Fatal(err)
	}
	if !mustApproved(t, ctx, store, "feishu", "ou_user") {
		t.Fatal("the approval of the other layout was not merged")
	}
	if _, ok := readPairingObject(t, filepath.Join(legacy, "feishu-approved.json")).Get("ou_user"); !ok {
		t.Fatal("ou_user missing from the migrated file")
	}
}

func TestProfileScopedDiscoveryListApprovedScopesPlatformDiscoveryToProfileDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "home")
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	store, err := NewPairingStore(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "profiles", "alice", "platforms", "pairing"); store.dir != want {
		t.Fatalf("dir = %s want %s", store.dir, want)
	}
	store.mu.Lock()
	err = store.approveUser(ctx, "telegram", "tg-456", "Bob")
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !mustApproved(t, ctx, store, "telegram", "tg-456") {
		t.Fatal("not approved")
	}
	approved, err := store.ListApproved("")
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 || approved[0].GetOr("user_id", nil) != "tg-456" || approved[0].GetOr("platform", nil) != "telegram" {
		t.Fatalf("approved = %v", approved)
	}
}

func TestCodeGenerationCodeUniqueness(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	codes := map[string]bool{}
	for _, user := range []string{"user0", "user1", "user2"} {
		codes[mustGenerate(t, ctx, store, "telegram", user, "")] = true
	}
	if len(codes) != 3 {
		t.Fatalf("codes = %v", codes)
	}
}

func isLowerHex(text string) bool {
	return strings.Trim(text, "0123456789abcdef") == ""
}

func TestHashedStoragePendingFileContainsHashAndSalt(t *testing.T) {
	dir := t.TempDir()
	ctx, store := makePairingStore(t, dir)
	code := mustGenerate(t, ctx, store, "telegram", "user1", "Alice")
	raw := readPairingObject(t, filepath.Join(dir, "telegram-pending.json"))
	if raw.Len() != 1 {
		t.Fatalf("entries = %d", raw.Len())
	}
	entry := raw.Vals[raw.Keys[0]].(*pyjson.Object)
	hash, _ := entry.GetOr("hash", nil).(string)
	salt, _ := entry.GetOr("salt", nil).(string)
	if len(hash) != 64 || !isLowerHex(hash) || len(salt) != 32 || !isLowerHex(salt) {
		t.Fatalf("hash = %q salt = %q", hash, salt)
	}
	for _, key := range raw.Keys {
		if key == code {
			t.Fatal("the code is a key of the pending file")
		}
		for _, field := range raw.Vals[key].(*pyjson.Object).Vals {
			if field == code {
				t.Fatal("the code is stored in the pending file")
			}
		}
	}
}

func TestHashedStoragePlaintextCodeNotStored(t *testing.T) {
	dir := t.TempDir()
	ctx, store := makePairingStore(t, dir)
	code := mustGenerate(t, ctx, store, "telegram", "user1", "")
	raw, err := os.ReadFile(filepath.Join(dir, "telegram-pending.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), code) {
		t.Fatal("the pending file holds the plaintext code")
	}
}

func TestLegacyPendingFileCompatCleanupExpiredHandlesMalformedEntries(t *testing.T) {
	dir := t.TempDir()
	writePairingFile(t, filepath.Join(dir, "telegram-pending.json"),
		`{"broken1": "not a dict", "broken2": {"user_id": "x"}, "broken3": {"created_at": "not a number"}}`)
	ctx, store := makePairingStore(t, dir)
	if err := store.cleanupExpired(ctx, "telegram"); err != nil {
		t.Fatal(err)
	}
	if raw := readPairingObject(t, filepath.Join(dir, "telegram-pending.json")); raw.Len() != 0 {
		t.Fatalf("pending = %v", raw.Keys)
	}
}

func TestLegacyPendingFileCompatApproveCodeSkipsMalformedEntries(t *testing.T) {
	dir := t.TempDir()
	entry := pyjson.ObjectOf("broken", pyjson.ObjectOf("user_id", "x", "created_at", pairingNow(), "salt", "not-hex", "hash", "doesntmatter"))
	text, err := pyjson.Dumps(entry)
	if err != nil {
		t.Fatal(err)
	}
	writePairingFile(t, filepath.Join(dir, "telegram-pending.json"), text)
	ctx, store := makePairingStore(t, dir)
	if result := mustApproveCode(t, ctx, store, "telegram", "ABCD1234"); result != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestRateLimitingSameUserRateLimited(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	mustGenerate(t, ctx, store, "telegram", "user1", "")
	if code, ok, err := store.GenerateCode(ctx, "telegram", "user1", ""); err != nil || ok {
		t.Fatalf("second code = %q %v %v", code, ok, err)
	}
}

func TestMaxPendingMaxPendingPerPlatform(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	for i := range PairingMaxPendingPerPlatform {
		mustGenerate(t, ctx, store, "telegram", "user"+string(rune('0'+i)), "")
	}
	if code, ok, err := store.GenerateCode(ctx, "telegram", "user9", ""); err != nil || ok {
		t.Fatalf("code past the cap = %q %v %v", code, ok, err)
	}
}

func TestApprovalFlowApproveValidCode(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	code := mustGenerate(t, ctx, store, "telegram", "user1", "Alice")
	result := mustApproveCode(t, ctx, store, "telegram", code)
	if result == nil || result.UserID != "user1" || result.UserName != "Alice" {
		t.Fatalf("result = %+v", result)
	}
}

func TestApprovalFlowApproveCodeWithInternalSpacing(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	code := mustGenerate(t, ctx, store, "telegram", "user1", "Alice")
	spaced := "  " + strings.Join(strings.Split(code, ""), "   ") + "  "
	result := mustApproveCode(t, ctx, store, "telegram", spaced)
	if result == nil || result.UserID != "user1" || result.UserName != "Alice" {
		t.Fatalf("result = %+v", result)
	}
}

func TestApprovalFlowApproveCodeWithWordsStillFails(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	code := mustGenerate(t, ctx, store, "telegram", "user1", "Alice")
	if result := mustApproveCode(t, ctx, store, "telegram", "code "+code); result != nil {
		t.Fatalf("result = %+v", result)
	}
}

func TestApprovalFlowApprovedUserIsApproved(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	mustApproveCode(t, ctx, store, "telegram", mustGenerate(t, ctx, store, "telegram", "user1", "Alice"))
	if !mustApproved(t, ctx, store, "telegram", "user1") {
		t.Fatal("not approved")
	}
}

func mustPending(t *testing.T, ctx context.Context, store *PairingStore, platform string) []PendingPairing {
	t.Helper()
	pending, err := store.ListPending(ctx, platform)
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

func TestApprovalFlowApproveRequestIdFromPendingList(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	botCode := mustGenerate(t, ctx, store, "telegram", "user1", "Alice")
	requestID := mustPending(t, ctx, store, "telegram")[0].RequestID
	if requestID == "" || requestID == botCode {
		t.Fatalf("request id = %q", requestID)
	}
	result, err := store.ApproveRequest(ctx, "telegram", strings.ToUpper(requestID))
	if err != nil || result == nil || result.UserID != "user1" || result.UserName != "Alice" {
		t.Fatalf("result = %+v %v", result, err)
	}
	if remaining := mustPending(t, ctx, store, "telegram"); len(remaining) != 0 {
		t.Fatalf("remaining = %v", remaining)
	}
}

func TestApprovalFlowApproveRequestNeverRevealsOrAcceptsTheCodeDigest(t *testing.T) {
	dir := t.TempDir()
	ctx, store := makePairingStore(t, dir)
	botCode := mustGenerate(t, ctx, store, "telegram", "user1", "Alice")
	entry := mustPending(t, ctx, store, "telegram")[0]
	stored := readPairingObject(t, filepath.Join(dir, "telegram-pending.json")).Vals[entry.RequestID].(*pyjson.Object)
	digest := stored.GetOr("hash", nil).(string)
	for _, value := range []any{entry.Platform, entry.RequestID, entry.UserID, entry.UserName} {
		if value == botCode {
			t.Fatal("the pending row reveals the code")
		}
	}
	if entry.RequestID == digest || entry.RequestID == digest[:8] {
		t.Fatal("the request id is derived from the code digest")
	}
	if result := mustApproveCode(t, ctx, store, "telegram", digest[:8]); result != nil {
		t.Fatalf("the digest prefix approved as a code: %+v", result)
	}
	if result, err := store.ApproveRequest(ctx, "telegram", digest[:8]); err != nil || result != nil {
		t.Fatalf("the digest prefix approved as a request: %+v %v", result, err)
	}
}

func TestApprovalFlowStaleRequestIdNeverLocksOutTheCodePath(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	code := mustGenerate(t, ctx, store, "telegram", "user1", "Alice")
	staleID := mustPending(t, ctx, store, "telegram")[0].RequestID
	if result, err := store.ApproveRequest(ctx, "telegram", staleID); err != nil || result == nil {
		t.Fatalf("first approval = %+v %v", result, err)
	}
	for range PairingMaxFailedAttempts + 3 {
		if result, err := store.ApproveRequest(ctx, "telegram", staleID); err != nil || result != nil {
			t.Fatalf("stale approval = %+v %v", result, err)
		}
	}
	if mustLockedOut(t, store, "telegram") {
		t.Fatal("stale request ids locked the platform out")
	}
	nextCode := mustGenerate(t, ctx, store, "telegram", "user2", "Bee")
	if mustApproveCode(t, ctx, store, "telegram", nextCode) == nil || code == nextCode {
		t.Fatal("the code path stopped working")
	}
}

func TestApprovalFlowWhatsappLegacyRawJidApprovalSurvivesAliasFlip(t *testing.T) {
	dir := t.TempDir()
	writePairingFile(t, filepath.Join(dir, "whatsapp", "session", "lid-mapping-999999999999999.json"), `"15551234567@s.whatsapp.net"`)
	writePairingFile(t, filepath.Join(dir, "whatsapp-approved.json"),
		`{"15551234567@s.whatsapp.net": {"user_name": "Legacy Alice", "approved_at": 1.0}}`)
	ctx := home.WithOverride(context.Background(), dir)
	store, err := newPairingStoreAt(ctx, dir, dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if !mustApproved(t, ctx, store, "whatsapp", "999999999999999@lid") {
		t.Fatal("the legacy JID approval did not survive the alias flip")
	}
}

func TestLockoutSuccessfulApprovalResetsFailureCounter(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	for range PairingMaxFailedAttempts - 1 {
		if mustApproveCode(t, ctx, store, "telegram", "WRONGCODE") != nil {
			t.Fatal("a wrong code approved")
		}
	}
	if mustLockedOut(t, store, "telegram") {
		t.Fatal("locked out one short of the cap")
	}
	if mustApproveCode(t, ctx, store, "telegram", mustGenerate(t, ctx, store, "telegram", "user1", "Alice")) == nil {
		t.Fatal("the valid code was refused")
	}
	limits, err := store.loadJSON(store.rateLimitPath())
	if err != nil {
		t.Fatal(err)
	}
	if fails, _ := pairingLimit(limits, "_failures:telegram"); fails != 0 {
		t.Fatalf("failures = %v", fails)
	}
	if mustApproveCode(t, ctx, store, "telegram", "WRONGCODE") != nil || mustLockedOut(t, store, "telegram") {
		t.Fatal("one fresh typo tripped the lockout")
	}
}

func TestLockoutLockoutBlocksCodeApproval(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	validCode := mustGenerate(t, ctx, store, "telegram", "attacker", "Attacker")
	for range PairingMaxFailedAttempts {
		if mustApproveCode(t, ctx, store, "telegram", "WRONGCODE") != nil {
			t.Fatal("a wrong code approved")
		}
	}
	if !mustLockedOut(t, store, "telegram") {
		t.Fatal("not locked out")
	}
	if mustApproveCode(t, ctx, store, "telegram", validCode) != nil || mustApproved(t, ctx, store, "telegram", "attacker") {
		t.Fatal("a valid code was accepted during the lockout")
	}
	limits, err := store.loadJSON(store.rateLimitPath())
	if err != nil {
		t.Fatal(err)
	}
	limits.Set("_lockout:telegram", pairingNow()-1)
	if err := savePairingJSONFile(ctx, store.rateLimitPath(), limits); err != nil {
		t.Fatal(err)
	}
	result := mustApproveCode(t, ctx, store, "telegram", validCode)
	if result == nil || result.UserID != "attacker" || !mustApproved(t, ctx, store, "telegram", "attacker") {
		t.Fatalf("result after the lockout = %+v", result)
	}
}

func TestCodeExpiryExpiredCodesCleanedUp(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	mustGenerate(t, ctx, store, "telegram", "user1", "")
	pending, err := store.loadJSON(store.pendingPath("telegram"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entryID := range pending.Keys {
		pending.Vals[entryID].(*pyjson.Object).Set("created_at", pairingNow()-PairingCodeTTLSeconds-1)
	}
	if err := savePairingJSONFile(ctx, store.pendingPath("telegram"), pending); err != nil {
		t.Fatal(err)
	}
	if remaining := mustPending(t, ctx, store, "telegram"); len(remaining) != 0 {
		t.Fatalf("remaining = %v", remaining)
	}
}

func TestRevokeRevokeApprovedUser(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	mustApproveCode(t, ctx, store, "telegram", mustGenerate(t, ctx, store, "telegram", "user1", "Alice"))
	if !mustApproved(t, ctx, store, "telegram", "user1") {
		t.Fatal("not approved")
	}
	if revoked, err := store.Revoke(ctx, "telegram", "user1"); err != nil || !revoked {
		t.Fatalf("revoke = %v %v", revoked, err)
	}
	if mustApproved(t, ctx, store, "telegram", "user1") {
		t.Fatal("still approved after the revoke")
	}
}

func TestRevokeRevokeFromOtherStoreIsNotUndoneByInflightApproval(t *testing.T) {
	dir := t.TempDir()
	ctx, seed := makePairingStore(t, dir)
	seed.mu.Lock()
	err := seed.approveUser(ctx, "telegram", "carol", "")
	seed.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	code := mustGenerate(t, ctx, seed, "telegram", "alice", "Alice")
	dashboard, err := newPairingStoreAt(ctx, dir, home.Dir(ctx), "")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := newPairingStoreAt(ctx, dir, home.Dir(ctx), "")
	if err != nil {
		t.Fatal(err)
	}
	approvedPath := dashboard.approvedPath("telegram")
	revokeDone := make(chan bool, 1)
	started := false
	realLoad := dashboard.loadJSON
	dashboard.loadJSON = func(path string) (*pyjson.Object, error) {
		data, err := realLoad(path)
		if path == approvedPath && !started {
			started = true
			finished := make(chan struct{})
			go func() {
				revoked, _ := cli.Revoke(ctx, "telegram", "carol")
				revokeDone <- revoked
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(300 * time.Millisecond):
			}
		}
		return data, err
	}
	approval := make(chan error, 1)
	go func() {
		_, err := dashboard.ApproveCode(ctx, "telegram", code)
		approval <- err
	}()
	select {
	case err := <-approval:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the approval never finished")
	}
	select {
	case revoked := <-revokeDone:
		if !revoked {
			t.Fatal("the revoke found no approved user")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the revoke never finished")
	}
	final := readPairingObject(t, approvedPath)
	if _, ok := final.Get("alice"); !ok {
		t.Fatalf("alice missing: %v", final.Keys)
	}
	if _, ok := final.Get("carol"); ok {
		t.Fatalf("the in flight approval wrote carol back: %v", final.Keys)
	}
}

func TestListAndClearListApproved(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	mustApproveCode(t, ctx, store, "telegram", mustGenerate(t, ctx, store, "telegram", "user1", "Alice"))
	approved, err := store.ListApproved("telegram")
	if err != nil {
		t.Fatal(err)
	}
	if len(approved) != 1 || approved[0].GetOr("user_id", nil) != "user1" || approved[0].GetOr("platform", nil) != "telegram" {
		t.Fatalf("approved = %v", approved)
	}
}

func TestProfileScopedStorageDefaultStoreUsesGlobalDir(t *testing.T) {
	dir := t.TempDir()
	_, store := makePairingStore(t, dir)
	if profile, scoped := store.Profile(); scoped || profile != "" {
		t.Fatalf("profile = %q %v", profile, scoped)
	}
	if store.dir != dir || store.approvedPath("weixin") != filepath.Join(dir, "weixin-approved.json") {
		t.Fatalf("dir = %s", store.dir)
	}
}

func TestProfileScopedStorageDefaultStoreIsNotFrozenAtFirstUse(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"first", "second"} {
		h := filepath.Join(root, name)
		store, err := NewPairingStore(home.WithOverride(context.Background(), h), "")
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(h, "platforms", "pairing"); store.dir != want {
			t.Fatalf("dir = %s want %s", store.dir, want)
		}
	}
}

func TestProfileScopedStorageProfileStoreUsesProfilesSubdir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	store, err := NewPairingStore(context.Background(), "yangyang")
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(root, "profiles", "yangyang", "platforms", "pairing")
	if profile, scoped := store.Profile(); !scoped || profile != "yangyang" {
		t.Fatalf("profile = %q %v", profile, scoped)
	}
	if store.dir != expected || store.approvedPath("weixin") != filepath.Join(expected, "weixin-approved.json") {
		t.Fatalf("dir = %s", store.dir)
	}
	if info, err := os.Stat(expected); err != nil || !info.IsDir() {
		t.Fatalf("the store did not create its directory: %v", err)
	}
}

func TestProfileScopedStorageProfileStoreMatchesProfileCliHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	profileHome := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := NewPairingStore(context.Background(), "coder")
	if err != nil {
		t.Fatal(err)
	}
	if want := home.HermesDir(profileHome, "platforms/pairing", "pairing"); store.dir != want {
		t.Fatalf("dir = %s want %s", store.dir, want)
	}
}

func TestProfileScopedStorageDefaultProfileStoreIsGlobalStore(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	named, err := NewPairingStore(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := NewPairingStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if named.dir != plain.dir {
		t.Fatalf("default profile dir %s, global dir %s", named.dir, plain.dir)
	}
}

func TestProfileScopedStorageProfileStoreMergesSplitPairingLayouts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	profileHome := filepath.Join(root, "profiles", "coder")
	writePairingFile(t, filepath.Join(profileHome, "pairing", "telegram-approved.json"), `{"legacy-user": {"user_name": "Legacy"}}`)
	writePairingFile(t, filepath.Join(profileHome, "platforms", "pairing", "telegram-approved.json"), `{"new-user": {"user_name": "New"}}`)
	ctx := context.Background()
	store, err := NewPairingStore(ctx, "coder")
	if err != nil {
		t.Fatal(err)
	}
	if !mustApproved(t, ctx, store, "telegram", "legacy-user") || !mustApproved(t, ctx, store, "telegram", "new-user") {
		t.Fatal("an approval of one layout was lost")
	}
}

func approveDirect(t *testing.T, ctx context.Context, store *PairingStore, platform, userID, userName string) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.approveUser(ctx, platform, userID, userName); err != nil {
		t.Fatal(err)
	}
}

func TestProfileScopedStorageProfileApprovalDoesNotLeakToGlobal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	globalStore, err := newPairingStoreAt(ctx, root, root, "")
	if err != nil {
		t.Fatal(err)
	}
	profileStore, err := NewPairingStore(ctx, "yangyang")
	if err != nil {
		t.Fatal(err)
	}
	approveDirect(t, ctx, profileStore, "weixin", "yangyang_user", "杨洋")
	approveDirect(t, ctx, globalStore, "weixin", "global_user", "Default")
	if !mustApproved(t, ctx, profileStore, "weixin", "yangyang_user") || mustApproved(t, ctx, profileStore, "weixin", "global_user") {
		t.Fatal("the profile store sees the wrong users")
	}
	if !mustApproved(t, ctx, globalStore, "weixin", "global_user") || mustApproved(t, ctx, globalStore, "weixin", "yangyang_user") {
		t.Fatal("the global store sees the wrong users")
	}
}

func TestProfileScopedStorageProfileUsesDistinctRateLimitFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ICEBATH_HOME", root)
	ctx := context.Background()
	globalStore, err := newPairingStoreAt(ctx, root, root, "")
	if err != nil {
		t.Fatal(err)
	}
	profileStore, err := NewPairingStore(ctx, "yangyang")
	if err != nil {
		t.Fatal(err)
	}
	if globalStore.rateLimitPath() != filepath.Join(root, "_rate_limits.json") {
		t.Fatalf("global rate limit path = %s", globalStore.rateLimitPath())
	}
	if want := filepath.Join(root, "profiles", "yangyang", "platforms", "pairing", "_rate_limits.json"); profileStore.rateLimitPath() != want {
		t.Fatalf("profile rate limit path = %s", profileStore.rateLimitPath())
	}
}

func saveApproved(t *testing.T, ctx context.Context, path string, pairs ...any) {
	t.Helper()
	if err := savePairingJSONFile(ctx, path, pyjson.ObjectOf(pairs...)); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalCacheReusesParseAndObservesReplacement(t *testing.T) {
	dir := t.TempDir()
	ctx, store := makePairingStore(t, dir)
	other, err := newPairingStoreAt(ctx, dir, home.Dir(ctx), "")
	if err != nil {
		t.Fatal(err)
	}
	path := store.approvedPath("telegram")
	saveApproved(t, ctx, path, "user1", pyjson.ObjectOf("user_name", "Alice", "approved_at", 1.0))
	var parsed *pyjson.Object
	for range 5 {
		if !mustApproved(t, ctx, store, "telegram", "user1") {
			t.Fatal("user1 not approved")
		}
		cached := store.approvedCache["telegram"].data
		if parsed != nil && cached != parsed {
			t.Fatal("an unchanged file was parsed again")
		}
		parsed = cached
	}
	if revoked, err := other.Revoke(ctx, "telegram", "user1"); err != nil || !revoked {
		t.Fatalf("revoke = %v %v", revoked, err)
	}
	if mustApproved(t, ctx, store, "telegram", "user1") {
		t.Fatal("the revoke of another store was not observed")
	}
	saveApproved(t, ctx, path, "user1", pyjson.ObjectOf("user_name", "Alice", "approved_at", 1.0))
	if !mustApproved(t, ctx, store, "telegram", "user1") {
		t.Fatal("the restored approval was not observed")
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(dir, "replacement")
	if err := os.WriteFile(replacement, bytes.ReplaceAll(raw, []byte("user1"), []byte("user2")), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(path); err != nil || after.Size() != before.Size() {
		t.Fatalf("replacement size differs: %v", err)
	}
	if mustApproved(t, ctx, store, "telegram", "user1") || !mustApproved(t, ctx, store, "telegram", "user2") {
		t.Fatal("a same size same mtime replacement was not observed")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if mustApproved(t, ctx, store, "telegram", "user2") {
		t.Fatal("a removed file still approves")
	}
	saveApproved(t, ctx, path, "user3", pyjson.NewObject())
	if !mustApproved(t, ctx, store, "telegram", "user3") {
		t.Fatal("a recreated file was not observed")
	}
}

func TestCliListedRequestIdAndBotCodeCanBeApproved(t *testing.T) {
	ctx, store := makePairingStore(t, t.TempDir())
	mustGenerate(t, ctx, store, "telegram", "listed-user", "Listed User")
	var out bytes.Buffer
	if _, err := pairingCommand(ctx, store, map[string]any{"pairing_action": "list"}, &out); err != nil {
		t.Fatal(err)
	}
	requestID := mustPending(t, ctx, store, "telegram")[0].RequestID
	if !strings.Contains(out.String(), requestID) {
		t.Fatalf("list output lacks the request id: %s", out.String())
	}
	out.Reset()
	if _, err := pairingCommand(ctx, store, map[string]any{"pairing_action": "approve", "platform": "telegram", "code": requestID}, &out); err != nil {
		t.Fatal(err)
	}
	requestOutput := out.String()
	botCode := mustGenerate(t, ctx, store, "telegram", "code-user", "Code User")
	out.Reset()
	if _, err := pairingCommand(ctx, store, map[string]any{"pairing_action": "approve", "platform": "telegram", "code": botCode}, &out); err != nil {
		t.Fatal(err)
	}
	approved, err := store.ListApproved("telegram")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, entry := range approved {
		ids = append(ids, entry.GetOr("user_id", nil).(string))
	}
	slices.Sort(ids)
	if !strings.Contains(requestOutput, "listed-user") || !strings.Contains(out.String(), "code-user") || !slices.Equal(ids, []string{"code-user", "listed-user"}) {
		t.Fatalf("request output %q, code output %q, approved %v", requestOutput, out.String(), ids)
	}
}

// Captures warnings of the default logger for one test
func capturePairingLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return &logs
}
