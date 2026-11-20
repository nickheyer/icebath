//go:build !windows

package state

import (
	"fmt"
	"os"
	"testing"
	"time"
)

func TestTurnLeaseOfSiblingNamespaceIsNotStolen(t *testing.T) {
	pinPidNamespace(t, &LocalPidNamespace{ID: "111", Supported: true}, nil)
	db := leaseDB(t)
	readSession(t, db, NewSession{ID: "shared", Source: "test"})
	sibling := fmt.Sprintf("pid=%d:pidns=222:turn=webui-turn:platform=webui", deadPid(t))
	if !leaseTry(t, db, "shared", sibling, 0.3) {
		t.Fatal("sibling acquire")
	}
	contender := fmt.Sprintf("pid=%d:pidns=111:turn=gw-turn:platform=cli", os.Getpid())
	if leaseTry(t, db, "shared", contender, 300) {
		t.Fatal("sibling namespace lease stolen before its ttl")
	}
	time.Sleep(400 * time.Millisecond)
	if !leaseTry(t, db, "shared", contender, 300) {
		t.Fatal("expired sibling lease must release at ttl")
	}
	readSession(t, db, NewSession{ID: "legacy", Source: "test"})
	legacy := fmt.Sprintf("pid=%d:turn=old:platform=x", deadPid(t))
	if !leaseTry(t, db, "legacy", legacy, 300) {
		t.Fatal("legacy acquire")
	}
	if leaseTry(t, db, "legacy", contender, 300) {
		t.Fatal("unstamped holder must defer to ttl")
	}
}
