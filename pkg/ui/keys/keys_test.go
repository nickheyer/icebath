package keys

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestParseCanonicalizesChords(t *testing.T) {
	cases := map[string]string{"Alt+Shift+L": "alt+shift+l", "alt+L": "alt+shift+l", "G": "shift+g", "shift+g": "shift+g", "ctrl+pgdn": "ctrl+pgdown", "alt++": "alt++", "alt+shift+=": "alt++", "+": "+", "Escape": "esc", "alt+space": "alt+space", "Control+Return": "ctrl+enter", "alt+_": "alt+_", "alt+shift+-": "alt+_", "shift+tab": "shift+tab", "F5": "f5", "/": "/", "super+q": "super+q"}
	for in, want := range cases {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Fatalf("%q gave %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "alt+", "ctrl+nope", "mods+a", "shift+ ", "alt+ab"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}

func TestChordsCoverKittyAndLegacySpellings(t *testing.T) {
	has := func(chords []string, want string) bool {
		for _, c := range chords {
			if c == want {
				return true
			}
		}
		return false
	}
	legacy := Chords(tea.KeyPressMsg{Code: 'L', Mod: tea.ModAlt})
	kitty := Chords(tea.KeyPressMsg{Code: 'l', Mod: tea.ModAlt | tea.ModShift, ShiftedCode: 'L'})
	if !has(legacy, "alt+shift+l") || !has(kitty, "alt+shift+l") {
		t.Fatalf("alt+shift+l legacy %v kitty %v", legacy, kitty)
	}
	if c := Chords(tea.KeyPressMsg{Code: 'G', Text: "G"}); !has(c, "shift+g") {
		t.Fatalf("legacy G %v", c)
	}
	if c := Chords(tea.KeyPressMsg{Code: 'g', Mod: tea.ModShift, ShiftedCode: 'G', Text: "G"}); !has(c, "shift+g") {
		t.Fatalf("kitty shift+g %v", c)
	}
	if c := Chords(tea.KeyPressMsg{Code: '=', Mod: tea.ModAlt | tea.ModShift, ShiftedCode: '+'}); !has(c, "alt++") {
		t.Fatalf("kitty alt+shift+= %v", c)
	}
	if c := Chords(tea.KeyPressMsg{Code: '+', Mod: tea.ModAlt}); !has(c, "alt++") {
		t.Fatalf("legacy alt++ %v", c)
	}
	if c := Chords(tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModAlt}); !has(c, "alt+space") {
		t.Fatalf("alt+space %v", c)
	}
	if c := Chords(tea.KeyPressMsg{Code: tea.KeyKpEnter}); !has(c, "enter") {
		t.Fatalf("keypad enter %v", c)
	}
	if c := Chords(tea.KeyPressMsg{Code: 'q', Mod: tea.ModAlt | tea.ModCapsLock, BaseCode: 'q'}); !has(c, "alt+q") {
		t.Fatalf("locks are ignored %v", c)
	}
	if c := Chords(tea.KeyPressMsg{Code: 'é', Mod: tea.ModAlt, BaseCode: 'w'}); !has(c, "alt+w") || !has(c, "alt+é") {
		t.Fatalf("base layout key %v", c)
	}
	if Chords(tea.KeyPressMsg{Code: tea.KeyExtended, Text: "ab"}) != nil {
		t.Fatal("multi rune text binds nothing")
	}
	if _, ok := Captured(tea.KeyPressMsg{Code: tea.KeyLeftShift}); ok {
		t.Fatal("a modifier alone is not captured")
	}
	if c, ok := Captured(tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}); !ok || c != "ctrl+x" {
		t.Fatalf("capture %q", c)
	}
}

func TestPressRoundTripsEveryChord(t *testing.T) {
	m := Default()
	for _, a := range m.Actions() {
		for _, c := range m.Keys(a.ID) {
			k := Press(c)
			if got, ok := Captured(k); !ok || got != c {
				t.Fatalf("%s: %q pressed captures as %q", a.ID, c, got)
			}
			if !slices.Contains(Chords(k), c) {
				t.Fatalf("%s: %q pressed reads as %v", a.ID, c, Chords(k))
			}
		}
	}
	if k := Press("shift+g"); k.Text != "G" || k.String() != "G" {
		t.Fatalf("a shifted letter types its capital %+v", k)
	}
	if k := Press("ctrl+a"); k.Text != "" || k.String() != "ctrl+a" {
		t.Fatalf("a control chord types nothing %+v", k)
	}
	if k := Press("space"); k.Text != " " || k.String() != "space" {
		t.Fatalf("space %+v", k)
	}
	if k := Press("nope+x"); k.Code != 0 {
		t.Fatal("a bad chord presses nothing")
	}
}

func TestSymbolsOfNavigationKeys(t *testing.T) {
	if s, ok := Symbol("pgup"); !ok || s != "⇞" {
		t.Fatal("page up has its key symbol")
	}
	if s, ok := Symbol("pgdown"); !ok || s != "⇟" {
		t.Fatal("page down has its key symbol")
	}
	if _, ok := Symbol("ctrl+u"); ok {
		t.Fatal("a chord with modifiers has no symbol")
	}
}

func TestFormatAndPairSpellTypedNames(t *testing.T) {
	for chord, want := range map[string]string{"shift+g": "G", "alt+shift+l": "alt+shift+l", "down": "down", "pgdown": "pgdown", "pgdn": "pgdown", "backspace": "backspace", "ctrl+Return": "ctrl+enter", "": ""} {
		if got := Format(chord); got != want {
			t.Fatalf("%q formats as %q, want %q", chord, got, want)
		}
		if want != "" {
			if back, err := Parse(Format(chord)); err != nil || Format(back) != want {
				t.Fatalf("%q does not read back %q %v", want, back, err)
			}
		}
	}
	if Pair("j", "k") != "j/k" || Pair("", "k") != "k" || Pair("j", "") != "j" || Pair("down", "up") != "up/down" || Pair("right", "left") != "left/right" || Pair("shift+down", "shift+up") != "shift+up/shift+down" || Pair("alt+down", "up") != "alt+down/up" || Pair("alt++", "alt+_") != "alt++/alt+_" {
		t.Fatal("pair")
	}
}

func TestMapBindsLooksUpAndGuardsConflicts(t *testing.T) {
	m := Default()
	if m.Display("palette") != "alt+space" || m.First("layout.dwindle") != "" {
		t.Fatal("defaults")
	}
	ids := m.Lookup([]string{Sessions, List, Panel}, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if len(ids) != 1 || ids[0] != "list.down" {
		t.Fatalf("lookup %v", ids)
	}
	ids = m.Lookup([]string{Session}, tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModAlt})
	if len(ids) != 1 || ids[0] != "palette" {
		t.Fatalf("global after the card %v", ids)
	}
	ids = m.Lookup(nil, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if len(ids) != 0 {
		t.Fatalf("plain keys bind nothing globally %v", ids)
	}
	if err := m.Rebind("workspace.new", "alt+space"); err == nil || !strings.Contains(err.Error(), "palette") {
		t.Fatalf("conflict %v", err)
	}
	if err := m.Rebind("sessions.fork", "j"); err != nil {
		t.Fatalf("a chord of another context is free %v", err)
	}
	ids = m.Lookup([]string{Sessions, List, Panel}, tea.KeyPressMsg{Code: 'j', Text: "j"})
	if len(ids) != 2 || ids[0] != "sessions.fork" || ids[1] != "list.down" {
		t.Fatalf("the card context wins %v", ids)
	}
	if err := m.Add("sessions.fork", "F"); err != nil || m.DisplayAll("sessions.fork") != "j F" {
		t.Fatalf("add %v %q", err, m.DisplayAll("sessions.fork"))
	}
	if m.AtDefault("sessions.fork") {
		t.Fatal("changed")
	}
	if err := m.Reset("sessions.fork"); err != nil || !m.AtDefault("sessions.fork") || m.Display("sessions.fork") != "f" {
		t.Fatal("reset")
	}
	if err := m.Unbind("sessions.fork"); err != nil || m.First("sessions.fork") != "" {
		t.Fatal("unbind")
	}
	if err := m.Rebind("nope", "x"); err == nil {
		t.Fatal("unknown action")
	}
	if err := m.Reset("nope"); err == nil {
		t.Fatal("unknown reset")
	}
	if err := m.Rebind("sessions.fork", "ctrl+"); err == nil {
		t.Fatal("bad chord")
	}
	if m.Holder("sessions.resume", "enter") != "" || m.Holder("sessions.resume", "d") != "sessions.delete" {
		t.Fatal("holder")
	}
	m.Register(Action{ID: "custom", Label: "custom", Doc: "fires a test", Default: []string{"alt+c"}})
	if a, ok := m.Action("custom"); !ok || a.Context != Global || m.Display("custom") != "alt+c" {
		t.Fatal("register")
	}
	if n := len(m.InContexts([]string{Sessions})); n == 0 || m.InContexts([]string{Sessions})[0].ID != "sessions.resume" {
		t.Fatal("in contexts")
	}
	if len(m.Contexts()) == 0 || m.Contexts()[0] != Global {
		t.Fatal("contexts")
	}
	for _, a := range m.Actions() {
		if ContextDocs[a.Context] == "" {
			t.Fatalf("%s has an undocumented context %q", a.ID, a.Context)
		}
		if a.Label == "" || a.Doc == "" {
			t.Fatalf("%s lacks a label or doc", a.ID)
		}
	}
}

func TestFileRoundTripsAndReportsBadEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "k", FileName)
	m, err := Open(path)
	if err != nil || m.Path() != path {
		t.Fatalf("open %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"palette": ["alt+space"]`) || !strings.HasPrefix(string(data), "{\n  \"global\": {") {
		t.Fatalf("defaults written\n%s", data)
	}
	if err := m.Rebind("workspace.new", "alt+shift+n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Unbind("window.zoom"); err != nil {
		t.Fatal(err)
	}
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path)
	if err != nil || again.Display("workspace.new") != "alt+shift+n" || again.First("window.zoom") != "" {
		t.Fatalf("reload %v %q %q", err, again.Display("workspace.new"), again.First("window.zoom"))
	}
	os.WriteFile(path, []byte(`{"global": {"palette": "alt+p", "nope": "x", "workspace.new": ["ctrl+"]}, "weird": {}, "list": {"palette": "y"}}`), 0o644)
	bad, err := Open(path)
	if err == nil || bad.Display("palette") != "alt+p" {
		t.Fatalf("partial load %v %q", err, bad.Display("palette"))
	}
	for _, want := range []string{"unknown action \"nope\"", "unknown context \"weird\"", "belongs to global", "workspace.new"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error lacks %q: %v", want, err)
		}
	}
	os.WriteFile(path, []byte(`{"global": {"palette": null}}`), 0o644)
	if m, err := Open(path); err != nil || m.First("palette") != "" {
		t.Fatalf("null unbinds %v", err)
	}
	os.WriteFile(path, []byte(`{"global": {"palette": 5}}`), 0o644)
	if _, err := Open(path); err == nil {
		t.Fatal("a number is not a chord")
	}
	os.WriteFile(path, []byte(`not json`), 0o644)
	if _, err := Open(path); err == nil {
		t.Fatal("bad json")
	}
	os.WriteFile(path, []byte(`{"global": 5}`), 0o644)
	if _, err := Open(path); err == nil {
		t.Fatal("bad context body")
	}
	if m, err := Open(""); err != nil || m.Save() != nil {
		t.Fatal("no path")
	}
	if _, err := Open(filepath.Join(dir, "k")); err == nil {
		t.Fatal("a directory is not a file")
	}
}

