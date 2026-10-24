package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestSaveWritesWhereTheNextStartReads(t *testing.T) {
	user := t.TempDir()
	project := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, user)
	t.Setenv(ProfileEnv, "")
	t.Setenv(ModelEnv, "")
	var c Config
	if err := c.SaveSetting(project, "cascade", "true"); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSetting(project, "compact_threshold", "0"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(user, ConfigFile))
	if !strings.Contains(string(data), `"cascade": true`) || !strings.Contains(string(data), `"threshold": 0`) {
		t.Fatalf("a missing user file is created with typed values %s", data)
	}
	os.WriteFile(filepath.Join(user, ConfigFile), []byte(`{"ui":{"theme":"light"},"hooks":{"Stop":[{"hooks":[{"command":"a && b"}]}]},"unknown":{"big":12345678901234567890}}`), 0o644)
	os.MkdirAll(filepath.Join(project, ProjectDir), 0o755)
	os.WriteFile(filepath.Join(project, ProjectDir, ConfigFile), []byte(`{"ui":{"theme":"contrast"},"max_tokens":10}`), 0o644)
	extra := filepath.Join(t.TempDir(), "extra.json")
	os.WriteFile(extra, []byte(`{"max_tokens":20}`), 0o644)
	c = Config{Sources: []string{extra}}
	if err := c.SaveSetting(project, "theme", "plain"); err != nil {
		t.Fatal(err)
	}
	if err := c.SaveSetting(project, "max_tokens", "4096"); err != nil {
		t.Fatal(err)
	}
	if err := Save(project, c.Sources, Field{Path: []string{"profile"}, Value: "openai"}, Field{Path: []string{"model"}, Value: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.UI.Theme != "plain" || got.MaxTokens != 4096 || got.Profile != "openai" || got.Model != "gpt-5" {
		t.Fatalf("every file that sets a field takes the new value %+v", got)
	}
	if again, _ := ReadConfigFile(extra); again.MaxTokens != 4096 {
		t.Fatal("a source given on the command line is written too")
	}
	data, _ = os.ReadFile(filepath.Join(user, ConfigFile))
	for _, kept := range []string{`"theme": "plain"`, `"command": "a && b"`, `"big": 12345678901234567890`, `"profile": "openai"`} {
		if !strings.Contains(string(data), kept) {
			t.Fatalf("user file keeps %s %s", kept, data)
		}
	}
	if strings.Contains(string(data), "max_tokens") {
		t.Fatalf("a field set elsewhere stays out of the user file %s", data)
	}
	order := []int{strings.Index(string(data), `"ui"`), strings.Index(string(data), `"hooks"`), strings.Index(string(data), `"unknown"`), strings.Index(string(data), `"profile"`), strings.Index(string(data), `"model"`)}
	for i := 1; i < len(order); i++ {
		if order[i-1] < 0 || order[i-1] > order[i] {
			t.Fatalf("keys keep their file order and new ones follow %s", data)
		}
	}
	if err := c.SaveSetting(project, "cascade", "true"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(user, ConfigFile))
	if strings.Index(string(data), `"theme"`) > strings.Index(string(data), `"cascade"`) {
		t.Fatalf("nested keys keep their order too %s", data)
	}
	os.WriteFile(filepath.Join(user, ConfigFile), []byte(`[1]`), 0o644)
	if err := c.SaveSetting(project, "cascade", "false"); err == nil {
		t.Fatal("a file that is not an object fails")
	}
	os.WriteFile(filepath.Join(user, ConfigFile), nil, 0o644)
	if err := c.SaveSetting(project, "cascade", "false"); err == nil {
		t.Fatal("an empty file fails")
	}
	if err := c.SaveSetting(project, "motion", "9"); err == nil {
		t.Fatal("a bad value is refused before anything is written")
	}
	os.WriteFile(filepath.Join(user, ConfigFile), []byte(`{"ui":"flat"}`), 0o644)
	if err := c.SaveSetting(project, "cascade", "false"); err == nil {
		t.Fatal("a field under a non object fails")
	}
	os.WriteFile(filepath.Join(user, ConfigFile), []byte(`{bad`), 0o644)
	if err := c.SaveSetting(project, "cascade", "false"); err == nil {
		t.Fatal("an unreadable file fails")
	}
	os.WriteFile(filepath.Join(user, ConfigFile), []byte(`{"ui":null}`), 0o644)
	if err := c.SaveSetting(project, "cascade", "false"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ReadConfigFile(filepath.Join(user, ConfigFile)); got.UI.Cascade == nil || *got.UI.Cascade {
		t.Fatal("a null object is replaced")
	}
	if err := c.SaveSetting(project, "extra.thinking", `{"type":"adaptive"}`); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Join(user, ConfigFile), 0o400)
	if err := c.SaveSetting(project, "cascade", "true"); err == nil {
		t.Fatal("a file that cannot be written fails")
	}
	got, _ = ReadConfigFile(filepath.Join(user, ConfigFile))
	var flat bytes.Buffer
	if json.Compact(&flat, got.Extra["thinking"]); flat.String() != `{"type":"adaptive"}` {
		t.Fatalf("extra fields keep their JSON %s", got.Extra["thinking"])
	}
}

func TestUnsetSettingsLeaveEveryFile(t *testing.T) {
	user := t.TempDir()
	project := t.TempDir()
	t.Setenv(profiles.ConfigDirEnv, user)
	os.WriteFile(filepath.Join(user, ConfigFile), []byte(`{"max_rounds":999,"ui":{"motion":0.5},"compaction":{"threshold":9000,"keep":2}}`), 0o644)
	os.MkdirAll(filepath.Join(project, ProjectDir), 0o755)
	os.WriteFile(filepath.Join(project, ProjectDir, ConfigFile), []byte(`{"max_rounds":5,"temperature":0.2}`), 0o644)
	c, err := Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"max_rounds", "temperature", "compact_threshold", "motion", "max_tokens", "mode", "theme", "cascade", "extra.thinking"} {
		if err := c.SaveSetting(project, key, ""); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
	if c.MaxRounds != 0 || c.Temperature != nil || c.Compaction.Threshold != 0 || c.UI.Motion != nil {
		t.Fatalf("unset settings clear the configuration %+v", c)
	}
	got, err := Discover(project)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRounds != 0 || got.Temperature != nil || got.Compaction.Threshold != 0 || got.UI.Motion != nil || got.Compaction.Keep != 2 {
		t.Fatalf("the next start reads them unset with the rest kept %+v", got)
	}
	data, _ := os.ReadFile(filepath.Join(user, ConfigFile))
	if strings.Contains(string(data), "ui") || strings.Contains(string(data), "max_rounds") || !strings.Contains(string(data), `"keep": 2`) {
		t.Fatalf("objects left empty go too\n%s", data)
	}
	if err := c.Set("nonsense", ""); err == nil {
		t.Fatal("unknown settings are refused even when unset")
	}
	c.Set("extra.note", "x")
	c.Set("extra.note", "")
	if _, ok := c.Extra["note"]; ok {
		t.Fatal("an extra field unsets")
	}
}
