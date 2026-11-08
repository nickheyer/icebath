package gateway

import (
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/plugins"
)

func TestHostRendezvousServiceReadsTheLiveServeRecord(t *testing.T) {
	hostDir(t)
	svc := HostRendezvousService()
	if r, err := svc.ReadRecord(RoleServe); err != nil || r != nil {
		t.Fatalf("absent record gave %v %v", r, err)
	}
	if outcome, err := ClaimHostLock(RoleServe); err != nil || outcome != HostLockAcquired {
		t.Fatalf("claim %v %v", outcome, err)
	}
	port := 9119
	if PublishHostRecord(RoleServe, HostPublishOptions{Host: "0.0.0.0", Port: &port, Token: "live-session-token"}) == nil {
		t.Fatal("publish failed")
	}
	record, err := svc.ReadRecord(RoleServe)
	if err != nil || record == nil || record.Role != RoleServe || record.Port != 9119 {
		t.Fatalf("record %+v %v", record, err)
	}
	if !svc.RecordTokenIsConsistent(record) || svc.DialHost(record) != "127.0.0.1" {
		t.Fatal("live record not consistent or not dialled over loopback")
	}
	if token, err := svc.ReadToken(RoleServe); err != nil || token != "live-session-token" {
		t.Fatalf("token %q %v", token, err)
	}
	if err := os.WriteFile(HostTokenPath(RoleServe), []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if svc.RecordTokenIsConsistent(record) {
		t.Fatal("torn restart token reported consistent")
	}
	foreign := &plugins.HostRecord{Role: RoleServe, Port: 1}
	if !svc.RecordTokenIsConsistent(foreign) || svc.DialHost(foreign) != "127.0.0.1" {
		t.Fatal("tokenless record without a host")
	}
}
