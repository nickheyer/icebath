package pyjson

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadObjectOrEmptyToleratesBOMAndDegradesToEmpty(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, body []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bom := write("mem0.json", []byte("\xef\xbb\xbf{\"workspace\": \"bom-ws\", \"container_tag\": \"bom-tag\", \"enabled\": true, \"mode\": \"local_embedded\"}"))
	got := ReadObjectOrEmpty(bom)
	if got.Vals["workspace"] != "bom-ws" || got.Vals["container_tag"] != "bom-tag" || len(got.Keys) != 4 || got.Keys[3] != "mode" {
		t.Fatalf("bom object %+v", got)
	}
	for name, path := range map[string]string{
		"missing":    filepath.Join(dir, "absent.json"),
		"directory":  dir,
		"malformed":  write("bad.json", []byte("{not json")),
		"not object": write("list.json", []byte("[1, 2]")),
		"bad utf8":   write("bytes.json", []byte("{\"a\": \"\xff\"}")),
		"double bom": write("bom2.json", []byte("\xef\xbb\xbf\xef\xbb\xbf{}")),
	} {
		if got := ReadObjectOrEmpty(path); got == nil || len(got.Keys) != 0 || len(got.Vals) != 0 {
			t.Fatalf("%s gave %+v", name, got)
		}
	}
}
