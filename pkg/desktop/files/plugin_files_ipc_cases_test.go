package files

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type pluginIpc struct {
	s      *Service
	host   *fakeHost
	window *fakeOwner
	picks  []DialogResult
	home   string
	vault  string
}

func newPluginIpc(t *testing.T) *pluginIpc {
	scratch := realTemp(t)
	p := &pluginIpc{home: filepath.Join(scratch, ".icebath"), vault: filepath.Join(scratch, "Vault"), window: newFakeOwner()}
	os.Mkdir(p.home, 0o755)
	os.Mkdir(p.vault, 0o755)
	writeFile(t, filepath.Join(p.vault, "note.md"), "hello")
	p.host = &fakeHost{open: func(WatchOwner, OpenDialogOptions) DialogResult {
		next := p.picks[0]
		p.picks = p.picks[1:]
		return next
	}}
	p.s = NewService(p.host, Config{HermesHome: p.home, UserData: filepath.Join(scratch, "userData"), ProtectedHomes: []string{p.home}})
	return p
}

func (p *pluginIpc) request(t *testing.T, pick DialogResult, labels any) map[string]any {
	p.picks = append(p.picks, pick)
	res, err := p.s.PluginFilesRequest(p.window, "plexus", labels)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func grantsOf(t *testing.T, s *Service, plugin string) []any {
	g, err := s.PluginFilesGrants(plugin)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestHermesPluginFilesRequestACancelledPickerGrantsNothing(t *testing.T) {
	p := newPluginIpc(t)
	res := p.request(t, DialogResult{Canceled: true}, map[string]any{"title": "Choose a folder for Plexus"})
	if !reflect.DeepEqual(res, map[string]any{"ok": true, "folder": nil}) {
		t.Fatal(res)
	}
	last := p.host.openCalls[len(p.host.openCalls)-1]
	if p.host.openParent[0] != WatchOwner(p.window) || !reflect.DeepEqual(last.Properties, []string{"openDirectory", "createDirectory"}) || last.Title != "Choose a folder for Plexus" {
		t.Fatal(last)
	}
	if len(grantsOf(t, p.s, "plexus")) != 0 {
		t.Fatal("granted")
	}
}

func TestHermesPluginFilesRequestTheIcebathHomeIsRefusedEvenWhenTheUserPicksIt(t *testing.T) {
	p := newPluginIpc(t)
	res := p.request(t, DialogResult{FilePaths: []string{p.home}}, nil)
	if res["ok"] != false || res["error"] != "protected" || len(grantsOf(t, p.s, "plexus")) != 0 {
		t.Fatal(res)
	}
}

func TestHermesPluginFilesRequestThePickedFolderBecomesAGrantThePluginCanReadThroughUntilRevoked(t *testing.T) {
	p := newPluginIpc(t)
	res := p.request(t, DialogResult{FilePaths: []string{p.vault}}, nil)
	folder := res["folder"].(map[string]any)
	if folder["path"] != p.vault {
		t.Fatal(folder)
	}
	read := p.s.PluginFilesOp("plexus", map[string]any{"op": "readText", "folder": folder["id"], "path": "note.md"})
	if read["ok"] != true || read["text"] != "hello" {
		t.Fatal(read)
	}
	if _, err := p.s.PluginFilesRevoke("plexus", folder["id"], false); err != nil {
		t.Fatal(err)
	}
	after := p.s.PluginFilesOp("plexus", map[string]any{"op": "readText", "folder": folder["id"], "path": "note.md"})
	if after["ok"] != false || after["error"] != "not-granted" {
		t.Fatal(after)
	}
}

func TestHermesPluginFilesRequestARefusedOrCancelledPickTellsNoWindowAnything(t *testing.T) {
	p := newPluginIpc(t)
	p.request(t, DialogResult{Canceled: true}, nil)
	p.request(t, DialogResult{FilePaths: []string{p.home}}, nil)
	if len(p.host.broadcasts) != 0 {
		t.Fatal(p.host.broadcasts)
	}
}

func TestEveryWindowHearsAboutGrantChangesAGrantAndARevokeEachBroadcastThePluginIdARevokeOfNothingDoesNot(t *testing.T) {
	p := newPluginIpc(t)
	res := p.request(t, DialogResult{FilePaths: []string{p.vault}}, nil)
	id := res["folder"].(map[string]any)["id"]
	want := sent{PluginFilesChanged, map[string]any{"pluginId": "plexus"}}
	if len(p.host.broadcasts) != 1 || !reflect.DeepEqual(p.host.broadcasts[0], want) {
		t.Fatal(p.host.broadcasts)
	}
	p.s.PluginFilesRevoke("plexus", id, false)
	if len(p.host.broadcasts) != 2 {
		t.Fatal(p.host.broadcasts)
	}
	p.s.PluginFilesRevoke("plexus", id, false)
	if len(p.host.broadcasts) != 2 {
		t.Fatal(p.host.broadcasts)
	}
}
