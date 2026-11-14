package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"reflect"
	"runtime"
	"testing"
	"time"
	"unsafe"
	"weak"

	"github.com/nickheyer/icebath/pkg/tools"
)

func uuidHex(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

// Fresh profile home with every manager session locked before and after the test
func vaultProfile(t *testing.T) (context.Context, *tools.Env) {
	t.Helper()
	env := testEnv(t)
	ctx := tools.WithThreadLocals(context.Background())
	LockVault(ctx, nil)
	t.Cleanup(func() { LockVault(ctx, nil) })
	return ctx, env
}

func pinIdleTTL(t *testing.T, ttl time.Duration) {
	t.Helper()
	old := vaultIdleTTLNanos.Swap(int64(ttl))
	t.Cleanup(func() { vaultIdleTTLNanos.Store(old) })
}

// Whether needle is inside v the way a state dump sees it, function values staying closed
func reachable(v reflect.Value, needle string, seen map[uintptr]bool, depth int) bool {
	if !v.IsValid() || depth > 8 {
		return false
	}
	switch v.Kind() {
	case reflect.String:
		return v.String() == needle
	case reflect.Map:
		if seen[v.Pointer()] {
			return false
		}
		seen[v.Pointer()] = true
		iter := v.MapRange()
		for iter.Next() {
			if reachable(iter.Key(), needle, seen, depth+1) || reachable(iter.Value(), needle, seen, depth+1) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			if reachable(v.Index(i), needle, seen, depth+1) {
				return true
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if reachable(v.Field(i), needle, seen, depth+1) {
				return true
			}
		}
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return false
		}
		return reachable(v.Elem(), needle, seen, depth+1)
	}
	return false
}

func heldByUnlockGlobals(token string) bool {
	for _, global := range []any{vaultTokens, &vaultIdleTTLNanos, vaultSessionSlot} {
		if reachable(reflect.ValueOf(global), token, map[uintptr]bool{}, 0) {
			return true
		}
	}
	return false
}

// Stores a freshly built token and keeps only a weak reference to its bytes
//
//go:noinline
func storeWeakToken(t *testing.T, ctx context.Context, backend, prefix string) weak.Pointer[byte] {
	token := prefix + uuidHex(t)
	if !StoreVaultSessionToken(ctx, backend, token, nil) {
		t.Fatal("token not stored")
	}
	return weak.Make(unsafe.StringData(token))
}

func tokenBytesAlive(wp weak.Pointer[byte]) bool {
	runtime.GC()
	return wp.Value() != nil
}

func TestSessionTokenIsNotReachableFromTheUnlockModuleGlobals(t *testing.T) {
	ctx, _ := vaultProfile(t)
	token := "dummy-session-" + uuidHex(t)
	if !StoreVaultSessionToken(ctx, "bitwarden", token, nil) {
		t.Fatal("token not stored")
	}
	if got, ok := VaultSessionToken(ctx, "bitwarden"); !ok || got != token {
		t.Fatalf("the real call path lost the token: %q %v", got, ok)
	}
	if heldByUnlockGlobals(token) {
		t.Fatal("a plain import or module-state dump reaches the token")
	}
}

func TestIdleTokenIsDroppedAtTheTtlWithoutAnotherVaultCall(t *testing.T) {
	ctx, _ := vaultProfile(t)
	pinIdleTTL(t, 200*time.Millisecond)
	wp := storeWeakToken(t, ctx, "onepassword", "dummy-idle-")
	if !tokenBytesAlive(wp) {
		t.Fatal("the deep walk must see the live store, or it proves nothing")
	}
	waitUntil(t, 5*time.Second, "an expired token released without waiting for the next vault call",
		func() bool { return !tokenBytesAlive(wp) })
}

func TestSessionTeardownFencesALateUnlockAfterItsTokenExpired(t *testing.T) {
	ctx, _ := vaultProfile(t)
	pinIdleTTL(t, 50*time.Millisecond)
	sid := "dummy-session"
	SetVaultSessionID(ctx, &sid)
	defer SetVaultSessionID(ctx, nil)
	if !StoreVaultSessionToken(ctx, "bitwarden", "dummy-old-"+uuidHex(t), nil) {
		t.Fatal("token not stored")
	}
	generation := BeginVaultUnlock(ctx, "bitwarden")
	waitUntil(t, 5*time.Second, "the old token expires", func() bool { return !IsVaultUnlocked(ctx, "bitwarden") })
	ReleaseVaultSession("dummy-session")
	if StoreVaultSessionToken(ctx, "bitwarden", "dummy-late-"+uuidHex(t), &generation) {
		t.Fatal("a late unlock committed after its owning session ended")
	}
	if IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatal("bitwarden unlocked after the fenced commit")
	}
}

func TestSiblingSessionTeardownKeepsALoginAfterAnEarlierTokenExpired(t *testing.T) {
	ctx, _ := vaultProfile(t)
	pinIdleTTL(t, 50*time.Millisecond)
	defer SetVaultSessionID(ctx, nil)
	first, second := "dummy-first", "dummy-second"
	SetVaultSessionID(ctx, &first)
	if !StoreVaultSessionToken(ctx, "bitwarden", "dummy-first-"+uuidHex(t), nil) {
		t.Fatal("token not stored")
	}
	waitUntil(t, 5*time.Second, "the first token expires", func() bool { return !IsVaultUnlocked(ctx, "bitwarden") })
	pinIdleTTL(t, 60*time.Second)
	SetVaultSessionID(ctx, &second)
	if !StoreVaultSessionToken(ctx, "bitwarden", "dummy-second-"+uuidHex(t), nil) {
		t.Fatal("token not stored")
	}
	ReleaseVaultSession("dummy-first")
	if !IsVaultUnlocked(ctx, "bitwarden") {
		t.Fatal("the first session ending must not revoke the second's login")
	}
}
