package googlemeet

import (
	"reflect"
	"strings"
	"testing"
)

// Swaps the platform and command runner of the audio bridge for the test
func fakeBridge(t *testing.T, system string, run func(argv []string) (string, string, int, error)) {
	t.Helper()
	rp, rr := bridgePlatform, bridgeRun
	t.Cleanup(func() { bridgePlatform, bridgeRun = rp, rr })
	bridgePlatform = func() string { return system }
	bridgeRun = run
}

func TestSetupLinuxLoadsNullSinkAndVirtualSource(t *testing.T) {
	var calls [][]string
	fakeBridge(t, "Linux", func(argv []string) (string, string, int, error) {
		calls = append(calls, argv)
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "module-null-sink") {
			return "42\n", "", 0, nil
		}
		if strings.Contains(joined, "module-virtual-source") {
			return "43\n", "", 0, nil
		}
		t.Fatalf("unexpected pactl invocation: %v", argv)
		return "", "", 0, nil
	})
	br := newAudioBridge("icebath_meet")
	info, err := br.setup()
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0][0] != "pactl" || calls[0][1] != "load-module" || calls[0][2] != "module-null-sink" || calls[0][3] != "sink_name=icebath_meet_sink" {
		t.Fatal(calls)
	}
	if calls[1][1] != "load-module" || calls[1][2] != "module-virtual-source" || calls[1][3] != "source_name=icebath_meet_src" || calls[1][4] != "master=icebath_meet_sink.monitor" {
		t.Fatal(calls)
	}
	want := map[string]any{"platform": "linux", "device_name": "icebath_meet_src", "write_target": "icebath_meet_sink",
		"sample_rate": 48000, "channels": 2, "module_ids": []any{42, 43}}
	if !reflect.DeepEqual(info, want) {
		t.Fatal(info)
	}
	if d, _ := br.device(); d != "icebath_meet_src" {
		t.Fatal(d)
	}
	if w, _ := br.target(); w != "icebath_meet_sink" {
		t.Fatal(w)
	}
}

func TestTeardownLinuxUnloadsModulesInReverseOrder(t *testing.T) {
	fakeBridge(t, "Linux", func(argv []string) (string, string, int, error) {
		if strings.Contains(strings.Join(argv, " "), "module-null-sink") {
			return "42\n", "", 0, nil
		}
		return "43\n", "", 0, nil
	})
	br := newAudioBridge("icebath_meet")
	if _, err := br.setup(); err != nil {
		t.Fatal(err)
	}
	var unload [][]string
	bridgeRun = func(argv []string) (string, string, int, error) {
		unload = append(unload, argv)
		return "", "", 0, nil
	}
	br.teardown()
	if len(unload) != 2 || unload[0][1] != "unload-module" || unload[0][2] != "43" || unload[1][2] != "42" {
		t.Fatal(unload)
	}
	unload = nil
	br.teardown()
	if len(unload) != 0 {
		t.Fatal(unload)
	}
}

func TestPropertyAccessBeforeSetupRaises(t *testing.T) {
	br := newAudioBridge("icebath_meet")
	if _, err := br.device(); err == nil {
		t.Fatal("device before setup")
	}
	if _, err := br.target(); err == nil {
		t.Fatal("target before setup")
	}
}
