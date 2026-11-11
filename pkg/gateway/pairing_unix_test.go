//go:build unix

package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func TestSaveJsonFileSetsFilePermissions(t *testing.T) {
	ctx, _ := makePairingStore(t, t.TempDir())
	target := filepath.Join(t.TempDir(), "secret.json")
	if err := savePairingJSONFile(ctx, target, pyjson.ObjectOf("data", 1)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

// Makes a file unreadable for the test, skipping where the caller reads every file regardless of mode
func revokeRead(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("the superuser reads a mode 000 file")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

func TestUnreadablePairingFilePermissionErrorLogsWarningAndReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	approvedPath := filepath.Join(dir, "weixin-approved.json")
	writePairingFile(t, approvedPath, `{"o9cq80fake@im.wechat": {"user_name": "x", "approved_at": 0}}`)
	revokeRead(t, approvedPath)
	logs := capturePairingLogs(t)
	_, store := makePairingStore(t, dir)
	result, err := store.loadJSON(approvedPath)
	if err != nil || result.Len() != 0 {
		t.Fatalf("result = %v %v", result, err)
	}
	if text := logs.String(); !strings.Contains(text, "not readable") || !strings.Contains(text, "docker exec") || !strings.Contains(text, "-u icebath") {
		t.Fatalf("logs = %s", text)
	}
}

func TestApprovalCacheIsolatedAndUnreadableFilesFailClosed(t *testing.T) {
	root := t.TempDir()
	ctx, first := makePairingStore(t, filepath.Join(root, "first"))
	_, second := makePairingStore(t, filepath.Join(root, "second"))
	path := first.approvedPath("telegram")
	saveApproved(t, ctx, path, "user1", pyjson.NewObject())
	if !mustApproved(t, ctx, first, "telegram", "user1") || mustApproved(t, ctx, second, "telegram", "user1") {
		t.Fatal("the stores share their approvals")
	}
	revokeRead(t, path)
	if mustApproved(t, ctx, first, "telegram", "user1") {
		t.Fatal("an unreadable file still approves from the cache")
	}
	saveApproved(t, ctx, path, "user2", pyjson.NewObject())
	if mustApproved(t, ctx, first, "telegram", "user1") || !mustApproved(t, ctx, first, "telegram", "user2") {
		t.Fatal("the replaced file was not observed")
	}
}
