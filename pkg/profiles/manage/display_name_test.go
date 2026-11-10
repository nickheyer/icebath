package manage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Default home of the display name fixture, Path.home and the configured home under a temp dir
func displayEnv(t *testing.T) string {
	t.Helper()
	tmp := manageEnv(t)
	captureOut(t)
	return filepath.Join(tmp, ".icebath")
}

func isValueErr(err error) bool {
	var ve *profiles.ValueError
	return errors.As(err, &ve)
}

func strp(s string) *string { return &s }

func TestMetaAndValidationRoundTripPreservesOtherFields(t *testing.T) {
	def := displayEnv(t)
	if err := profiles.WriteProfileMeta(context.Background(), def, profiles.MetaUpdate{Description: strp("ops agent")}); err != nil {
		t.Fatal(err)
	}
	if err := profiles.WriteProfileMeta(context.Background(), def, profiles.MetaUpdate{DisplayName: strp("小助手")}); err != nil {
		t.Fatal(err)
	}
	meta := profiles.ReadProfileMeta(def)
	if meta.DisplayName != "小助手" || meta.Description != "ops agent" {
		t.Fatalf("%+v", meta)
	}
}

func TestMetaAndValidationMissingFileDefaultsEmpty(t *testing.T) {
	def := displayEnv(t)
	if got := profiles.ReadProfileMeta(def).DisplayName; got != "" {
		t.Fatalf("%q", got)
	}
}

func TestMetaAndValidationBotTitleReadFromHermesBotsUiMeta(t *testing.T) {
	def := displayEnv(t)
	writeFile(t, filepath.Join(def, "profile.yaml"), "ui_meta:\n  icebath-bots:\n    title: JordyV\n")
	if got := profiles.ReadProfileMeta(def).BotTitle; got != "JordyV" {
		t.Fatalf("meta %q", got)
	}
	infos, err := ListProfiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := ProfileToDict(infos[0]).Get("bot_title"); got != "JordyV" {
		t.Fatalf("roster %v", got)
	}
}

func TestMetaAndValidationBotTitleAbsentWhenNoBotsMeta(t *testing.T) {
	def := displayEnv(t)
	writeFile(t, filepath.Join(def, "profile.yaml"), "display_name: JordieF\nui_meta:\n  user-sections: {}\n")
	if got := profiles.ReadProfileMeta(def).BotTitle; got != "" {
		t.Fatalf("%q", got)
	}
}

func TestMetaAndValidationBotTitleNeverRaisesOnMalformedShapes(t *testing.T) {
	def := displayEnv(t)
	for _, text := range []string{"ui_meta: icebath-bots\n", "ui_meta:\n  icebath-bots: 7\n"} {
		writeFile(t, filepath.Join(def, "profile.yaml"), text)
		if got := profiles.ReadProfileMeta(def).BotTitle; got != "" {
			t.Fatalf("%q: %q", text, got)
		}
	}
}

