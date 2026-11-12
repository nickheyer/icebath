package storage

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/hermetic"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

func objectOf(pairs ...string) *Object {
	o := NewObject()
	for i := 0; i+1 < len(pairs); i += 2 {
		o.Set(pairs[i], pairs[i+1])
	}
	return o
}

func mustWrite(t *testing.T, file string, seed bool, values *Object) UIPreferencesSnapshot {
	t.Helper()
	snap, err := WriteUIPreferences(file, UIPreferencesWrite{Seed: seed, Values: values})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestAClientsLocalStorageImportOnlyAddsTheKeysAStoreLacks(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	file := UIPreferencesPath(t.TempDir())
	mustWrite(t, file, false, objectOf("icebath-desktop-mode-v1", "dark"))
	seeded := mustWrite(t, file, true, objectOf("icebath-desktop-mode-v1", "light", "icebath-desktop-theme-v2", "nous"))
	want := `{"icebath-desktop-mode-v1":"dark","icebath-desktop-theme-v2":"nous"}`
	if got := jsStringify(seeded.Values, ""); got != want {
		t.Fatalf("seeded %s", got)
	}
	read, err := ReadUIPreferences(file)
	if err != nil {
		t.Fatal(err)
	}
	if jsStringify(read.Values, "") != want {
		t.Fatalf("read %s", jsStringify(read.Values, ""))
	}
}

func TestUIPreferencesWritePayloadIsRefusedWholeForAnUnsupportedKeyOrValue(t *testing.T) {
	for payload, want := range map[string]string{
		`null`:                               "ui preferences write needs a values object",
		`{"values":[]}`:                      "ui preferences write needs a values object",
		`{"values":{"theme":"x"}}`:           "unsupported preference key: theme",
		`{"values":{"icebath.desktop.a":1}}`: "unsupported value for icebath.desktop.a",
	} {
		if _, err := ParseUIPreferencesWrite(json.RawMessage(payload)); err == nil || err.Error() != want {
			t.Fatalf("%s gave %v", payload, err)
		}
	}
	w, err := ParseUIPreferencesWrite(json.RawMessage(`{"seed":true,"values":{"hermes-desktop-x":null,"icebath.desktop.y":"1"}}`))
	if err != nil || !w.Seed || w.Values.Len() != 2 {
		t.Fatalf("%v %v", w, err)
	}
}

func TestUIPreferencesStoreFileMatchesTheElectronWriterAndReadsHermesKeysUnderIcebathNames(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	file := UIPreferencesPath(t.TempDir())
	if err := os.WriteFile(file, []byte("\xef\xbb\xbf"+`{"version":1,"generation":"g1","revision":4,"values":{"hermes-desktop-mode-v1":"dark","icebath-desktop-mode-v1":"light","hermes.desktop.k":"v","x":2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := ReadUIPreferences(file)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(snap); string(got) != `{"exists":true,"generation":"g1","revision":4,"values":{"icebath-desktop-mode-v1":"light","icebath.desktop.k":"v"}}` {
		t.Fatalf("snapshot %s", got)
	}
	after := mustWrite(t, file, false, objectOf("icebath.desktop.k", "w"))
	raw, _ := os.ReadFile(file)
	want := "{\n  \"version\": 1,\n  \"generation\": \"g1\",\n  \"revision\": 5,\n  \"values\": {\n    \"icebath-desktop-mode-v1\": \"light\",\n    \"icebath.desktop.k\": \"w\"\n  }\n}\n"
	if string(raw) != want || after.Revision != 5 {
		t.Fatalf("file %q", raw)
	}
	missing, err := ReadUIPreferences(UIPreferencesPath(t.TempDir()))
	if got, _ := json.Marshal(missing); err != nil || string(got) != `{"exists":false,"revision":0,"values":{}}` {
		t.Fatalf("missing %s %v", got, err)
	}
}
