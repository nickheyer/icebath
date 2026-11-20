package gateway

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestGatewayHostRecordCarriesThePythonKeySetInOrder(t *testing.T) {
	create := 1700000000.5
	start := int64(4242)
	r := &HostRecord{Role: RoleGateway, PID: 77, CreateTime: &create, StartTime: &start, BootEpoch: "boot-1",
		ProtocolVersion: HostProtocolVersion, Profiles: []string{"default", "work"}, UpdatedAt: "2026-01-01T00:00:00+00:00", Home: "/h"}
	o := r.ToJSON()
	want := []string{"role", "home", "pid", "createTime", "startTime", "bootEpoch", "host", "port",
		"protocolVersion", "tokenFingerprint", "profiles", "updatedAt"}
	if !reflect.DeepEqual(o.Keys, want) {
		t.Fatalf("keys %v", o.Keys)
	}
	if host, _ := o.Get("host"); host != "" {
		t.Fatalf("host %v", host)
	}
	if port, ok := o.Get("port"); !ok || port != nil {
		t.Fatalf("port %v %v", port, ok)
	}
	if fp, _ := o.Get("tokenFingerprint"); fp != "" {
		t.Fatalf("token fingerprint %v", fp)
	}
}

func TestPublishedGatewayRecordFileHasNoBoundPortAndReadsBack(t *testing.T) {
	hostDir(t)
	h := t.TempDir()
	r := publishGateway(t, h)
	raw, err := os.ReadFile(HostRecordPath(RoleGateway))
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["host"] != "" || decoded["port"] != nil || decoded["tokenFingerprint"] != "" {
		t.Fatalf("record %s", raw)
	}
	back := ReadHostRecord(RoleGateway, true)
	if back == nil || back.PID != r.PID || back.Home != h || !reflect.DeepEqual(back.Profiles, []string{"default"}) {
		t.Fatalf("read back %+v", back)
	}
}

func TestDescribeHostRecordNamesTheMissingPortAndTheServedProfiles(t *testing.T) {
	if got := DescribeHostRecord(&HostRecord{Role: RoleGateway, PID: 12, Profiles: []string{"default", "work"}}); got != "PID 12 (no bound port; profiles: default, work)" {
		t.Fatalf("describe %q", got)
	}
	if got := DescribeHostRecord(&HostRecord{Role: RoleGateway, PID: 13}); got != "PID 13 (no bound port; profiles: unknown)" {
		t.Fatalf("describe without profiles %q", got)
	}
}
