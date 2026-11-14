package pm

import (
	"archive/zip"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

// Points the module directory and the running venv prefix at a checkout and a venv
func projectVenvRunningFrom(t *testing.T, checkout, venv string) {
	savedDir, savedPrefix := moduleDir, runningVenvPrefix
	moduleDir = func() string { return checkout }
	runningVenvPrefix = func() (string, bool, error) { return venv, true, nil }
	t.Cleanup(func() { moduleDir, runningVenvPrefix = savedDir, savedPrefix })
}

// file URI of a resolved path like pathlib as_uri
func projectVenvURI(path string) string {
	if runtime.GOOS == "windows" {
		return "file:///" + pyurl.Quote(filepath.ToSlash(path), "/:")
	}
	return "file://" + pyurl.Quote(path, "/")
}

// Writes a venv interpreter and an icebath-agent dist-info whose direct_url names the given text
func projectVenvInstalledFrom(t *testing.T, venv, directURL string) string {
	t.Helper()
	python := VenvPython(venv)
	if err := os.MkdirAll(filepath.Dir(python), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(python, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	site, err := SitePackages(venv)
	if err != nil {
		t.Fatal(err)
	}
	distInfo := filepath.Join(site, "icebath_agent-0.0.0.dist-info")
	if err := os.MkdirAll(distInfo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distInfo, "METADATA"), []byte("Name: icebath-agent\nVersion: 0.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(distInfo, "direct_url.json"), []byte(directURL), 0o644); err != nil {
		t.Fatal(err)
	}
	return site
}

func projectVenvTemp(t *testing.T) string {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProjectVenvDirOutOfTree(t *testing.T) {
	editable := func(source string) string {
		return `{"url": "` + projectVenvURI(source) + `", "dir_info": {"editable": true}}`
	}
	t.Run("out_of_tree_install_resolves_the_running_interpreter_venv", func(t *testing.T) {
		tmp := projectVenvTemp(t)
		checkout := filepath.Join(tmp, "icebath-agent")
		if err := os.Mkdir(checkout, 0o755); err != nil {
			t.Fatal(err)
		}
		venv := filepath.Join(tmp, "venvs", "icebath")
		projectVenvInstalledFrom(t, venv, editable(checkout))
		projectVenvRunningFrom(t, checkout, venv)
		if got, err := projectVenvDir(checkout); err != nil || got != venv {
			t.Fatalf("projectVenvDir = %q, %v, want %q", got, err, venv)
		}
	})
	t.Run("another_installs_interpreter_is_never_claimed", func(t *testing.T) {
		tmp := projectVenvTemp(t)
		dev := filepath.Join(tmp, "dev", "icebath-agent")
		app := filepath.Join(tmp, "app", "icebath-agent")
		for _, dir := range []string{dev, app} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		venv := filepath.Join(tmp, "app", "venv")
		projectVenvInstalledFrom(t, venv, editable(app))
		projectVenvRunningFrom(t, dev, venv)
		if got, err := projectVenvDir(dev); err != nil || got != "" {
			t.Fatalf("projectVenvDir = %q, %v, want none", got, err)
		}
	})
	t.Run("foreign_root_and_in_tree_venv_are_unchanged", func(t *testing.T) {
		tmp := projectVenvTemp(t)
		checkout := filepath.Join(tmp, "icebath-agent")
		other := filepath.Join(tmp, "not-our-checkout")
		for _, dir := range []string{checkout, other} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		venv := filepath.Join(tmp, "venvs", "icebath")
		projectVenvInstalledFrom(t, venv, editable(checkout))
		projectVenvRunningFrom(t, checkout, venv)
		if got, err := projectVenvDir(other); err != nil || got != "" {
			t.Fatalf("projectVenvDir(other) = %q, %v, want none", got, err)
		}
		if err := os.Mkdir(filepath.Join(checkout, ".venv"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got, err := projectVenvDir(checkout); err != nil || got != filepath.Join(checkout, ".venv") {
			t.Fatalf("projectVenvDir = %q, %v, want the in tree .venv", got, err)
		}
	})
}

func TestVenvInstallsCheckoutReadsDirectURL(t *testing.T) {
	tmp := projectVenvTemp(t)
	checkout := filepath.Join(tmp, "icebath-agent")
	if err := os.Mkdir(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, directURL, wantErr string
		want                     bool
	}{
		{name: "matching", directURL: `{"url": "` + projectVenvURI(checkout) + `"}`, want: true},
		{name: "percent encoded", directURL: `{"url": "` + projectVenvURI(filepath.Dir(checkout)) + `/icebath%2Dagent"}`, want: true},
		{name: "malformed json skipped", directURL: `{"url": `},
		{name: "bom skipped", directURL: "\xef\xbb\xbf{}"},
		{name: "bad utf-8 skipped", directURL: "{\"url\": \"\xff\"}"},
		{name: "empty file", directURL: ""},
		{name: "not a file url", directURL: `{"url": "https://example.com/x"}`},
		{name: "uppercase scheme never matches", directURL: `{"url": "FILE://` + filepath.ToSlash(checkout) + `"}`},
		{name: "array raises", directURL: `[]`, wantErr: "'list' object has no attribute 'get'"},
		{name: "null url raises", directURL: `{"url": null}`, wantErr: "'NoneType' object has no attribute 'startswith'"},
	}
	if runtime.GOOS != "windows" {
		cases = append(cases, struct {
			name, directURL, wantErr string
			want                     bool
		}{name: "nul raises", directURL: `{"url": "file:///a%00b"}`, wantErr: "lstat: embedded null character in path"}, struct {
			name, directURL, wantErr string
			want                     bool
		}{name: "remote host raises", directURL: `{"url": "file:////server/x"}`, wantErr: "<urlopen error file:// scheme is supported only on localhost>"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			venv := filepath.Join(projectVenvTemp(t), "venv")
			projectVenvInstalledFrom(t, venv, tc.directURL)
			got, err := venvInstallsCheckout(venv, checkout)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("venvInstallsCheckout = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
	t.Run("other names and a missing direct_url are skipped, egg-info counts", func(t *testing.T) {
		venv := filepath.Join(projectVenvTemp(t), "venv")
		site := projectVenvInstalledFrom(t, venv, `{"url": "https://example.com"}`)
		other := filepath.Join(site, "icebath_agent_extra-1.dist-info")
		bare := filepath.Join(site, "Icebath.Agent-2.dist-info")
		egg := filepath.Join(site, "icebath_agent-3.egg-info")
		for _, dir := range []string{other, bare, egg} {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(other, "direct_url.json"), []byte(`{"url": "`+projectVenvURI(checkout)+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := venvInstallsCheckout(venv, checkout); err != nil || got {
			t.Fatalf("another distribution claimed the checkout: %v, %v", got, err)
		}
		if err := os.WriteFile(filepath.Join(egg, "direct_url.json"), []byte(`{"url": "`+projectVenvURI(checkout)+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, err := venvInstallsCheckout(venv, checkout); err != nil || !got {
			t.Fatalf("egg-info install not found: %v, %v", got, err)
		}
	})
	t.Run("zip path entry", func(t *testing.T) {
		archive := filepath.Join(projectVenvTemp(t), "site.zip")
		f, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		w := zip.NewWriter(f)
		member, err := w.Create("icebath_agent-0.0.0.dist-info/direct_url.json")
		if err == nil {
			_, err = member.Write([]byte(`{"url": "` + projectVenvURI(checkout) + `"}`))
		}
		if err != nil || w.Close() != nil || f.Close() != nil {
			t.Fatal(err)
		}
		dists := metadataDistributions(archive, "icebath-agent")
		if len(dists) != 1 {
			t.Fatalf("zip distributions = %v", dists)
		}
		raw, err := dists[0].readText("direct_url.json")
		if err != nil || raw == "" {
			t.Fatalf("zip direct_url = %q, %v", raw, err)
		}
		if missing, err := dists[0].readText("RECORD"); err != nil || missing != "" {
			t.Fatalf("zip missing member = %q, %v", missing, err)
		}
	})
}

func TestVenvPrefixOfFindsPyvenvCfg(t *testing.T) {
	tmp := projectVenvTemp(t)
	write := func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	above := filepath.Join(tmp, "above")
	write(filepath.Join(above, "bin", "icebath"))
	write(filepath.Join(above, "pyvenv.cfg"))
	beside := filepath.Join(tmp, "beside")
	write(filepath.Join(beside, "icebath"))
	write(filepath.Join(beside, "pyvenv.cfg"))
	both := filepath.Join(tmp, "both")
	write(filepath.Join(both, "bin", "icebath"))
	write(filepath.Join(both, "bin", "pyvenv.cfg"))
	write(filepath.Join(both, "pyvenv.cfg"))
	plain := filepath.Join(tmp, "plain", "bin", "icebath")
	write(plain)
	cases := []struct {
		exe, want string
		ok        bool
	}{
		{filepath.Join(above, "bin", "icebath"), above, true},
		{filepath.Join(beside, "icebath"), beside, true},
		{filepath.Join(both, "bin", "icebath"), both, true},
		{plain, "", false},
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(tmp, "elsewhere", "icebath")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(above, "bin", "icebath"), link); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct {
			exe, want string
			ok        bool
		}{link, above, true})
	}
	for _, tc := range cases {
		got, ok, err := venvPrefixOf(tc.exe)
		if err != nil || got != tc.want || ok != tc.ok {
			t.Errorf("venvPrefixOf(%q) = %q, %v, %v, want %q, %v", tc.exe, got, ok, err, tc.want, tc.ok)
		}
	}
}

func TestProjectVenvDirExport(t *testing.T) {
	root := t.TempDir()
	if dir, err := ProjectVenvDir(root); err != nil || dir != "" {
		t.Fatalf("no venv %q %v", dir, err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".venv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if dir, err := ProjectVenvDir(root); err != nil || dir != filepath.Join(root, ".venv") {
		t.Fatalf("dot venv %q %v", dir, err)
	}
}
