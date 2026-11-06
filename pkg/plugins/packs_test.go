package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"gopkg.in/yaml.v3"
)

var (
	packSHAA = strings.Repeat("a", 40)
	packSHAB = strings.Repeat("b", 40)
)

// YAML text of the default test pack with top level overrides
func packYAML(t *testing.T, overrides map[string]any) string {
	doc := map[string]any{"name": "voice-pack", "description": "test pack", "author": "hyper", "version": "1.0.0",
		"plugins": []any{map[string]any{"repo": "owner/tts-plugin", "ref": packSHAA}, map[string]any{"name": "index-plugin", "ref": packSHAB}}}
	for k, v := range overrides {
		doc[k] = v
	}
	return dumpYAML(t, doc)
}

func dumpYAML(t *testing.T, doc any) string {
	raw, err := config.SafeDump(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mustParsePack(t *testing.T, text string) *PluginPack {
	pack, err := ParsePack(text, "")
	if err != nil {
		t.Fatal(err)
	}
	return pack
}

func packErrText(t *testing.T, err error) string {
	var pe *PackError
	if !errors.As(err, &pe) {
		t.Fatalf("want PackError, got %v", err)
	}
	return pe.Msg
}

func TestParsePackHappyPathAndNestedMeta(t *testing.T) {
	pack := mustParsePack(t, packYAML(t, nil))
	if pack.Name != "voice-pack" || pack.Author != "hyper" || len(pack.Plugins) != 2 || pack.Plugins[0].Repo != "owner/tts-plugin" || pack.Plugins[0].Ref != packSHAA || pack.Plugins[1].Name != "index-plugin" {
		t.Fatalf("pack %+v", pack)
	}
	nested := mustParsePack(t, dumpYAML(t, map[string]any{"pack": map[string]any{"name": "nested", "version": "2.0.0"},
		"plugins": []any{map[string]any{"source": "github:owner/repo", "version": packSHAA, "subdir": "sub/dir"}}}))
	if nested.Name != "nested" || nested.Version != "2.0.0" || nested.Plugins[0].Repo != "owner/repo" || nested.Plugins[0].Subdir != "sub/dir" || nested.Plugins[0].InstallIdentifier() != "owner/repo/sub/dir" {
		t.Fatalf("nested %+v", nested)
	}
	if got := mustParsePack(t, dumpYAML(t, map[string]any{"name": "p", "plugins": []any{map[string]any{"repo": "o/r", "ref": strings.Repeat("A", 40)}}})).Plugins[0].Ref; got != packSHAA {
		t.Fatalf("ref %q", got)
	}
}

func TestParsePackRejectsNonSHARefsNamingTheEntry(t *testing.T) {
	for _, bad := range []any{"main", "v1.2.0", strings.Repeat("a", 39), strings.Repeat("a", 41), "", nil} {
		_, err := ParsePack(dumpYAML(t, map[string]any{"name": "p", "plugins": []any{map[string]any{"repo": "owner/repo", "ref": bad}}}), "")
		if msg := packErrText(t, err); !strings.Contains(msg, "40-character") || !strings.Contains(msg, "owner/repo") {
			t.Fatalf("%v: %s", bad, msg)
		}
	}
}

func TestParsePackRejectsMalformedDocuments(t *testing.T) {
	for _, c := range [][2]string{{"[]", "mapping"}, {"name: p", "plugins"}, {"name: p\nplugins: []", "plugins"},
		{"plugins:\n  - repo: o/r\n    ref: " + packSHAA, "name"}, {"name: p\nplugins:\n  - ref: " + packSHAA, "either 'name'"},
		{"name: p\nplugins:\n  - not-a-mapping", "mapping"}, {"{", "YAML"}} {
		_, err := ParsePack(c[0], "")
		if msg := packErrText(t, err); !strings.Contains(msg, c[1]) {
			t.Fatalf("%q: %s", c[0], msg)
		}
	}
}

func TestConfigSeedRejectsForbiddenKeys(t *testing.T) {
	for _, key := range []string{"api_key", "MY_TOKEN", "password", "auth_header", "private_key"} {
		_, err := ValidateConfigSeed("p", map[string]any{key: "x"})
		if !strings.Contains(strings.ToLower(packErrText(t, err)), "secret") {
			t.Fatal(key)
		}
	}
	for _, key := range []string{"granted_capabilities", "capabilities_consent", "allow_tool_override"} {
		_, err := ValidateConfigSeed("p", map[string]any{key: true})
		if !strings.Contains(packErrText(t, err), "reserved") {
			t.Fatal(key)
		}
	}
	for _, c := range []struct {
		seed     map[string]any
		fragment string
	}{
		{map[string]any{"settings": map[string]any{"api_key": "LEAK"}}, "secret-shaped key 'settings.api_key'"},
		{map[string]any{"security": map[string]any{"granted_capabilities": []any{"tools"}}}, "reserved key 'security.granted_capabilities'"},
		{map[string]any{"profiles": []any{map[string]any{"allow_tool_override": true}}}, "reserved key 'profiles.allow_tool_override'"},
	} {
		_, err := ValidateConfigSeed("p", c.seed)
		if !strings.Contains(packErrText(t, err), c.fragment) {
			t.Fatal(c.fragment)
		}
	}
	ok := map[string]any{"settings": map[string]any{"voice": "nova", "tags": []any{"a"}}}
	if got, err := ValidateConfigSeed("p", ok); err != nil || !reflect.DeepEqual(got, ok) {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParsePack(packYAML(t, map[string]any{"config": map[string]any{"tts-plugin": map[string]any{"granted_capabilities": []any{"tools"}}}}), ""); err == nil {
		t.Fatal("reserved config seed accepted")
	}
	if got := mustParsePack(t, packYAML(t, map[string]any{"config": map[string]any{"tts-plugin": map[string]any{"voice": "nova"}}})).seedFor("tts-plugin"); !reflect.DeepEqual(got, map[string]any{"voice": "nova"}) {
		t.Fatalf("seed %v", got)
	}
	_, err := ParsePack("name: p\nplugins:\n  - {repo: o/r, ref: "+packSHAA+"}\nconfig:\n  tts:\n    1: x\n", "")
	if msg := packErrText(t, err); msg != "Pack config for plugin 'tts' has an invalid key: 1." {
		t.Fatalf("int key %q", msg)
	}
	_, err = ParsePack("name: p\nplugins:\n  - {repo: o/r, ref: "+packSHAA+"}\nconfig:\n  yes:\n    nested:\n      true: {api_key: x}\n", "")
	if msg := packErrText(t, err); !strings.Contains(msg, "plugin 'True' sets secret-shaped key 'nested.True.api_key'") {
		t.Fatalf("bool keys %q", msg)
	}
	_, err = ParsePack("name: p\nplugins:\n  - {repo: o/r, ref: "+packSHAA+"}\nconfig:\n  tts:\n    2001-12-14: x\n", "")
	if msg := packErrText(t, err); msg != "Pack config for plugin 'tts' has an invalid key: datetime.date(2001, 12, 14)." {
		t.Fatalf("date key %q", msg)
	}
	_, err = ParsePack("name: p\nplugins:\n  - {repo: o/r, ref: "+packSHAA+"}\nconfig:\n  tts:\n    2001-12-14t21:59:43.10-05:00: x\n", "")
	if msg := packErrText(t, err); msg != "Pack config for plugin 'tts' has an invalid key: datetime.datetime(2001, 12, 14, 21, 59, 43, 100000, tzinfo=datetime.timezone(datetime.timedelta(days=-1, seconds=68400), '-05:00'))." {
		t.Fatalf("datetime key %q", msg)
	}
	_, err = ParsePack("name: p\nplugins:\n  - {repo: o/r, ref: "+packSHAA+"}\nconfig:\n  2002-12-14:\n    nested:\n      2001-12-14 21:59:43.10 -5: {api_key: x}\n", "")
	if msg := packErrText(t, err); !strings.Contains(msg, "plugin '2002-12-14' sets secret-shaped key 'nested.2001-12-14 21:59:43.100000-05:00.api_key'") {
		t.Fatalf("timestamp key paths %q", msg)
	}
	_, err = ParsePack("name: p\nplugins:\n  - {repo: o/r, ref: "+packSHAA+"}\nconfig:\n  tts:\n    2001-13-01: x\n", "")
	if e, ok := err.(*pydatetime.Error); !ok || e.Kind != "ValueError" || e.Msg != "month must be in 1..12" {
		t.Fatalf("invalid timestamp key = %#v", err)
	}
	if got := mustParsePack(t, "name: p\nplugins:\n  - {repo: o/r, ref: "+packSHAA+"}\nconfig:\n  tts:\n    '2001-12-14': x\n").seedFor("tts"); !reflect.DeepEqual(got, map[string]any{"2001-12-14": "x"}) {
		t.Fatalf("quoted timestamp key %v", got)
	}
	for text, want := range map[string]any{"2001-1-2": "2001-1-2", "2001-12-14": pydatetime.Date{Year: 2001, Month: 12, Day: 14}, "'2001-12-14'": "2001-12-14"} {
		var doc yaml.Node
		if err := yaml.Unmarshal([]byte("{"+text+": 1}"), &doc); err != nil {
			t.Fatal(err)
		}
		if got, ok := yaml11Key(doc.Content[0].Content[0]); !ok || !reflect.DeepEqual(got, want) {
			t.Fatalf("yaml11Key(%s) = %#v %v", text, got, ok)
		}
	}
}

func TestSanitizedEntryConfigStripsForbiddenKeysAtAnyDepth(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  entries:\n    tts:\n      smtp: {host: mail.example, password: hunter2}\n      rules: [{name: r1, auth_token: t}]\n      voice: nova\n")
	want := map[string]any{"smtp": map[string]any{"host": "mail.example"}, "rules": []any{map[string]any{"name": "r1"}}, "voice": "nova"}
	if got := sanitizedEntryConfig(h, "tts"); !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestLoadPackSources(t *testing.T) {
	ctx := context.Background()
	if _, err := LoadPack(ctx, "http://example.com/pack.yaml"); !strings.Contains(packErrText(t, err), "https") {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "icebath-pack.yaml")
	writeFile(t, f, packYAML(t, nil))
	if pack, err := LoadPack(ctx, f); err != nil || pack.Name != "voice-pack" {
		t.Fatalf("%v %v", pack, err)
	}
	if _, err := LoadPack(ctx, "/nonexistent/pack.yaml"); !strings.Contains(packErrText(t, err), "not found") {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, packYAML(t, nil))
	}))
	defer srv.Close()
	saved := packHTTPClient
	packHTTPClient = srv.Client
	t.Cleanup(func() { packHTTPClient = saved })
	if pack, err := LoadPack(ctx, srv.URL+"/pack.yaml"); err != nil || pack.Name != "voice-pack" {
		t.Fatalf("%v %v", pack, err)
	}
	if _, err := LoadPack(ctx, srv.URL+"/missing"); !strings.HasPrefix(packErrText(t, err), "Could not fetch pack from "+srv.URL+"/missing: Client error '404 Not Found'") {
		t.Fatal(err)
	}
}

func fakeCatalog(t *testing.T, entries ...*PluginCatalogEntry) {
	saved := packCatalog
	packCatalog = func(context.Context) []*PluginCatalogEntry { return entries }
	t.Cleanup(func() { packCatalog = saved })
}

func TestResolvePackPlugins(t *testing.T) {
	pack := mustParsePack(t, packYAML(t, nil))
	fakeCatalog(t, &PluginCatalogEntry{Name: "index-plugin", Repo: "https://github.com/idx-owner/idx-repo", Sha: packSHAB, Description: "d", Maintainer: "idx-owner",
		Capabilities: CatalogCapabilities{ProvidesTools: []string{"tools"}}})
	resolved := ResolvePackPlugins(context.Background(), pack)
	if resolved[0].Identifier != "owner/tts-plugin" || resolved[1].Identifier != "https://github.com/idx-owner/idx-repo" || !reflect.DeepEqual(resolved[1].IndexCapabilities, []string{"tools"}) {
		t.Fatalf("%+v", resolved)
	}
	fakeCatalog(t)
	ghost := mustParsePack(t, dumpYAML(t, map[string]any{"name": "p", "plugins": []any{map[string]any{"name": "ghost", "ref": packSHAA}}}))
	if r := ResolvePackPlugins(context.Background(), ghost)[0]; r.Identifier != "" || !strings.Contains(r.ResolveError, "not found") {
		t.Fatalf("%+v", r)
	}
}

func resolvedAsIs(pack *PluginPack) []ResolvedPackPlugin {
	var out []ResolvedPackPlugin
	for _, e := range pack.Plugins {
		out = append(out, ResolvedPackPlugin{Entry: e, Identifier: e.InstallIdentifier()})
	}
	return out
}

type packInstallCall struct {
	identifier, ref string
	force           bool
}

// Replaces the fan out collaborators, recording installs and consent calls
func fanoutSeams(t *testing.T, install func(identifier string) (map[string]any, string, error)) (*[]packInstallCall, *[][]any) {
	var calls []packInstallCall
	var consents [][]any
	saved := []any{packInstallCore, packPromptEnv, packSetEnabled, packConsent, packDeclaredCaps}
	packInstallCore = func(ctx context.Context, identifier string, opts installCoreOptions) (string, map[string]any, string, error) {
		calls = append(calls, packInstallCall{identifier, opts.Ref, opts.Force})
		manifest, name, err := install(identifier)
		return filepath.Join(t.TempDir(), name), manifest, name, err
	}
	packPromptEnv = func(context.Context, string, map[string]any, *Console) error { return nil }
	packSetEnabled = func(context.Context, string, string, bool, []string, *Console) error { return nil }
	packConsent = func(ctx context.Context, console *Console, name string, declared []string, c string) error {
		consents = append(consents, []any{name, declared})
		return nil
	}
	packDeclaredCaps = func(manifest map[string]any, name string) []string {
		caps, _ := manifest["capabilities"].([]string)
		return caps
	}
	t.Cleanup(func() {
		packInstallCore = saved[0].(func(context.Context, string, installCoreOptions) (string, map[string]any, string, error))
		packPromptEnv = saved[1].(func(context.Context, string, map[string]any, *Console) error)
		packSetEnabled = saved[2].(func(context.Context, string, string, bool, []string, *Console) error)
		packConsent = saved[3].(func(context.Context, *Console, string, []string, string) error)
		packDeclaredCaps = saved[4].(func(map[string]any, string) []string)
	})
	return &calls, &consents
}

func twoRepoPack(t *testing.T, a, b string) *PluginPack {
	return mustParsePack(t, dumpYAML(t, map[string]any{"name": "p", "plugins": []any{map[string]any{"repo": a, "ref": packSHAA}, map[string]any{"repo": b, "ref": packSHAB}}}))
}

func TestInstallFanOut(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ctx := context.Background()
	caps := map[string][]string{"o/a": {"tools"}, "o/b": {"platform"}}
	calls, consents := fanoutSeams(t, func(id string) (map[string]any, string, error) {
		name := strings.TrimPrefix(id, "o/")
		return map[string]any{"name": name, "capabilities": caps[id]}, name, nil
	})
	pack := twoRepoPack(t, "o/a", "o/b")
	results, err := InstallPackPlugins(ctx, pack, resolvedAsIs(pack), NewConsole(&bytes.Buffer{}), false)
	if err != nil || !results[0].OK || !results[1].OK {
		t.Fatalf("%+v %v", results, err)
	}
	if want := []packInstallCall{{"o/a", packSHAA, false}, {"o/b", packSHAB, false}}; !reflect.DeepEqual(*calls, want) {
		t.Fatalf("calls %+v", *calls)
	}
	if want := [][]any{{"a", []string{"tools"}}, {"b", []string{"platform"}}}; !reflect.DeepEqual(*consents, want) {
		t.Fatalf("consents %v", *consents)
	}
	fanoutSeams(t, func(id string) (map[string]any, string, error) {
		if strings.Contains(id, "bad") {
			return nil, "", &PluginOperationError{Msg: "clone exploded"}
		}
		return map[string]any{"name": "good"}, "good", nil
	})
	bad := twoRepoPack(t, "o/bad", "o/good")
	results, err = InstallPackPlugins(ctx, bad, resolvedAsIs(bad), NewConsole(&bytes.Buffer{}), false)
	if err != nil || results[0].OK || !results[1].OK || !strings.Contains(results[0].Error, "clone exploded") || results[1].InstalledName != "good" {
		t.Fatalf("%+v %v", results, err)
	}
}

// Writes a pack file and sets the terminal and the answer the consent question reads
func packInstallWorld(t *testing.T, text string, tty bool, answer string) string {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	f := filepath.Join(t.TempDir(), "pack.yaml")
	writeFile(t, f, text)
	savedTTY, savedInput := isTTY, inputLine
	isTTY = func() bool { return tty }
	inputLine = func(string) (string, error) { return answer, nil }
	t.Cleanup(func() { isTTY, inputLine = savedTTY, savedInput })
	return f
}

func TestPackInstallCommand(t *testing.T) {
	ctx := context.Background()
	f := packInstallWorld(t, dumpYAML(t, map[string]any{"name": "p", "plugins": []any{map[string]any{"repo": "o/bad", "ref": packSHAA}, map[string]any{"repo": "o/good", "ref": packSHAB}}}), true, "y")
	fanoutSeams(t, func(id string) (map[string]any, string, error) {
		if strings.Contains(id, "bad") {
			return nil, "", &PluginOperationError{Msg: "boom"}
		}
		return map[string]any{"name": "good"}, "good", nil
	})
	var out bytes.Buffer
	var exit *CommandExit
	if err := CmdPackInstall(ctx, f, false, &out); !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(out.String(), "Pack 'p': 1 installed, 1 failed.") {
		t.Fatalf("%v\n%s", err, out.String())
	}
	savedResolve := packResolve
	packResolve = func(_ context.Context, pack *PluginPack) []ResolvedPackPlugin { return resolvedAsIs(pack) }
	t.Cleanup(func() { packResolve = savedResolve })
	for _, c := range []struct {
		tty    bool
		answer string
		text   string
	}{{false, "", "interactive"}, {true, "n", "Aborted — nothing installed."}} {
		f := packInstallWorld(t, packYAML(t, nil), c.tty, c.answer)
		calls, _ := fanoutSeams(t, func(string) (map[string]any, string, error) { return nil, "", nil })
		out.Reset()
		if err := CmdPackInstall(ctx, f, false, &out); !errors.As(err, &exit) || exit.Code != 1 || len(*calls) != 0 || !strings.Contains(out.String(), c.text) {
			t.Fatalf("%v %v\n%s", err, *calls, out.String())
		}
	}
}

func TestPackReviewSeedAndDispatch(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	ctx := context.Background()
	writeFile(t, filepath.Join(h, "config.yaml"), "plugins:\n  entries:\n    tts:\n      voice: alloy\n")
	f := filepath.Join(t.TempDir(), "pack.yaml")
	writeFile(t, f, "name: p\nversion: '2'\nauthor: me\nplugins:\n  - {repo: o/tts, ref: "+packSHAA+"}\n  - {name: ghost, ref: "+packSHAB+"}\nconfig:\n  tts: {voice: nova, speed: 2}\nskills: [s1]\n")
	fakeCatalog(t)
	var out bytes.Buffer
	if err := PackCommand(ctx, PackArgs{Action: "show", Source: f}, &out, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"p v2 — by me", "unresolved: not found in", "aaaaaaaaaaaa", "tts.voice = 'nova'", "tts.speed = 2", "Pack lists skills (NOT auto-installed yet): s1",
		"third-party code × 2 plugins", "1 entry could not be resolved", "Dry run only. Install with `icebath plugins pack install ...`."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("%q missing from\n%s", want, out.String())
		}
	}
	out.Reset()
	if err := seedPluginConfig(h, "tts", map[string]any{"voice": "nova", "speed": 2}, NewConsole(&out)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(h, "config.yaml"))
	if !strings.Contains(string(raw), "voice: alloy") || !strings.Contains(string(raw), "speed: 2") || !strings.Contains(out.String(), "plugins.entries.tts.voice already set — keeping your value.") || !strings.Contains(out.String(), "Seeded plugins.entries.tts from pack.") {
		t.Fatalf("%s\n%s", raw, out.String())
	}
	out.Reset()
	var exit *CommandExit
	if err := PackCommand(ctx, PackArgs{}, &out, &out); !errors.As(err, &exit) || !strings.Contains(out.String(), "Usage: icebath plugins pack {install|export|show}") {
		t.Fatalf("%v %s", err, out.String())
	}
}

