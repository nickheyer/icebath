package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Isolated home whose config.yaml the config handlers read and write
func configSetHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	return filepath.Join(h, "config.yaml")
}

// Applies one config.set key without a session
func cfgSet(t *testing.T, key string, value any) map[string]any {
	t.Helper()
	return callMethod(t, "config.set", map[string]any{"key": key, "value": value})
}

// Dotted key of the config file on disk
func cfgOnDisk(t *testing.T, path, dotted string) any {
	t.Helper()
	cfg, err := config.ReadUserConfigRaw(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var node any = cfg
	for _, part := range strings.Split(dotted, ".") {
		m, _ := node.(map[string]any)
		node = m[part]
	}
	return node
}

// Error code of an error response
func cfgErrCode(resp map[string]any) any {
	errObj, _ := resp["error"].(map[string]any)
	return errObj["code"]
}

func TestAMirroredSwitchReachesTheConfigFile(t *testing.T) {
	keys := make([]string, 0, len(displayToggleKeys))
	for key := range displayToggleKeys {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			path := configSetHome(t)
			if got := resultOf(t, cfgSet(t, key, "false")); !reflect.DeepEqual(got, map[string]any{"key": key, "value": false}) {
				t.Fatalf("false answer %v", got)
			}
			if v := cfgOnDisk(t, path, key); v != false {
				t.Fatalf("disk %v", v)
			}
			if got := resultOf(t, cfgSet(t, key, "true")); !reflect.DeepEqual(got, map[string]any{"key": key, "value": true}) {
				t.Fatalf("true answer %v", got)
			}
			if v := cfgOnDisk(t, path, key); v != true {
				t.Fatalf("disk %v", v)
			}
		})
	}
}

func TestTimestampsPersistsDisplayTimestamps(t *testing.T) {
	path := configSetHome(t)
	if got := resultOf(t, cfgSet(t, "timestamps", "on")); !reflect.DeepEqual(got, map[string]any{"key": "timestamps", "value": "on"}) {
		t.Fatalf("on answer %v", got)
	}
	if v := cfgOnDisk(t, path, "display.timestamps"); v != true {
		t.Fatalf("disk %v", v)
	}
	if got := resultOf(t, cfgSet(t, "timestamps", "off")); !reflect.DeepEqual(got, map[string]any{"key": "timestamps", "value": "off"}) {
		t.Fatalf("off answer %v", got)
	}
	if v := cfgOnDisk(t, path, "display.timestamps"); v != false {
		t.Fatalf("disk %v", v)
	}
}

// test_config_set_display_toggles.py and test_config_daybreak.py share this name, both cases run here
func TestANonBooleanIsRefusedRatherThanWritten(t *testing.T) {
	for _, key := range []string{"display.in_app_tips", "daybreak"} {
		t.Run(key, func(t *testing.T) {
			path := configSetHome(t)
			if code := cfgErrCode(cfgSet(t, key, "sometimes")); code != 4002 {
				t.Fatalf("code %v", code)
			}
			if _, err := os.Stat(path); err == nil {
				t.Fatal("config written")
			}
		})
	}
}

func TestEngineChoiceReachesTheConfigFileAndRoundTrips(t *testing.T) {
	path := configSetHome(t)
	if got := resultOf(t, cfgSet(t, "voice.voice_chat_mode", "gpt-live")); !reflect.DeepEqual(got, map[string]any{"key": "voice.voice_chat_mode", "value": "gpt-live"}) {
		t.Fatalf("answer %v", got)
	}
	if v := cfgOnDisk(t, path, "voice.voice_chat_mode"); v != "gpt-live" {
		t.Fatalf("disk %v", v)
	}
	if v := resultOf(t, cfgSet(t, "voice.voice_chat_mode", "Chained "))["value"]; v != "chained" {
		t.Fatalf("value %v", v)
	}
	if v := cfgOnDisk(t, path, "voice.voice_chat_mode"); v != "chained" {
		t.Fatalf("disk %v", v)
	}
}

func TestUnknownEngineIsRefusedRatherThanWritten(t *testing.T) {
	path := configSetHome(t)
	if code := cfgErrCode(cfgSet(t, "voice.voice_chat_mode", "realtime")); code != 4002 {
		t.Fatalf("code %v", code)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("config written")
	}
}

func TestTheSwitchRoundTripsThroughAgentDaybreak(t *testing.T) {
	path := configSetHome(t)
	get := func() map[string]any {
		return resultOf(t, callMethod(t, "config.get", map[string]any{"key": "daybreak"}))
	}
	if got := get(); !reflect.DeepEqual(got, map[string]any{"value": false}) {
		t.Fatalf("default %v", got)
	}
	if got := resultOf(t, cfgSet(t, "daybreak", true)); !reflect.DeepEqual(got, map[string]any{"key": "daybreak", "value": true}) {
		t.Fatalf("answer %v", got)
	}
	if v := cfgOnDisk(t, path, "agent.daybreak"); v != true {
		t.Fatalf("disk %v", v)
	}
	if got := get(); !reflect.DeepEqual(got, map[string]any{"value": true}) {
		t.Fatalf("after on %v", got)
	}
	if got := resultOf(t, cfgSet(t, "daybreak", "false")); !reflect.DeepEqual(got, map[string]any{"key": "daybreak", "value": false}) {
		t.Fatalf("answer %v", got)
	}
	if got := get(); !reflect.DeepEqual(got, map[string]any{"value": false}) {
		t.Fatalf("after off %v", got)
	}
}
