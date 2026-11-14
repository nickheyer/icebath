package observability

import (
	"reflect"
	"sort"
	"testing"
)

func TestSnapshotReportsPrepareAheadAndOlderSnapshotsStillPackage(t *testing.T) {
	_, ctx := directRuntime(t)
	states := map[string]string{}
	for _, c := range []struct {
		key   string
		value any
	}{{"True", true}, {"yes", "yes"}, {"on", "on"}, {"False", false}, {"false", "false"}, {"None", nil}} {
		fields, err := CollectInstallSnapshot(ctx, map[string]any{"compression": map[string]any{"prepare_ahead": c.value}})
		if err != nil {
			t.Fatal(err)
		}
		states[c.key] = fields["compression_prepare_ahead"]
	}
	want := map[string]string{"True": "on", "yes": "on", "on": "on", "False": "off", "false": "off", "None": "off"}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("%v", states)
	}
	current, err := CollectInstallSnapshot(ctx, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if current["compression_prepare_ahead"] != "off" {
		t.Fatalf("absent reads %q", current["compression_prepare_ahead"])
	}
	store := openStore(t, t.TempDir())
	before := map[string]string{}
	on := map[string]string{}
	for k, v := range current {
		on[k] = v
		if k != "compression_prepare_ahead" {
			before[k] = v
		}
	}
	on["compression_prepare_ahead"] = "on"
	for _, dims := range []map[string]string{before, on} {
		if err := store.RecordCounter(ctx, InstallSnapshotMetric, dims, testResource(), 1); err != nil {
			t.Fatal(err)
		}
	}
	paths := mustExport(t, store)
	if len(paths) != 1 {
		t.Fatalf("%v", paths)
	}
	pkg := readPackage(t, paths[0])
	validatePackage(t, 4, pkg)
	var got []string
	for _, m := range pkg["metrics"].([]any) {
		metric := m.(map[string]any)
		if metric["name"] != InstallSnapshotMetric {
			continue
		}
		value, ok := metric["dimensions"].(map[string]any)["compression_prepare_ahead"].(string)
		if !ok {
			value = "absent"
		}
		got = append(got, value)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"absent", "on"}) {
		t.Fatalf("%v", got)
	}
}