// Plugins dir, install metadata and config of an export test home
func exportWorld(t *testing.T, metadata, cfg string, plugins ...string) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	for _, name := range plugins {
		if err := os.MkdirAll(filepath.Join(h, "plugins", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(h, "plugins", ".install-metadata.json"), metadata)
	writeFile(t, filepath.Join(h, "config.yaml"), cfg)
}

func TestExportPack(t *testing.T) {
	ctx := context.Background()
	exportWorld(t, `{"tts": {"pinned": true, "revision": "`+packSHAA+`", "source": "https://github.com/owner/tts-plugin.git"}, "relay": {"pinned": true, "revision": "`+packSHAB+`", "source": "https://github.com/owner/mono.git#plugins/relay"}}`,
		"plugins:\n  enabled: [tts, relay]\n  entries:\n    tts: {voice: nova, api_key: sk-super-secret, granted_capabilities: [tools], allow_tool_override: true}\n", "tts", "relay")
	text, warnings, err := ExportPack(ctx, false, "exported")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("%v %v", warnings, err)
	}
	for _, leaked := range []string{"sk-super-secret", "api_key", "granted_capabilities", "allow_tool_override"} {
		if strings.Contains(text, leaked) {
			t.Fatalf("%s leaked\n%s", leaked, text)
		}
	}
	pack := mustParsePack(t, text)
	byRepo := map[string]PackPluginEntry{}
	for _, p := range pack.Plugins {
		byRepo[p.Repo] = p
	}
	if pack.Name != "exported" || byRepo["owner/tts-plugin"].Ref != packSHAA || byRepo["owner/mono"].Subdir != "plugins/relay" || byRepo["owner/mono"].Ref != packSHAB || !reflect.DeepEqual(pack.seedFor("tts"), map[string]any{"voice": "nova"}) || !strings.Contains(text, "voice: nova") {
		t.Fatalf("%s", text)
	}
	exportWorld(t, `{}`, "{}\n", "local-hack")
	text, warnings, _ = ExportPack(ctx, false, defaultPackName)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "local-hack") || !strings.Contains(text, "# WARNING") {
		t.Fatalf("%v\n%s", warnings, text)
	}
	if _, err := ParsePack(text, ""); err == nil {
		t.Fatal("local only export parsed")
	}
	exportWorld(t, `{"on": {"pinned": true, "revision": "`+packSHAA+`", "source": "https://github.com/o/on.git"}, "off": {"pinned": true, "revision": "`+packSHAB+`", "source": "https://github.com/o/off.git"}}`,
		"plugins:\n  enabled: ['on']\n", "on", "off")
	var out, errOut bytes.Buffer
	if err := PackCommand(ctx, PackArgs{Action: "export", EnabledOnly: true}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if pack := mustParsePack(t, out.String()); len(pack.Plugins) != 1 || pack.Plugins[0].Repo != "o/on" || pack.Name != defaultPackName {
		t.Fatalf("%s", out.String())
	}
}
