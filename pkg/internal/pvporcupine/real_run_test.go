//go:build wakewheels

package pvporcupine

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Directory holding every member of a wheel archive written with its recorded permissions
func unpackWheel(t *testing.T, wheel string) string {
	t.Helper()
	r, err := zip.OpenReader(wheel)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	dir := t.TempDir()
	for _, f := range r.File {
		target := filepath.Join(dir, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		src, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(src)
		src.Close()
		if err != nil {
			t.Fatal(err)
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(target, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRealLibraryMatchesPython(t *testing.T) {
	root := unpackWheel(t, filepath.Join("testdata", "pvporcupine", "pvporcupine-4.0.3-py3-none-any.whl"))
	raw, err := os.ReadFile("testdata/real_run.json")
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		Keywords []string `json:"keywords"`
		Devices  []string `json:"devices"`
		Create   []any    `json:"create"`
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatal(err)
	}
	m, err := Import(filepath.Join(root, "pvporcupine"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Keywords(), ref.Keywords) {
		t.Fatalf("keywords %v, Python has %v", m.Keywords(), ref.Keywords)
	}
	devices, err := m.AvailableDevices(nil)
	if err != nil || !reflect.DeepEqual(devices, ref.Devices) {
		t.Fatalf("devices %v %v, Python has %v", devices, err, ref.Devices)
	}
	handle, err := m.Create("icebath-not-a-key", CreateOptions{Keywords: []string{"porcupine"}})
	if ref.Create[0] == "ok" {
		if err != nil {
			t.Fatalf("create failed with %v where Python succeeded", err)
		}
		handle.Delete()
		return
	}
	var pe *PorcupineError
	if !errors.As(err, &pe) {
		t.Fatalf("create gave %v (%T), Python raised %v", err, err, ref.Create)
	}
	stack := []any{}
	for _, line := range pe.MessageStack {
		stack = append(stack, line)
	}
	if pe.Class != ref.Create[0] || pe.Error() != ref.Create[1] || !reflect.DeepEqual(stack, ref.Create[2]) {
		t.Fatalf("create raised %s %q, Python raised %v", pe.Class, pe.Error(), ref.Create)
	}
}
