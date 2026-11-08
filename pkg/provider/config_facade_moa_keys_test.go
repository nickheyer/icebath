package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

const unknownKeyNotice = "not a recognized config key"

// Home with an empty .env and the given JSON config, facade output captured
func moaKeysHome(t *testing.T, data map[string]any) (string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	restoreProcessEnv(t)
	hermeticEnv(t)
	h := filepath.Join(t.TempDir(), "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(h, ".env"), nil, 0o600)
	text, _ := json.Marshal(data)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), text, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	config.ResetCache()
	t.Cleanup(config.ResetCache)
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	prevOut, prevErr, prevParse := config.FacadeStdout, config.FacadeNoticeOut, config.ParseFailureOut
	config.FacadeStdout, config.FacadeNoticeOut, config.ParseFailureOut = out, errOut, errOut
	config.ResetConfigParseFailures()
	t.Cleanup(func() {
		config.FacadeStdout, config.FacadeNoticeOut, config.ParseFailureOut = prevOut, prevErr, prevParse
		config.ResetConfigParseFailures()
	})
	return h, out, errOut
}

// Command sources with the provider owners and no plugin declared keys
func moaCommandSources() config.ConfigCommandSources {
	return config.ConfigCommandSources{
		RuntimeKeyOwners: config.RuntimeKeyOwners{
			SelectionConfigKeys: tools.SelectionConfigKeys,
			ActiveManifests:     func() []config.PluginConfigKeys { return nil },
		},
		LeftCoreTerminalKeys: LeftCoreTerminalKeys,
		IsEnvSettingKey:      IsEnvSettingKey,
		SaveEnvSetting:       SaveEnvSetting,
		ReadEnvSetting:       ReadEnvSetting,
		RemoveEnvSetting:     RemoveEnvSetting,
		DropStaleModelRoute:  DropStaleModelRoute,
	}
}

func moaKey(name, field string) string {
	return "moa.presets." + strings.ReplaceAll(name, ".", `\.`) + "." + field
}

// Sets a key and reports whether the unknown key notice was printed
func setNotice(t *testing.T, h, key, value string, out, errOut *bytes.Buffer) bool {
	t.Helper()
	out.Reset()
	errOut.Reset()
	if err := config.SetConfigValue(context.Background(), h, key, value, false, moaCommandSources()); err != nil {
		t.Fatalf("set %s: %v %s", key, err, errOut)
	}
	if !strings.Contains(out.String(), "Set "+key) {
		t.Fatalf("set %s printed %q", key, out)
	}
	return strings.Contains(out.String()+errOut.String(), unknownKeyNotice)
}

func moaPresetOf(t *testing.T, h, name string) map[string]any {
	t.Helper()
	cfg, err := config.Load(h, os.LookupEnv)
	if err != nil {
		t.Fatal(err)
	}
	preset, err := ResolveMoAPreset(cfg["moa"], name)
	if err != nil {
		t.Fatal(err)
	}
	return preset
}

func TestMoAPresetFieldsSetAndGetWithoutNotice(t *testing.T) {
	slot := map[string]any{"provider": "openrouter", "model": "test/actor"}
	for _, name := range []string{"default", "review", "review.v2", "code review"} {
		h, out, errOut := moaKeysHome(t, map[string]any{"moa": map[string]any{"presets": map[string]any{
			name: map[string]any{"reference_models": []any{slot}, "aggregator": slot}}}})
		for _, c := range []struct {
			field, value string
			want         any
		}{{"fanout", "every_n:5", "every_n:5"}, {"reference_timeout", "900", float64(900)}} {
			if setNotice(t, h, moaKey(name, c.field), c.value, out, errOut) {
				t.Fatalf("%s %s noticed %s", name, c.field, out)
			}
			out.Reset()
			errOut.Reset()
			if err := config.GetConfigValue(context.Background(), h, moaKey(name, c.field), true, false, moaCommandSources()); err != nil {
				t.Fatal(err)
			}
			var got any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil || got != c.want || strings.Contains(errOut.String(), unknownKeyNotice) {
				t.Fatalf("%s get %s = %q %v %q", name, c.field, out, err, errOut)
			}
		}
		preset := moaPresetOf(t, h, name)
		if preset["fanout"] != "every_n:5" || fmt.Sprint(preset["reference_timeout"]) != "900" {
			t.Fatalf("%s resolved %v %v", name, preset["fanout"], preset["reference_timeout"])
		}
	}
}

func TestMoAAggregatorSlotFieldsSetWithoutNotice(t *testing.T) {
	slot := map[string]any{"provider": "openrouter", "model": "test/actor"}
	for _, name := range []string{"default", "review"} {
		h, out, errOut := moaKeysHome(t, map[string]any{"moa": map[string]any{"presets": map[string]any{
			name: map[string]any{"reference_models": []any{slot}, "aggregator": slot}}}})
		for _, c := range [][2]string{{"provider", "anthropic"}, {"model", "test/judge"}, {"reasoning_effort", "high"}} {
			if setNotice(t, h, moaKey(name, "aggregator."+c[0]), c[1], out, errOut) {
				t.Fatalf("%s aggregator.%s noticed", name, c[0])
			}
		}
		want := map[string]any{"provider": "anthropic", "model": "test/judge", "reasoning_effort": "high"}
		if got := moaPresetOf(t, h, name)["aggregator"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s aggregator %v", name, got)
		}
	}
}

func TestMoANonFieldPathsStillNotice(t *testing.T) {
	for _, key := range []string{
		"moa.presets.default.fanoutt", "moa.presets.review.reference_timeot", `moa.presets.review\.v2.fanoutt`,
		"moa.presets.review.unexpected.fanout", "moa.presets.review.fanout.unexpected", "moa.presets. review .fanout",
		"moa.presets.review", "moa.presets.review.aggregator.bogus", "moa.presets.review.enabled.x", "moa.presets.review.fanout.n",
	} {
		h, out, errOut := moaKeysHome(t, map[string]any{})
		if !setNotice(t, h, key, "5", out, errOut) {
			t.Fatalf("%s set without the notice: %q", key, out)
		}
	}
}
