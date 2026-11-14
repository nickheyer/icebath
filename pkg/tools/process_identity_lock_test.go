package tools

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Lock body the Desktop SSH runtime writes for one backend
func validLockPayload(pid int, ownershipID, spawnNonce string) *pyjson.Object {
	return pyjson.ObjectOf("schemaVersion", int64(2), "protocolVersion", int64(1), "ownershipId", ownershipID, "spawnNonce", spawnNonce,
		"tokenFingerprint", strings.Repeat("a", 32), "pid", int64(pid), "port", int64(0), "profile", "default",
		"hermesPath", "/opt/icebath/bin/icebath", "hermesHome", "~/.icebath",
		"logPath", fmt.Sprintf("~/.icebath/desktop-ssh/%s/%s.log", ownershipID, spawnNonce), "startedAt", "2026-08-04T20:00:00Z")
}

// Writes a lock body as the backend lock of an ownership directory under lockRoot
func writeBackendLock(t *testing.T, lockRoot, ownershipID string, body *pyjson.Object) {
	t.Helper()
	dir := filepath.Join(lockRoot, ownershipID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	text, err := pyjson.Dumps(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, BackendLockName), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func ownedPIDs(owned map[int]bool) []int { return slices.Sorted(maps.Keys(owned)) }

func TestLockOwnedServePidsReadsValidBackendLock(t *testing.T) {
	oid, nonce := strings.Repeat("f", 32), strings.Repeat("d", 16)
	lockRoot := filepath.Join(t.TempDir(), "desktop-ssh")
	writeBackendLock(t, lockRoot, oid, validLockPayload(7777, oid, nonce))
	otherOID := strings.Repeat("e", 32)
	stale := validLockPayload(8888, otherOID, nonce)
	stale.Set("schemaVersion", int64(99))
	writeBackendLock(t, lockRoot, otherOID, stale)
	if got := ownedPIDs(lockOwnedServePids(lockRoot)); !slices.Equal(got, []int{7777}) {
		t.Fatalf("owned = %v", got)
	}
}

func TestLockOwnedServePidsSeesRootHomeLocksFromAProfileHome(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	profileHome := filepath.Join(root, "profiles", "flocki")
	if err := os.MkdirAll(profileHome, 0o755); err != nil {
		t.Fatal(err)
	}
	oid, nonce := strings.Repeat("f", 32), strings.Repeat("d", 16)
	writeBackendLock(t, filepath.Join(root, "desktop-ssh"), oid, validLockPayload(7777, oid, nonce))
	t.Setenv("ICEBATH_HOME", profileHome)
	t.Cleanup(home.PatchUserHome(tmp))
	if got := ownedPIDs(lockOwnedServePids("")); !slices.Equal(got, []int{7777}) {
		t.Fatalf("owned = %v", got)
	}
}

func TestValidLockfilePayloadRejectsWrongOwnerAndShape(t *testing.T) {
	oid, nonce := strings.Repeat("f", 32), strings.Repeat("d", 16)
	if !validLockfilePayload(validLockPayload(1, oid, nonce), oid) {
		t.Fatal("a valid payload was rejected")
	}
	if validLockfilePayload(validLockPayload(1, strings.Repeat("e", 32), nonce), oid) {
		t.Fatal("a payload of another ownership was accepted")
	}
	for name, mutate := range map[string]func(*pyjson.Object){
		"pid":   func(o *pyjson.Object) { o.Set("pid", int64(0)) },
		"port":  func(o *pyjson.Object) { o.Set("port", int64(70000)) },
		"nonce": func(o *pyjson.Object) { o.Set("spawnNonce", strings.Repeat("z", 15)) },
		"log":   func(o *pyjson.Object) { o.Set("logPath", fmt.Sprintf("~/.icebath/desktop-ssh/%s/other.log", oid)) },
	} {
		bad := validLockPayload(1, oid, nonce)
		mutate(bad)
		if validLockfilePayload(bad, oid) {
			t.Fatalf("a payload with a bad %s was accepted", name)
		}
	}
}
