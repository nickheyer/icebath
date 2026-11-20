package tools

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestEnvOrderNoteFollowsDictInsertion(t *testing.T) {
	o := &envOrder{keys: []string{"B", "A", "C"}}
	o.note(map[string]string{"A": "1", "C": "1", "Z": "1", "M": "1", "K": "1"}, "Z", "MISSING", "A")
	if want := []string{"A", "C", "Z", "K", "M"}; !reflect.DeepEqual(o.keys, want) {
		t.Fatalf("keys %v, want %v", o.keys, want)
	}
	o.note(map[string]string{"A": "1", "C": "1", "Z": "1", "M": "1", "K": "1", "B": "2"})
	if want := []string{"A", "C", "Z", "K", "M", "B"}; !reflect.DeepEqual(o.keys, want) {
		t.Fatalf("a dropped name that returns goes last, got %v want %v", o.keys, want)
	}
	var none *envOrder
	none.note(map[string]string{"A": "1"})
}

func TestBuildSubprocessEnvOrderedKeepsProcessOrderThenInsertions(t *testing.T) {
	installTestEnvHooks(t)
	home := t.TempDir()
	t.Setenv("ICEBATH_HOME", home)
	t.Setenv("ZED_ORDER_PROBE", "1")
	t.Setenv("ALPHA_ORDER_PROBE", "2")
	env := &Env{Home: home}
	child, err := BuildSubprocessEnvOrdered(context.Background(), env, SubprocessEnvOptions{ScrubSecrets: true, InheritProfileHome: true,
		Extra: map[string]string{"NEW_EXTRA_PROBE": "x", providerEnvForcePrefix + "FORCED_PROBE": "y", "ZED_ORDER_PROBE": "changed"}})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := BuildSubprocessEnv(context.Background(), env, SubprocessEnvOptions{ScrubSecrets: true, InheritProfileHome: true,
		Extra: map[string]string{"NEW_EXTRA_PROBE": "x", providerEnvForcePrefix + "FORCED_PROBE": "y", "ZED_ORDER_PROBE": "changed"}})
	if err != nil || !reflect.DeepEqual(plain, child.Vars) {
		t.Fatalf("map form differs from the ordered form: %v", err)
	}
	inherited := []string{}
	seen := map[string]bool{}
	for _, kv := range os.Environ() {
		name := kv[:strings.IndexByte(kv, '=')]
		if _, kept := child.Vars[name]; kept && !seen[name] {
			seen[name] = true
			inherited = append(inherited, name)
		}
	}
	if !reflect.DeepEqual(child.Keys[:len(inherited)], inherited) {
		t.Fatalf("inherited names %v, want %v", child.Keys[:len(inherited)], inherited)
	}
	if child.Vars["ZED_ORDER_PROBE"] != "changed" || child.Vars["FORCED_PROBE"] != "y" {
		t.Fatalf("extras not applied: %v %v", child.Vars["ZED_ORDER_PROBE"], child.Vars["FORCED_PROBE"])
	}
	added := child.Keys[len(inherited):]
	want := []string{"FORCED_PROBE", "NEW_EXTRA_PROBE"}
	for _, name := range profileHomeEnvOrder() {
		if _, set := child.Vars[name]; set && !seen[name] {
			want = append(want, name)
		}
	}
	if !reflect.DeepEqual(added, want) {
		t.Fatalf("inserted names %v, want %v", added, want)
	}
	if got := child.Environ(); len(got) != len(child.Keys) || got[0] != child.Keys[0]+"="+child.Vars[child.Keys[0]] {
		t.Fatalf("environ %v", got[:1])
	}
}
