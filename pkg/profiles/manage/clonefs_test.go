package manage

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
)

func TestNonExportableEntriesSkipsSocketsCachesAndTempNames(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"keep.txt", "x.tmp", "y.pyc", "z.sock"} {
		writeFile(t, filepath.Join(dir, n), "")
	}
	if err := os.Mkdir(filepath.Join(dir, "__pycache__"), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "live"))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := os.Symlink("keep.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	got := nonExportableEntries(dir, []string{"keep.txt", "x.tmp", "y.pyc", "z.sock", "__pycache__", "live", "link", "vanished"})
	want := map[string]bool{"x.tmp": true, "y.pyc": true, "z.sock": true, "__pycache__": true, "live": true, "vanished": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
}

func TestCloneAllIgnoreExcludesInfrastructureOnlyForDefault(t *testing.T) {
	tmp := manageEnv(t)
	def := profiles.DefaultHome()
	named := namedProfile(t, tmp, "src")
	names := []string{"sessions", "profiles", "node_modules", "skills"}
	for _, root := range []string{def, named, filepath.Join(named, "skills")} {
		for _, n := range names {
			if err := os.MkdirAll(filepath.Join(root, n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	defIgnored := cloneAllCopytreeIgnore(def)(def, names)
	if !defIgnored["sessions"] || !defIgnored["profiles"] || !defIgnored["node_modules"] || defIgnored["skills"] {
		t.Fatalf("default %v", defIgnored)
	}
	namedIgnored := cloneAllCopytreeIgnore(named)(named, names)
	if !namedIgnored["sessions"] || namedIgnored["profiles"] || namedIgnored["node_modules"] {
		t.Fatalf("named %v", namedIgnored)
	}
	if sub := cloneAllCopytreeIgnore(named)(filepath.Join(named, "skills"), names); sub["sessions"] {
		t.Fatalf("below root %v", sub)
	}
}

func TestMaterializeSymlinkedFilesNeverWritesThrough(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "source.env")
	writeFile(t, outside, "KEY=1\n")
	prof := filepath.Join(tmp, "clone")
	writeFile(t, filepath.Join(prof, "SOUL.md"), "soul")
	if err := os.Symlink(outside, filepath.Join(prof, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "gone"), filepath.Join(prof, "auth.json")); err != nil {
		t.Fatal(err)
	}
	done, err := materializeSymlinkedFiles(prof)
	if err != nil || !reflect.DeepEqual(done, []string{".env", "auth.json"}) {
		t.Fatalf("%v %v", done, err)
	}
	st, err := os.Lstat(filepath.Join(prof, ".env"))
	if err != nil || !st.Mode().IsRegular() {
		t.Fatalf("env %v %v", st, err)
	}
	if _, err := os.Lstat(filepath.Join(prof, "auth.json")); !os.IsNotExist(err) {
		t.Fatalf("dangling kept %v", err)
	}
	writeFile(t, filepath.Join(prof, ".env"), "KEY=2\n")
	if data, _ := os.ReadFile(outside); string(data) != "KEY=1\n" {
		t.Fatalf("source changed %q", data)
	}
}

func TestStripMultiplexFlagDropsEmptyGateway(t *testing.T) {
	tmp := manageEnv(t)
	prof := namedProfile(t, tmp, "clone")
	cfg := filepath.Join(prof, "config.yaml")
	writeFile(t, cfg, "model: m\ngateway:\n  multiplex_profiles: true\nmultiplex_profiles: true\n")
	if err := stripMultiplexFlag(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(cfg)
	if strings.Contains(string(data), "multiplex") || strings.Contains(string(data), "gateway") || !strings.Contains(string(data), "model: m") {
		t.Fatalf("%q", data)
	}
	writeFile(t, cfg, "gateway:\n  port: 1\n")
	before, _ := os.ReadFile(cfg)
	if err := stripMultiplexFlag(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(cfg); string(after) != string(before) {
		t.Fatalf("rewritten %q", after)
	}
}

func TestClonePluginsSkipsStagingDirsAndListsNames(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src"), filepath.Join(tmp, "dst")
	writeFile(t, filepath.Join(src, "plugins", "beta", "plugin.yaml"), "name: beta\n")
	writeFile(t, filepath.Join(src, "plugins", "alpha", "plugin.yaml"), "name: alpha\n")
	writeFile(t, filepath.Join(src, "plugins", ".install-metadata.json"), "{}")
	writeFile(t, filepath.Join(src, "plugins", ".install-tmp1", "x"), "")
	writeFile(t, filepath.Join(src, "plugins", ".update-tmp2", "x"), "")
	writeFile(t, filepath.Join(dst, "plugins", "existing", "plugin.yaml"), "")
	if err := clonePlugins(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "plugins", ".install-metadata.json")); err != nil {
		t.Fatal("install record dropped")
	}
	for _, staged := range []string{".install-tmp1", ".update-tmp2"} {
		if _, err := os.Stat(filepath.Join(dst, "plugins", staged)); err == nil {
			t.Fatalf("%s copied", staged)
		}
	}
	if got := ClonedPluginNames(dst); !reflect.DeepEqual(got, []string{"alpha", "beta", "existing"}) {
		t.Fatalf("%v", got)
	}
	if got := ClonedPluginNames(filepath.Join(tmp, "none")); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestCloneFileTightensEnvAndSeedSkipsExisting(t *testing.T) {
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src"), filepath.Join(tmp, "dst")
	writeFile(t, filepath.Join(src, ".env"), "K=1\n")
	writeFile(t, filepath.Join(src, "memories", "MEMORY.md"), "m")
	for _, rel := range []string{".env", "memories/MEMORY.md", "missing.md"} {
		if err := cloneFile(src, dst, rel); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := os.Stat(filepath.Join(dst, ".env")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("env mode %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "missing.md")); err == nil {
		t.Fatal("missing file created")
	}
	seedFileIfMissing(filepath.Join(dst, ".env"), "OVERWRITE", 0)
	if data, _ := os.ReadFile(filepath.Join(dst, ".env")); string(data) != "K=1\n" {
		t.Fatalf("seed overwrote %q", data)
	}
	seedFileIfMissing(filepath.Join(dst, "new.txt"), "fresh", 0o600)
	if st, err := os.Stat(filepath.Join(dst, "new.txt")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("seed %v %v", st, err)
	}
}