func TestFileFromAnOlderRegistryIsWrittenAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	os.WriteFile(path, []byte(`{"global": {"open.gone": ["alt+o"], "palette": "alt+p"}, "config.gone": {"x": "y"}}`), 0o644)
	m, err := Open(path)
	if err == nil || !strings.Contains(err.Error(), "unknown action \"open.gone\"") || !strings.Contains(err.Error(), "written again without them") {
		t.Fatalf("names gone from the registry are reported %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "gone") || !strings.Contains(string(data), `"palette": ["alt+p"]`) || !strings.Contains(string(data), `"detail.scroll.down"`) || m.Stale() {
		t.Fatalf("the file is written again with the binds kept and every current action listed\n%s", data)
	}
	if m, err := Open(path); err != nil || m.First("palette") != "alt+p" {
		t.Fatalf("the rewritten file reads clean %v", err)
	}
	os.WriteFile(path, []byte(`{"global": {"palette": "alt+q"}}`), 0o644)
	if m, err := Open(path); err != nil || m.First("palette") != "alt+q" {
		t.Fatalf("a file missing actions fills them in %v", err)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"window.zoom": ["alt+f"]`) {
		t.Fatal("missing actions are written with their defaults")
	}
	bad := `{"global": {"open.gone": "x", "palette": "ctrl+"}}`
	os.WriteFile(path, []byte(bad), 0o644)
	if _, err := Open(path); err == nil {
		t.Fatal("a bad chord is reported")
	}
	if data, _ := os.ReadFile(path); string(data) != bad {
		t.Fatal("a file with a mistake the registry cannot fix is left for the person")
	}
}

func TestFileSwapsKeysAndReloadsOutsideEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	m, err := Open(path)
	if err != nil || m.Stale() {
		t.Fatalf("fresh file %v %v", err, m.Stale())
	}
	os.WriteFile(path, []byte(`{"sessions": {"sessions.fork": "d", "sessions.delete": "f"}}`), 0o644)
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	if !m.Stale() {
		t.Fatal("an outside edit reads as stale")
	}
	if err := m.Reload(); err != nil || m.First("sessions.fork") != "d" || m.First("sessions.delete") != "f" || m.Stale() {
		t.Fatalf("traded keys bind as one swap %v %q %q", err, m.First("sessions.fork"), m.First("sessions.delete"))
	}
	if m.First("palette") != "alt+space" {
		t.Fatal("actions the file leaves out keep their defaults")
	}
	os.WriteFile(path, []byte(`{"sessions": {"sessions.fork": "w", "sessions.delete": "w"}}`), 0o644)
	err = m.Reload()
	if err == nil || !strings.Contains(err.Error(), "sessions.delete") || m.First("sessions.fork") != "w" || m.First("sessions.delete") != "d" {
		t.Fatalf("a clash keeps the later action at its default %v %q %q", err, m.First("sessions.fork"), m.First("sessions.delete"))
	}
	if err := m.Rebind("sessions.fork", "F"); err != nil || m.Save() != nil || m.Stale() {
		t.Fatal("a save of our own is not stale")
	}
	os.WriteFile(path, []byte(`{"sessions": {"sessions.fork": ["ctrl+", "g"]}}`), 0o644)
	if err := m.Reload(); err == nil || m.First("sessions.fork") != "f" {
		t.Fatalf("a chord that does not parse leaves the action at its default %v %q", err, m.First("sessions.fork"))
	}
	m.Rebind("sessions.fork", "F")
	os.WriteFile(path, []byte(`{"sessions": {`), 0o644)
	if err := m.Reload(); err == nil || m.First("sessions.fork") != "shift+f" {
		t.Fatalf("a file that does not parse keeps the keys in force %v %q", err, m.First("sessions.fork"))
	}
	os.Remove(path)
	if m.Stale() || m.Reload() == nil {
		t.Fatal("a missing file is not stale and does not reload")
	}
	if (&Map{}).Stale() {
		t.Fatal("a map without a file is never stale")
	}
}

func TestNoContextHandsOneDefaultKeyToTwoActions(t *testing.T) {
	m := Default()
	seen := map[string]string{}
	for _, a := range m.Actions() {
		if ContextDocs[a.Context] == "" {
			t.Fatalf("%s sits in an undocumented context %q", a.ID, a.Context)
		}
		for _, c := range a.Default {
			k := a.Context + " " + c
			if other, ok := seen[k]; ok {
				t.Fatalf("%s and %s both default to %s in %s", other, a.ID, c, a.Context)
			}
			seen[k] = a.ID
		}
	}
	for _, id := range []string{"form.submit", "config.profile.add", "config.profile.toggle", "config.provider.new", "config.hook.add", "config.mcp.add", "config.tool.toggle", "config.log.clear", "config.key.type", "config.app.unset"} {
		if m.First(id) == "" {
			t.Fatalf("%s has a default key", id)
		}
	}
}
