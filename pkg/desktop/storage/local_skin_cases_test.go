package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func colorOf(skin *Object, key string) any {
	if skin == nil {
		return nil
	}
	colors, _ := skin.vals["colors"].(*Object)
	if colors == nil {
		return nil
	}
	return colors.vals[key]
}

func TestReadsAConfiguredCustomSkinWithoutALiveGateway(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.yaml"), "display:\n  skin: neon\n")
	writeFile(t, filepath.Join(home, "skins", "neon.yaml"), "name: neon\ndescription: neon after dark\ncolors:\n  background: \"#101020\"\n  ui_text: \"#eeeeee\"\n  ui_accent: \"#ff33aa\"\n")
	skin := ReadLocalDisplaySkin(home, "")
	if skin == nil || skin.vals["name"] != "neon" || skin.vals["description"] != "neon after dark" {
		t.Fatalf("skin %v", skin)
	}
	for k, v := range map[string]string{"background": "#101020", "ui_text": "#eeeeee", "ui_accent": "#ff33aa", "status_bar_bg": "#1a1a2e"} {
		if colorOf(skin, k) != v {
			t.Fatalf("%s is %v", k, colorOf(skin, k))
		}
	}
}

func TestReadsTheActiveNamedProfileInsteadOfTheRootHome(t *testing.T) {
	home := t.TempDir()
	profileHome := LocalSkinHome(home, "research")
	writeFile(t, filepath.Join(home, "config.yaml"), "display:\n  skin: root\n")
	writeFile(t, filepath.Join(profileHome, "config.yaml"), "display:\n  skin: profile\n")
	writeFile(t, filepath.Join(profileHome, "skins", "profile.yaml"), "name: profile\ncolors:\n  ui_accent: \"#00ffff\"\n")
	skin := ReadLocalDisplaySkin(home, "research")
	if skin == nil || skin.vals["name"] != "profile" || colorOf(skin, "ui_accent") != "#00ffff" || colorOf(skin, "status_bar_bg") != "#1a1a2e" {
		t.Fatalf("skin %v", skin)
	}
}

func TestUsesARoutedSessionOrHudProfileBeforeAnyGatewayCanConnect(t *testing.T) {
	home := t.TempDir()
	profileHome := LocalSkinHome(home, "research")
	writeFile(t, filepath.Join(profileHome, "config.yaml"), "display:\n  skin: research\n")
	writeFile(t, filepath.Join(profileHome, "skins", "research.yaml"), "name: research\ncolors:\n  ui_accent: \"#00ffff\"\n")
	routed := "research"
	payload := ReadLocalSkinPayload(home, &routed, "default")
	if payload == nil || payload.vals["profile"] != "research" {
		t.Fatalf("payload %v", payload)
	}
	skin := payload.vals["skin"].(*Object)
	if skin.vals["name"] != "research" || colorOf(skin, "ui_accent") != "#00ffff" {
		t.Fatalf("skin %v", skin)
	}
}

func TestReturnsBuiltInNamesEvenThoughTheyHaveNoLocalSkinFile(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.yaml"), "display: { skin: mono }\n")
	if got, _ := json.Marshal(ReadLocalDisplaySkin(home, "")); string(got) != `{"name":"mono"}` {
		t.Fatalf("got %s", got)
	}
}

func TestFailsClosedForMalformedConfigAndTraversalLikeSkinNames(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "config.yaml"), "display: [not, a, mapping]\n")
	if skin := ReadLocalDisplaySkin(home, ""); skin != nil {
		t.Fatalf("got %v", skin)
	}
	writeFile(t, filepath.Join(home, "config.yaml"), "display:\n  skin: ../../config\n")
	if skin := ReadLocalDisplaySkin(home, ""); skin != nil {
		t.Fatalf("got %v", skin)
	}
}

func TestLocalSkinYAMLFollowsTheCoreSchemaAndRefusesDuplicateKeysAndAliasFloods(t *testing.T) {
	v, err := parseYAMLJS("a: 0x10\nb: yes\nc: ~\n1.0: x\n", 20)
	if err != nil {
		t.Fatal(err)
	}
	if got := jsStringify(v, ""); got != `{"1":"x","a":16,"b":"yes","c":null}` {
		t.Fatalf("got %s", got)
	}
	if _, err := parseYAMLJS("a: 1\na: 2\n", 20); err == nil {
		t.Fatal("duplicate keys accepted")
	}
	flood := "a: &a [x, x, x, x, x]\nb: &b [*a, *a, *a, *a, *a]\nc: [*b, *b, *b, *b, *b]\n"
	if _, err := parseYAMLJS(flood, 20); err != errYAMLAliases {
		t.Fatalf("flood gave %v", err)
	}
	if _, err := parseYAMLJS("a: 1\n---\nb: 2\n", 20); err == nil {
		t.Fatal("multiple documents accepted")
	}
}
