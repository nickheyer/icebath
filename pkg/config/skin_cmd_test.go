package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Temp home with a skins dir and display.skin set to name, returning the context and skins dir
func skinCmdHome(t *testing.T, active string) (context.Context, string) {
	t.Helper()
	ctx, dir := skinHome(t)
	ResetCache()
	t.Cleanup(ResetCache)
	captureFacade(t)
	h := filepath.Dir(dir)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("display:\n  skin: "+active+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return ctx, dir
}

// Loaded skin file as a mapping, failing the test when it does not parse
func loadSkinFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := FastSafeLoad(raw)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := doc.(map[string]any)
	return m
}

func runSkinSet(t *testing.T, ctx context.Context, key, value string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code, err := SkinCommand(ctx, SkinArgs{Command: "set", Key: key, Value: value}, testCommandSources(), &out, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

const oasisSkin = "name: oasis\ncolors:\n  background: \"#08201f\"\n  banner_title: \"#f2dfb3\"\n"

func TestSkinSetEditsActiveUserSkinInPlacePreservingEverythingElse(t *testing.T) {
	ctx, dir := skinCmdHome(t, "oasis")
	os.WriteFile(filepath.Join(dir, "oasis.yaml"), []byte(oasisSkin), 0o644)
	if code, _, _ := runSkinSet(t, ctx, "ui_tool", "#00FFFF"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	data := loadSkinFile(t, filepath.Join(dir, "oasis.yaml"))
	colors, _ := data["colors"].(map[string]any)
	if colors["ui_tool"] != "#00FFFF" || colors["background"] != "#08201f" || colors["banner_title"] != "#f2dfb3" {
		t.Fatalf("colors %v", colors)
	}
	if data["name"] != "oasis" {
		t.Fatalf("name %v", data["name"])
	}
	if _, err := os.Stat(filepath.Join(dir, "oasis-custom.yaml")); !os.IsNotExist(err) {
		t.Fatal("a user skin was forked")
	}
}

func TestSkinSetForksABuiltinWithoutInventingABackground(t *testing.T) {
	ctx, dir := skinCmdHome(t, "default")
	code, out, _ := runSkinSet(t, ctx, "ui_tool", "#00FFFF")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	fork := filepath.Join(dir, "default-custom.yaml")
	data := loadSkinFile(t, fork)
	colors, _ := data["colors"].(map[string]any)
	if colors["ui_tool"] != "#00FFFF" {
		t.Fatalf("colors %v", colors)
	}
	if _, ok := colors["background"]; ok {
		t.Fatal("fork invented a background")
	}
	if s, _ := colors["banner_title"].(string); s == "" {
		t.Fatal("full palette not carried over")
	}
	cfg, _ := os.ReadFile(filepath.Join(filepath.Dir(dir), "config.yaml"))
	if !strings.Contains(string(cfg), "default-custom") {
		t.Fatalf("fork not activated %q", cfg)
	}
	if got := MappingKeys(data); strings.Join(got[:5], ",") != "name,description,colors,branding,tool_prefix" {
		t.Fatalf("key order %v", got)
	}
	if data["description"] != "default + custom ui_tool" {
		t.Fatalf("description %v", data["description"])
	}
	if !strings.HasSuffix(out, "/skins/default-custom.yaml (live within ~1s)\n") || !strings.HasPrefix(out, "✓ ui_tool = #00FFFF in ") {
		t.Fatalf("stdout %q", out)
	}
}

func TestSkinSetRejectsNonHex(t *testing.T) {
	ctx, _ := skinCmdHome(t, "default")
	code, _, errOut := runSkinSet(t, ctx, "ui_tool", "teal")
	if code != 1 || errOut != "✗ 'teal' is not a #rrggbb hex color\n" {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
}

func TestSkinSetPersistsTheSkinDurably(t *testing.T) {
	ctx, dir := skinCmdHome(t, "oasis")
	path := filepath.Join(dir, "oasis.yaml")
	os.WriteFile(path, []byte(oasisSkin), 0o644)
	synced := 0
	prev := fsyncFile
	fsyncFile = func(f *os.File) error { synced++; return prev(f) }
	t.Cleanup(func() { fsyncFile = prev })
	if code, _, _ := runSkinSet(t, ctx, "ui_tool", "#00FFFF"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if synced == 0 {
		t.Fatal("skin file written without fsync, a crash can leave it empty")
	}
	colors, _ := loadSkinFile(t, path)["colors"].(map[string]any)
	if colors["ui_tool"] != "#00FFFF" || colors["background"] != "#08201f" || colors["banner_title"] != "#f2dfb3" {
		t.Fatalf("colors %v", colors)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("temp file left behind %s", e.Name())
		}
	}
}

func TestSkinSetPreservesASymlinkedSkinFile(t *testing.T) {
	ctx, dir := skinCmdHome(t, "oasis")
	realDir := filepath.Join(filepath.Dir(dir), "dotfiles")
	os.MkdirAll(realDir, 0o755)
	real := filepath.Join(realDir, "oasis.yaml")
	os.WriteFile(real, []byte("name: oasis\ncolors:\n  background: \"#08201f\"\n"), 0o644)
	link := filepath.Join(dir, "oasis.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if code, _, _ := runSkinSet(t, ctx, "ui_tool", "#00FFFF"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	colors, _ := loadSkinFile(t, real)["colors"].(map[string]any)
	if colors["ui_tool"] != "#00FFFF" || colors["background"] != "#08201f" {
		t.Fatalf("colors %v", colors)
	}
}

func TestSkinSetWritesTheFileAsSafeDumpDoes(t *testing.T) {
	ctx, dir := skinCmdHome(t, "oasis")
	path := filepath.Join(dir, "oasis.yaml")
	os.WriteFile(path, []byte("colors:\n  background: \"#08201f\"\nbranding:\n  agent_name: \"Ünïcode: yes\"\n"), 0o644)
	if code, _, _ := runSkinSet(t, ctx, "ui_tool", "#00ffff"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	raw, _ := os.ReadFile(path)
	want := "colors:\n  background: '#08201f'\n  ui_tool: '#00ffff'\nbranding:\n  agent_name: 'Ünïcode: yes'\nname: oasis\n"
	if string(raw) != want {
		t.Fatalf("written %q", raw)
	}
}

func TestSkinUseAndList(t *testing.T) {
	ctx, dir := skinCmdHome(t, "default")
	os.WriteFile(filepath.Join(dir, "oasis.yaml"), []byte("name: oasis\ndescription: Sea green\n"), 0o644)
	var out, errOut bytes.Buffer
	if code, err := SkinCommand(ctx, SkinArgs{Command: "use", Name: "oasis"}, testCommandSources(), &out, &errOut); err != nil || code != 0 {
		t.Fatalf("use %d %v", code, err)
	}
	if out.String() != "✓ active skin → oasis (live within ~1s)\n" {
		t.Fatalf("use stdout %q", out.String())
	}
	ResetCache()
	out.Reset()
	if code, err := SkinCommand(ctx, SkinArgs{}, testCommandSources(), &out, &errOut); err != nil || code != 0 {
		t.Fatalf("list %d %v", code, err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != len(BuiltinSkinNames())+1 {
		t.Fatalf("listed %q", out.String())
	}
	if lines[0] != "  default          builtin  "+PyStr(getOr(mustBuiltin(t, "default"), "description", "")) {
		t.Fatalf("builtin row %q", lines[0])
	}
	if last := lines[len(lines)-1]; last != "* oasis            user     Sea green" {
		t.Fatalf("user row %q", last)
	}
}

func mustBuiltin(t *testing.T, name string) map[string]any {
	t.Helper()
	skin, ok := builtinSkin(name)
	if !ok {
		t.Fatalf("no built in %s", name)
	}
	return skin
}
