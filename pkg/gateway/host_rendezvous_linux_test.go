package gateway

import "testing"

func TestBootEpochMismatchRejectsATickMatch(t *testing.T) {
	dir := hostDir(t)
	r := publishGateway(t, dir)
	if r.BootEpoch == "" {
		t.Fatal("no boot epoch published")
	}
	foreign := *r
	foreign.BootEpoch = r.BootEpoch + "-other-boot"
	if !HostLivenessIsProven(r) || HostLivenessIsProven(&foreign) || !HostRecordIsStale(&foreign) {
		t.Fatal("another boot's epoch matched")
	}
}

func TestRecordedEpochWithUnreadableCurrentEpochIsUnprovable(t *testing.T) {
	dir := hostDir(t)
	r := publishGateway(t, dir)
	if r.BootEpoch == "" {
		t.Fatal("no boot epoch published")
	}
	prev := hostCurrentBootEpoch
	hostCurrentBootEpoch = func() string { return "" }
	t.Cleanup(func() { hostCurrentBootEpoch = prev })
	if recordIncarnationMatches(r) != nil || HostLivenessIsProven(r) || HostRecordIsStale(r) {
		t.Fatal("an unreadable current epoch is not unprovable")
	}
}
