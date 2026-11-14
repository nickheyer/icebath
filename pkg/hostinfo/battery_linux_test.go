package hostinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxSensorReadsTheFirstBatteryAndTheACLine(t *testing.T) {
	root := t.TempDir()
	prev := powerSupplyPath
	powerSupplyPath = root
	t.Cleanup(func() { powerSupplyPath = prev })
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := platformSensorsBattery(); ok {
		t.Fatal("a tree without batteries must read as no battery")
	}
	write("BAT1/energy_now", "10\n")
	write("BAT1/energy_full", "100\n")
	write("BAT0/energy_now", "41000\n")
	write("BAT0/energy_full", "50000\n")
	write("BAT0/status", "Discharging\n")
	reading, ok := platformSensorsBattery()
	if !ok || reading.percent == nil || *reading.percent != 82 || reading.plugged == nil || *reading.plugged {
		t.Fatalf("reading %+v ok %v", reading, ok)
	}
	write("AC/online", "1\n")
	if reading, _ := platformSensorsBattery(); reading.plugged == nil || !*reading.plugged {
		t.Fatal("the AC line must win over the battery status")
	}
	if err := os.Remove(filepath.Join(root, "BAT0", "energy_full")); err != nil {
		t.Fatal(err)
	}
	write("BAT0/capacity", "77\n")
	if reading, ok := platformSensorsBattery(); !ok || *reading.percent != 77 {
		t.Fatalf("capacity reading %+v ok %v", reading, ok)
	}
}
