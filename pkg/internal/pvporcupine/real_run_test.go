//go:build wakewheels

package pvporcupine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestRealLibraryMatchesPython(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
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
	m, err := Import(filepath.Join(root, "dev", "slices", "wakeword_s25", "ref", "pvp_whl", "unpacked", "pvporcupine"))
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
