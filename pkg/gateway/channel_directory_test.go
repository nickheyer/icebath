package gateway

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Points the alias overlay at a missing file and the directory at dir, restoring both after the test
func isolateChannelDirectory(t *testing.T, dir string) {
	t.Helper()
	savedDir, savedAliases := channelDirectoryPath, channelAliasesPath
	t.Cleanup(func() { channelDirectoryPath, channelAliasesPath = savedDir, savedAliases })
	channelAliasesPath = filepath.Join(t.TempDir(), "none.json")
	channelDirectoryPath = filepath.Join(dir, "channel_directory.json")
}

// Writes a directory file holding the given platforms JSON
func writeChannelDirectory(t *testing.T, dir, platforms string) {
	t.Helper()
	text := `{"updated_at": "2026-01-01T00:00:00", "platforms": ` + platforms + `}`
	if err := os.WriteFile(filepath.Join(dir, "channel_directory.json"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirectory(t *testing.T) {
	t.Run("test_missing_file", func(t *testing.T) {
		dir := t.TempDir()
		isolateChannelDirectory(t, dir)
		channelDirectoryPath = filepath.Join(dir, "nonexistent.json")
		result := loadChannelDirectory(context.Background())
		platforms, _ := result.Get("platforms")
		updated, has := result.Get("updated_at")
		if obj, ok := platforms.(*pyjson.Object); !ok || obj.Len() != 0 || !has || updated != nil {
			t.Fatalf("result %v", result)
		}
	})
}

func TestResolveChannelName(t *testing.T) {
	ctx := context.Background()
	resolve := func(t *testing.T, platforms, platform, name string) string {
		t.Helper()
		dir := t.TempDir()
		isolateChannelDirectory(t, dir)
		writeChannelDirectory(t, dir, platforms)
		got, err := resolveChannelName(ctx, platform, name)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("test_exact_match", func(t *testing.T) {
		platforms := `{"discord": [{"id": "111", "name": "bot-home", "guild": "MyServer", "type": "channel"},
			{"id": "222", "name": "general", "guild": "MyServer", "type": "channel"}]}`
		if got := resolve(t, platforms, "discord", "bot-home"); got != "111" {
			t.Fatal(got)
		}
		if got := resolve(t, platforms, "discord", "#bot-home"); got != "111" {
			t.Fatal(got)
		}
	})
	t.Run("test_case_insensitive", func(t *testing.T) {
		platforms := `{"slack": [{"id": "C01", "name": "Engineering", "type": "channel"}]}`
		if got := resolve(t, platforms, "slack", "engineering"); got != "C01" {
			t.Fatal(got)
		}
		if got := resolve(t, platforms, "slack", "ENGINEERING"); got != "C01" {
			t.Fatal(got)
		}
	})
	t.Run("test_prefix_match_unambiguous", func(t *testing.T) {
		platforms := `{"slack": [{"id": "C01", "name": "engineering-backend", "type": "channel"},
			{"id": "C02", "name": "design-team", "type": "channel"}]}`
		if got := resolve(t, platforms, "slack", "engineering"); got != "C01" {
			t.Fatal(got)
		}
	})
	t.Run("test_no_match_returns_none", func(t *testing.T) {
		if got := resolve(t, `{"telegram": [{"id": "123", "name": "John", "type": "dm"}]}`, "telegram", "nonexistent"); got != "" {
			t.Fatal(got)
		}
	})
	t.Run("raw_id_label_and_guild_forms", func(t *testing.T) {
		platforms := `{"slack": [{"id": "C0B0QV5434G", "name": "eng", "type": "channel"}],
			"discord": [{"id": "1", "name": "general", "guild": "Alpha"}, {"id": "2", "name": "general", "guild": "Beta"}],
			"telegram": [{"id": "-100", "name": "Team", "type": "group"}, {"id": "-200", "name": "Team chat", "type": "group"}]}`
		if got := resolve(t, platforms, "slack", " C0B0QV5434G "); got != "C0B0QV5434G" {
			t.Fatal(got)
		}
		if got := resolve(t, platforms, "discord", "beta/general"); got != "2" {
			t.Fatal(got)
		}
		if got := resolve(t, platforms, "telegram", "Team (group)"); got != "-100" {
			t.Fatal(got)
		}
		if got := resolve(t, platforms, "telegram", "tea"); got != "" {
			t.Fatalf("ambiguous prefix resolved to %q", got)
		}
	})
	t.Run("guild_qualified_name", func(t *testing.T) {
		platforms := `{"discord": [{"id": "1", "name": "ops", "guild": "Alpha"}, {"id": "2", "name": "ops", "guild": "Beta"}]}`
		if got := resolve(t, platforms, "discord", "Beta/ops"); got != "2" {
			t.Fatal(got)
		}
	})
	t.Run("malformed_entry_raises", func(t *testing.T) {
		dir := t.TempDir()
		isolateChannelDirectory(t, dir)
		writeChannelDirectory(t, dir, `{"slack": [{"id": "C01"}]}`)
		if _, err := resolveChannelName(ctx, "slack", "eng"); err == nil {
			t.Fatal("an entry without a name resolved")
		}
	})
}

func TestLookupChannelType(t *testing.T) {
	ctx := context.Background()
	lookup := func(t *testing.T, chatID string) any {
		t.Helper()
		dir := t.TempDir()
		isolateChannelDirectory(t, dir)
		writeChannelDirectory(t, dir, `{"discord": [{"id": "100", "name": "ideas", "guild": "Server1", "type": "forum"}]}`)
		got, err := lookupChannelType(ctx, "discord", chatID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	t.Run("test_forum_channel", func(t *testing.T) {
		if got := lookup(t, "100"); got != "forum" {
			t.Fatal(got)
		}
	})
	t.Run("test_unknown_chat_id_returns_none", func(t *testing.T) {
		if got := lookup(t, "999"); got != nil {
			t.Fatal(got)
		}
	})
}

func TestChannelAliases(t *testing.T) {
	t.Run("test_alias_injects_undiscovered_group", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		isolateChannelDirectory(t, dir)
		writeChannelDirectory(t, dir, `{"whatsapp": []}`)
		channelAliasesPath = filepath.Join(dir, "channel_aliases.json")
		if err := os.WriteFile(channelAliasesPath, []byte(`{"whatsapp": {"999@g.us": "marketing"}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := resolveChannelName(ctx, "whatsapp", "marketing"); err != nil || got != "999@g.us" {
			t.Fatalf("%q %v", got, err)
		}
		platforms, _ := loadChannelDirectory(ctx).Get("platforms")
		entries, _ := platforms.(*pyjson.Object).Get("whatsapp")
		var injected []*pyjson.Object
		for _, e := range entries.([]any) {
			if id, _ := e.(*pyjson.Object).Get("id"); id == "999@g.us" {
				injected = append(injected, e.(*pyjson.Object))
			}
		}
		if len(injected) == 0 {
			t.Fatal("alias entry missing")
		}
		if kind, _ := injected[0].Get("type"); kind != "group" {
			t.Fatal(kind)
		}
	})
	t.Run("alias_renames_a_discovered_entry_and_a_bom_file_reads", func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		isolateChannelDirectory(t, dir)
		text := "\xef\xbb\xbf" + `{"platforms": {"telegram": [{"id": "5", "name": "old", "type": "dm"}]}}`
		if err := os.WriteFile(channelDirectoryPath, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		channelAliasesPath = filepath.Join(dir, "channel_aliases.json")
		if err := os.WriteFile(channelAliasesPath, []byte(`{"telegram": {"5": "  boss  ", "6": ""}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := resolveChannelName(ctx, "telegram", "boss"); err != nil || got != "5" {
			t.Fatalf("%q %v", got, err)
		}
		platforms, _ := loadChannelDirectory(ctx).Get("platforms")
		entries, _ := platforms.(*pyjson.Object).Get("telegram")
		if len(entries.([]any)) != 1 {
			t.Fatalf("an empty alias added an entry %v", entries)
		}
	})
}