func TestMetaAndValidationEmptyClearsKeyFromFile(t *testing.T) {
	def := displayEnv(t)
	if err := profiles.WriteProfileMeta(context.Background(), def, profiles.MetaUpdate{DisplayName: strp("Harumesu")}); err != nil {
		t.Fatal(err)
	}
	if err := profiles.WriteProfileMeta(context.Background(), def, profiles.MetaUpdate{DisplayName: strp("")}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(def, "profile.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := config.FastSafeLoad(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := data.(map[string]any)
	if !ok {
		t.Fatalf("not a mapping %T", data)
	}
	if _, has := m["display_name"]; has {
		t.Fatalf("%v", m)
	}
}

func TestMetaAndValidationSetterStripsAndCapsLength(t *testing.T) {
	displayEnv(t)
	if got, err := profiles.SetProfileDisplayName(context.Background(), "default", "  Harumesu "); err != nil || got != "Harumesu" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := profiles.SetProfileDisplayName(context.Background(), "default", strings.Repeat("x", 65)); !isValueErr(err) {
		t.Fatalf("long %v", err)
	}
}

func TestMetaAndValidationSetterMissingProfileRaises(t *testing.T) {
	displayEnv(t)
	if _, err := profiles.SetProfileDisplayName(context.Background(), "ghost", "Boo"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing %v", err)
	}
}

func TestRenameDefaultSetsDisplayNameOnly(t *testing.T) {
	def := displayEnv(t)
	got, err := RenameProfile(context.Background(), "default", "Harumesu")
	if err != nil || got != def {
		t.Fatalf("%q %v", got, err)
	}
	if !isDir(def) {
		t.Fatal("default home moved")
	}
	if dn := profiles.ReadProfileMeta(def).DisplayName; dn != "Harumesu" {
		t.Fatalf("%q", dn)
	}
}

func TestRenameDefaultReflectedInListProfiles(t *testing.T) {
	displayEnv(t)
	if _, err := RenameProfile(context.Background(), "default", "Harumesu"); err != nil {
		t.Fatal(err)
	}
	infos, err := ListProfiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.IsDefault {
			if info.Name != "default" || info.DisplayName != "Harumesu" {
				t.Fatalf("%+v", info)
			}
			return
		}
	}
	t.Fatal("no default profile listed")
}

func TestRenameDefaultRejectsEmptyAndPreservesExisting(t *testing.T) {
	def := displayEnv(t)
	ctx := context.Background()
	if _, err := RenameProfile(ctx, "default", "Harumesu"); err != nil {
		t.Fatal(err)
	}
	if _, err := RenameProfile(ctx, "default", "   "); !isValueErr(err) {
		t.Fatalf("blank accepted %v", err)
	}
	if dn := profiles.ReadProfileMeta(def).DisplayName; dn != "Harumesu" {
		t.Fatalf("%q", dn)
	}
}

func TestRenameDefaultRenameToDefaultStillReserved(t *testing.T) {
	displayEnv(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "worker", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RenameProfile(ctx, "worker", "default"); !isValueErr(err) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("%v", err)
	}
}

func TestRenameDefaultNamedRenameStillRealAndKeepsDisplayName(t *testing.T) {
	def := displayEnv(t)
	ctx := context.Background()
	skipAliasCollision(t)
	if _, err := CreateProfile(ctx, "oldname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	oldDir, err := profiles.GetProfileDir("oldname")
	if err != nil {
		t.Fatal(err)
	}
	if err := profiles.WriteProfileMeta(ctx, oldDir, profiles.MetaUpdate{DisplayName: strp("Old Friend")}); err != nil {
		t.Fatal(err)
	}
	newDir, err := RenameProfile(ctx, "oldname", "newname")
	if err != nil {
		t.Fatal(err)
	}
	if isDir(filepath.Join(def, "profiles", "oldname")) {
		t.Fatal("old dir remains")
	}
	if newDir != filepath.Join(def, "profiles", "newname") {
		t.Fatalf("%q", newDir)
	}
	if dn := profiles.ReadProfileMeta(newDir).DisplayName; dn != "Old Friend" {
		t.Fatalf("%q", dn)
	}
}

func TestResolutionUnaffectedDisplayNameIsNotAResolvableId(t *testing.T) {
	def := displayEnv(t)
	if _, err := RenameProfile(context.Background(), "default", "Harumesu"); err != nil {
		t.Fatal(err)
	}
	if dir, err := profiles.GetProfileDir("default"); err != nil || dir != def {
		t.Fatalf("%q %v", dir, err)
	}
	if !profiles.ProfileExists("default") || profiles.ProfileExists("harumesu") {
		t.Fatal("display name became resolvable")
	}
	if got, err := profiles.ResolveProfileEnv("default"); err != nil || got != def {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := profiles.ResolveProfileEnv("harumesu"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%v", err)
	}
}
