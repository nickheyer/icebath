package gatewaycli

import (
	"context"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestGatewayRuntimeSnapshotRunningAndMismatchFollowHermes(t *testing.T) {
	stopped := GatewayRuntimeSnapshot{Manager: "manual process"}
	if stopped.Running() || stopped.HasProcessServiceMismatch() {
		t.Fatalf("stopped %+v", stopped)
	}
	supervised := GatewayRuntimeSnapshot{Manager: "s6 (container supervisor)", ServiceInstalled: true, ServiceRunning: true}
	if !supervised.Running() || supervised.HasProcessServiceMismatch() {
		t.Fatalf("supervised %+v", supervised)
	}
	manual := GatewayRuntimeSnapshot{Manager: "systemd (user)", ServiceInstalled: true, GatewayPIDs: []int{4242}}
	if !manual.Running() || !manual.HasProcessServiceMismatch() {
		t.Fatalf("manual %+v", manual)
	}
	if serviceScopeLabel(true) != "system" || serviceScopeLabel(false) != "user" {
		t.Fatal("scope labels")
	}
}

func TestGetGatewayRuntimeSnapshotNamesAManagerForAnIdleHome(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	snapshot, err := GetGatewayRuntimeSnapshot(home.WithOverride(context.Background(), h), false)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Manager == "" || len(snapshot.GatewayPIDs) != 0 {
		t.Fatalf("%+v", snapshot)
	}
}
