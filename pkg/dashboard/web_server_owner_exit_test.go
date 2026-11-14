package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	ownerOID   = strings.Repeat("f", 32)
	ownerME    = strings.Repeat("a", 16)
	ownerNEW   = strings.Repeat("b", 16)
	ownerAgeOK = 600.0
)

func ownerLock(nonce string) map[string]any {
	return map[string]any{"schemaVersion": 2, "protocolVersion": 1, "ownershipId": ownerOID, "spawnNonce": nonce,
		"tokenFingerprint": strings.Repeat("c", 32), "pid": 4242, "port": 0, "profile": "default",
		"hermesPath": "/opt/hermes/bin/hermes", "hermesHome": "~/.hermes",
		"logPath": "~/.hermes/desktop-ssh/" + ownerOID + "/" + nonce + ".log", "startedAt": "2026-10-03T00:00:00Z"}
}

func TestRetiresOnlyOnAValidLockNamingAnotherSpawn(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "desktop-ssh", ownerOID, "backend.lock.json")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	verdict := func(body any, ageS float64) bool {
		switch b := body.(type) {
		case nil:
			os.Remove(lockPath)
		case string:
			if err := os.WriteFile(lockPath, []byte(b), 0o644); err != nil {
				t.Fatal(err)
			}
		default:
			raw, _ := json.Marshal(b)
			if err := os.WriteFile(lockPath, raw, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return ShouldRetireSuperseded(tools.ReadValidBackendLock(lockPath), ownerME, ageS)
	}
	if !verdict(ownerLock(ownerNEW), ownerAgeOK) {
		t.Fatal("a valid lock naming another spawn must retire")
	}
	if verdict(ownerLock(ownerME), ownerAgeOK) {
		t.Fatal("our own lock must stay up")
	}
	if verdict(nil, ownerAgeOK) {
		t.Fatal("a missing lock must stay up")
	}
	if verdict("{not json", ownerAgeOK) {
		t.Fatal("an unreadable lock must stay up")
	}
	skewed := ownerLock(ownerNEW)
	skewed["schemaVersion"] = 99
	if verdict(skewed, ownerAgeOK) {
		t.Fatal("another Desktop build's lock must stay up")
	}
	if verdict(ownerLock(ownerNEW), 5.0) {
		t.Fatal("a just spawned backend must stay up")
	}
	if verdict(ownerLock(ownerNEW), gateway.ReapMinAgeSeconds-1) {
		t.Fatal("the settle window must match the orphan reaper")
	}
}

func runOwnerWatchdog(t *testing.T, fence *fakeRetireFence, nonces []string) bool {
	var exit atomic.Bool
	i, clock := 0, 0.0
	readLock := func(string) map[string]any {
		nonce := nonces[len(nonces)-1]
		if i < len(nonces) {
			nonce = nonces[i]
			i++
		}
		if nonce == "" {
			return nil
		}
		return ownerLock(nonce)
	}
	now := func() float64 {
		v := clock
		clock += 1000
		return v
	}
	waitWatchdog(t, StartOwnerWatchdog(&exit, "backend.lock.json", ownerME, readLock, fence, 0.01, now, len(nonces)+2))
	return exit.Load()
}

func TestSupersededBackendRetiresThroughTheFenceOnlyWhenIdle(t *testing.T) {
	idle := &fakeRetireFence{idle: true}
	if !runOwnerWatchdog(t, idle, []string{ownerNEW, ownerNEW}) {
		t.Fatal("two consecutive supersessions must retire")
	}
	if !idle.committed.Load() {
		t.Fatal("admission must close before exit")
	}
	if runOwnerWatchdog(t, &fakeRetireFence{idle: true}, []string{ownerNEW, "", ownerNEW, ownerME}) {
		t.Fatal("no two in a row must stay up")
	}
	busy := &fakeRetireFence{idle: false}
	if runOwnerWatchdog(t, busy, []string{ownerNEW, ownerNEW, ownerNEW}) {
		t.Fatal("an in flight turn must keep the backend up")
	}
	if busy.committed.Load() {
		t.Fatal("a busy fence must not commit")
	}
}
