package gateway

import "testing"

func TestHostGatewayTopologyDescribeAndServes(t *testing.T) {
	topo := &HostGatewayTopology{PID: 42, Profiles: []string{"default", "Coder"}, Source: "host_record"}
	if got := topo.Describe(); got != "the host gateway (PID 42) serving profiles default, Coder" {
		t.Fatalf("%q", got)
	}
	if got := (&HostGatewayTopology{PID: 7}).Describe(); got != "the host gateway (PID 7) serving profiles an unknown profile set" {
		t.Fatalf("%q", got)
	}
	for name, want := range map[string]bool{"default": true, "coder": true, "CODER": true, "other": false, "": false} {
		if got, err := topo.Serves(name); err != nil || got != want {
			t.Fatalf("%q: %v %v", name, got, err)
		}
	}
}

func TestLaunchedByOtherTenantTreatsALegacyRecordAsOurs(t *testing.T) {
	ours := t.TempDir()
	if other, err := launchedByOtherTenant("", ours); err != nil || other {
		t.Fatalf("%v %v", other, err)
	}
	if other, err := launchedByOtherTenant(ours, ours); err != nil || other {
		t.Fatalf("%v %v", other, err)
	}
	if other, err := launchedByOtherTenant(t.TempDir(), ours); err != nil || !other {
		t.Fatalf("%v %v", other, err)
	}
}
